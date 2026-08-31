// Package instance manages Oxide instances.
package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	diskprovider "github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/disk"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const pollInterval = 2 * time.Second

var (
	// ErrNotFound reports that the instance no longer exists on the rack.
	ErrNotFound = errors.New("instance no longer exists")

	// ErrInsufficientCapacity reports that no sled has room for the instance's CPU and memory.
	ErrInsufficientCapacity = errors.New("insufficient capacity")

	// ErrInsufficientStorage reports that the boot disk could not be provisioned for want of room.
	ErrInsufficientStorage = errors.New("insufficient storage capacity")
)

// Options is the per-launch configuration a create cannot carry in the Oxide API's own create shape.
type Options struct {
	// Owner is the expected owner of the instance and every NodeClaim-owned launch resource.
	Owner ownership.Marker
	// AffinityGroups are the affinity groups, by ID, the instance joins between the create and the start.
	AffinityGroups []string
}

// OwnedDisk is a disk whose immutable ID and description marker have both been verified.
type OwnedDisk struct {
	ID     string
	Marker ownership.Marker
}

// Released is the rack capacity an instance gave back when it was torn down.
type Released struct {
	Ncpus  int
	Memory uint64
}

// Provider creates and deletes instances in a single Oxide project.
type Provider struct {
	ox      *oxide.Client
	project string
	disks   *diskprovider.Provider

	deadlines Deadlines

	onRelease func(context.Context, Released)
}

// Option configures a provider at construction.
type Option func(*Provider)

// WithDeadlines replaces the default provider deadlines.
func WithDeadlines(deadlines Deadlines) Option {
	return func(p *Provider) { p.deadlines = deadlines }
}

// WithDiskProvider supplies the shared Oxide disk provider.
func WithDiskProvider(disks *diskprovider.Provider) Option {
	return func(p *Provider) { p.disks = disks }
}

// NewProvider builds an instance provider.
func NewProvider(
	ox *oxide.Client,
	project string,
	onRelease func(context.Context, Released),
	opts ...Option,
) *Provider {
	p := &Provider{
		ox:        ox,
		project:   project,
		onRelease: onRelease,
		deadlines: DefaultDeadlines(),
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.disks == nil {
		p.disks = diskprovider.NewProvider(
			ox,
			project,
			diskprovider.WithDeleteTimeout(p.deadlines.DiskDeleting),
		)
	}
	return p
}

// Create launches an instance and returns it once the rack has placed it on a sled.
func (p *Provider) Create(
	ctx context.Context,
	params oxide.InstanceCreateParams,
	opts Options,
) (*oxide.Instance, error) {
	if params.Body == nil || params.Body.Name == "" {
		return nil, errors.New("instance create requires a body with a name")
	}
	if params.Project == "" {
		params.Project = oxide.NameOrId(p.project)
	}

	ctx, cancel := context.WithTimeout(ctx, p.deadlines.Launch)
	defer cancel()

	params.Body.Start = new(false)
	name := string(params.Body.Name)
	if err := ownership.Verify(params.Body.Description, opts.Owner); err != nil {
		return nil, err
	}

	plannedDisks, err := plannedOwnedDisks(params.Body, opts.Owner)
	if err != nil {
		return nil, err
	}
	createdDiskNames := diskNames(params.Body)

	log.FromContext(ctx).V(1).Info("creating disks", "disks", createdDiskNames)
	if err := p.ensureDisks(ctx, params.Body, opts.Owner); err != nil {
		if !errors.Is(err, ErrStuckLaunchResource) {
			p.abandonDisks(ctx, plannedDisks, err)
		}
		return nil, err
	}
	log.FromContext(ctx).V(1).Info("disks created", "disks", createdDiskNames)

	log.FromContext(ctx).V(1).Info("creating instance", "instance", params.Body.Name)
	created, err := p.ox.InstanceCreate(ctx, params)
	if err != nil {
		switch {

		case errors.Is(err, oxide.ErrInsufficientCapacity):

			p.abandonDisks(ctx, plannedDisks, err)
			return nil, fmt.Errorf(
				"unable to create instance %s, %w, %w", name, ErrInsufficientCapacity, err,
			)
		case errors.Is(err, oxide.ErrObjectAlreadyExists):

			return p.adopt(ctx, name, plannedDisks, opts)
		case ambiguous(ctx, err):

			created, found := p.findInstance(ctx, name)
			if !found {
				p.abandonDisks(ctx, plannedDisks, err)
				return nil, fmt.Errorf("unable to create instance %s, %w", name, err)
			}

			return p.adoptInstance(
				ctx, created, plannedDisks, opts, true,
			)
		default:
			p.abandonDisks(ctx, plannedDisks, err)
			return nil, fmt.Errorf("unable to create instance %s, %w", name, err)
		}
	}
	log.FromContext(ctx).V(1).Info("instance created", "instance", params.Body.Name)

	settled, err := p.awaitSettled(ctx, created)
	if err != nil {
		p.abandonUnlessStuck(ctx, created.Id, opts.Owner, plannedDisks, err)
		return nil, err
	}
	created = settled

	if err := p.joinAffinityGroups(ctx, created.Id, opts.AffinityGroups); err != nil {

		p.abandon(ctx, created.Id, opts.Owner, plannedDisks, err)
		return nil, err
	}

	log.FromContext(ctx).V(1).Info("starting instance", "instance", params.Body.Name)
	started, err := p.start(ctx, created.Id)
	if err != nil {
		p.abandon(ctx, created.Id, opts.Owner, plannedDisks, err)
		return nil, err
	}
	log.FromContext(ctx).V(1).Info("instance started", "instance", params.Body.Name)
	return started, nil
}

// Delete tears down an instance and every resource it owns that outlives it.
func (p *Provider) Delete(ctx context.Context, ref string, teardown Teardown) error {
	instance, err := p.ox.InstanceView(ctx, oxide.InstanceViewParams{
		Instance: oxide.NameOrId(ref),
		Project:  p.projectFor(ref),
	})
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {
			disks, diskErr := p.ownedTeardownDisks(ctx, teardown)
			if diskErr != nil {
				return diskErr
			}
			if diskErr = p.deleteOwnedDisks(ctx, disks); diskErr != nil {
				return diskErr
			}
			return ErrNotFound
		}
		return fmt.Errorf("unable to view the instance, %w", err)
	}
	if err := ownership.Verify(instance.Description, teardown.Owner); err != nil {
		p.rejectOwnership(ctx, "delete", "instance", instance.Id, string(instance.Name), err)
		return err
	}

	disks, err := p.ownedAttachedDisks(ctx, instance, teardown.Owner)
	if err != nil {
		return err
	}
	planned, err := p.ownedTeardownDisks(ctx, teardown)
	if err != nil {
		return err
	}
	disks = appendOwnedDisks(disks, planned...)

	if err := p.stop(ctx, instance); err != nil {
		if errors.Is(err, ErrNotFound) {

			p.notifyReleased(ctx, instance)
			if diskErr := p.deleteOwnedDisks(ctx, disks); diskErr != nil {
				return diskErr
			}
			return ErrNotFound
		}
		return err
	}
	current, err := p.ox.InstanceView(ctx, oxide.InstanceViewParams{
		Instance: oxide.NameOrId(instance.Id),
	})
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {
			p.notifyReleased(ctx, instance)
			if diskErr := p.deleteOwnedDisks(ctx, disks); diskErr != nil {
				return diskErr
			}
			return ErrNotFound
		}
		return fmt.Errorf("unable to re-view the instance before deletion, %w", err)
	}
	if err := ownership.Verify(current.Description, teardown.Owner); err != nil {
		p.rejectOwnership(ctx, "delete", "instance", current.Id, string(current.Name), err)
		return err
	}

	if err := p.ox.InstanceDelete(ctx, oxide.InstanceDeleteParams{
		Instance: oxide.NameOrId(current.Id),
	}); err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
		return fmt.Errorf("unable to delete the instance, %w", err)
	}

	p.notifyReleased(ctx, instance)

	return p.deleteOwnedDisks(ctx, disks)
}

func (p *Provider) notifyReleased(ctx context.Context, instance *oxide.Instance) {
	if p.onRelease == nil {
		return
	}
	p.onRelease(ctx, Released{
		Ncpus:  int(instance.Ncpus),
		Memory: uint64(instance.Memory),
	})
}

func (p *Provider) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), p.deadlines.Cleanup)
}

func (p *Provider) cleanup(parent context.Context, fn func(context.Context)) {
	ctx, cancel := p.cleanupContext(parent)
	defer cancel()
	fn(ctx)
}

func (p *Provider) awaitSettled(
	ctx context.Context,
	instance *oxide.Instance,
) (*oxide.Instance, error) {
	if instance.RunState != oxide.InstanceStateCreating {
		return instance, nil
	}

	started := time.Now()
	pollCtx, cancel := context.WithTimeout(ctx, p.deadlines.InstanceCreating)
	defer cancel()

	settled := instance
	err := wait.PollUntilContextCancel(
		pollCtx,
		pollInterval,
		false,
		func(ctx context.Context) (bool, error) {
			view, err := p.ox.InstanceView(
				ctx,
				oxide.InstanceViewParams{Instance: oxide.NameOrId(instance.Id)},
			)
			if err != nil {
				if errors.Is(err, oxide.ErrObjectNotFound) {
					return false, ErrNotFound
				}
				return false, fmt.Errorf("unable to view instance %s, %w", instance.Name, err)
			}
			settled = view
			return view.RunState != oxide.InstanceStateCreating, nil
		},
	)
	if err == nil {
		return settled, nil
	}
	if wait.Interrupted(err) {
		return nil, &StuckLaunchResourceError{
			Kind:     KindInstance,
			ID:       instance.Id,
			Name:     string(instance.Name),
			State:    string(oxide.InstanceStateCreating),
			StuckFor: time.Since(started),
		}
	}
	return nil, err
}

func (p *Provider) adopt(
	ctx context.Context,
	name string,
	plannedDisks []OwnedDisk,
	opts Options,
) (*oxide.Instance, error) {
	log.FromContext(ctx).V(1).Info("instance already exists, adopting it", "instance", name)

	instance, err := p.ox.InstanceView(ctx, oxide.InstanceViewParams{
		Instance: oxide.NameOrId(name),
		Project:  oxide.NameOrId(p.project),
	})
	if err != nil {
		p.abandonDisks(ctx, plannedDisks, err)
		if errors.Is(err, oxide.ErrObjectNotFound) {

			return nil, fmt.Errorf(
				"instance %s already exists but was deleted before it could be adopted", name,
			)
		}
		return nil, fmt.Errorf("unable to view existing instance %s, %w", name, err)
	}

	return p.adoptInstance(ctx, instance, plannedDisks, opts, false)
}

func (p *Provider) adoptInstance(
	ctx context.Context,
	instance *oxide.Instance,
	plannedDisks []OwnedDisk,
	opts Options,
	sameLaunch bool,
) (*oxide.Instance, error) {
	name := string(instance.Name)
	if err := ownership.Verify(instance.Description, opts.Owner); err != nil {
		p.rejectOwnership(ctx, "adopt", "instance", instance.Id, name, err)
		p.abandonDisks(ctx, plannedDisks, err)
		return nil, err
	}

	if instance.RunState == oxide.InstanceStateCreating {
		settled, err := p.awaitSettled(ctx, instance)
		if err != nil {
			p.abandonUnlessStuck(ctx, instance.Id, opts.Owner, plannedDisks, err)
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf(
					"instance %s already exists but was deleted before it could be adopted", name,
				)
			}
			return nil, err
		}
		instance = settled
	}

	switch instance.RunState {

	case oxide.InstanceStateRunning,
		oxide.InstanceStateStarting,
		oxide.InstanceStateRebooting,
		oxide.InstanceStateMigrating:
		p.abandonUnusedDisks(ctx, plannedDisks, sameLaunch)
		return instance, nil

	case oxide.InstanceStateStopped:

		if err := p.joinAffinityGroups(ctx, instance.Id, opts.AffinityGroups); err != nil {
			p.abandon(ctx, instance.Id, opts.Owner, plannedDisks, err)
			return nil, err
		}
		started, err := p.start(ctx, instance.Id)
		if err != nil {

			p.abandon(ctx, instance.Id, opts.Owner, plannedDisks, err)
			return nil, err
		}
		p.abandonUnusedDisks(ctx, plannedDisks, sameLaunch)
		return started, nil

	default:
		abandonErr := fmt.Errorf(
			"instance %s already exists in run state %q and cannot be adopted",
			name, instance.RunState,
		)
		var deleteErr error
		p.cleanup(ctx, func(cleanupCtx context.Context) {
			deleteErr = p.Delete(cleanupCtx, instance.Id, Teardown{Owner: opts.Owner})
		})
		if deleteErr != nil && !errors.Is(deleteErr, ErrNotFound) {
			return nil, fmt.Errorf(
				"%w, and it could not be terminated, %w", abandonErr, deleteErr,
			)
		}
		return nil, fmt.Errorf("%w, so it was terminated", abandonErr)
	}
}

func (p *Provider) joinAffinityGroups(ctx context.Context, id string, groups []string) error {
	for _, group := range groups {
		if _, err := p.ox.ExperimentalAffinityGroupMemberInstanceAdd(
			ctx,
			oxide.AffinityGroupMemberInstanceAddParams{
				AffinityGroup: oxide.NameOrId(group),
				Instance:      oxide.NameOrId(id),
			},
		); err != nil && !errors.Is(err, oxide.ErrObjectAlreadyExists) {
			return fmt.Errorf(
				"unable to add instance %s to affinity group %s, %w", id, group, err,
			)
		}
	}
	return nil
}

func (p *Provider) start(ctx context.Context, id string) (*oxide.Instance, error) {
	started, err := p.ox.InstanceStart(
		ctx,
		oxide.InstanceStartParams{Instance: oxide.NameOrId(id)},
	)
	if err != nil {
		switch {
		case errors.Is(err, oxide.ErrInsufficientCapacity):
			return nil, fmt.Errorf(
				"unable to start instance %s, %w, %w", id, ErrInsufficientCapacity, err,
			)
		case errors.Is(err, oxide.ErrObjectNotFound):
			return nil, ErrNotFound
		default:
			return nil, fmt.Errorf("unable to start instance %s, %w", id, err)
		}
	}
	return started, nil
}

func (p *Provider) stop(ctx context.Context, instance *oxide.Instance) error {
	if deletable(instance.RunState) {
		return nil
	}

	if instance.RunState != oxide.InstanceStateStopping {
		if _, err := p.ox.InstanceStop(ctx, oxide.InstanceStopParams{
			Instance: oxide.NameOrId(instance.Id),
		}); err != nil {
			switch {
			case errors.Is(err, oxide.ErrObjectNotFound):
				return ErrNotFound

			case errors.Is(err, oxide.ErrInvalidRequest):
				log.FromContext(ctx).V(1).Info(
					"instance stop rejected, waiting for the in-flight transition instead",
					"instance-id", instance.Id,
					"run-state", instance.RunState,
					"reason", err.Error(),
				)
			default:
				return fmt.Errorf("unable to stop the instance, %w", err)
			}
		}
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, p.deadlines.InstanceStopping)
	defer cancel()

	var lastState oxide.InstanceState
	err := wait.PollUntilContextCancel(
		ctx,
		pollInterval,
		true,
		func(ctx context.Context) (bool, error) {
			current, err := p.ox.InstanceView(
				ctx,
				oxide.InstanceViewParams{Instance: oxide.NameOrId(instance.Id)},
			)
			if err != nil {
				if errors.Is(err, oxide.ErrObjectNotFound) {
					return false, ErrNotFound
				}
				return false, fmt.Errorf("unable to view the instance, %w", err)
			}
			lastState = current.RunState
			return deletable(current.RunState), nil
		},
	)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		if wait.Interrupted(err) {

			return &StuckLaunchResourceError{
				Kind:     KindInstance,
				ID:       instance.Id,
				Name:     string(instance.Name),
				State:    string(lastState),
				StuckFor: time.Since(started),
			}
		}
		return err
	}
	return nil
}

func (p *Provider) abandon(
	ctx context.Context,
	ref string,
	owner ownership.Marker,
	disks []OwnedDisk,
	cause error,
) {
	p.cleanup(ctx, func(cleanupCtx context.Context) {
		p.abandonWithContext(cleanupCtx, ref, owner, disks, cause)
	})
}

func (p *Provider) abandonWithContext(
	ctx context.Context,
	ref string,
	owner ownership.Marker,
	disks []OwnedDisk,
	cause error,
) {
	if err := p.Delete(
		ctx,
		ref,
		Teardown{Owner: owner},
	); err != nil &&
		!errors.Is(err, ErrNotFound) {
		log.FromContext(ctx).Error(
			err,
			"unable to clean up an instance that did not launch",
			"instance", ref,
			"cause", cause.Error(),
		)
	}
	p.abandonDisksWithContext(ctx, disks, cause)
}

func (p *Provider) abandonUnusedDisks(ctx context.Context, disks []OwnedDisk, sameLaunch bool) {
	if sameLaunch || len(disks) == 0 {
		return
	}
	log.FromContext(ctx).V(1).Info(
		"removing the disks this attempt created, having adopted an instance attached to another attempt's",
		"disks", diskIDs(disks),
	)
	p.cleanup(ctx, func(cleanupCtx context.Context) {
		p.abandonDisksWithContext(
			cleanupCtx,
			disks,
			errors.New("the launch adopted an existing instance"),
		)
	})
}

func (p *Provider) abandonUnlessStuck(
	ctx context.Context,
	ref string,
	owner ownership.Marker,
	disks []OwnedDisk,
	cause error,
) {
	if errors.Is(cause, ErrStuckLaunchResource) {
		log.FromContext(ctx).Info(
			"leaving a stuck launch resource for the orphan reaper rather than deleting it inline",
			"instance", ref,
			"cause", cause.Error(),
		)
		return
	}
	p.abandon(ctx, ref, owner, disks, cause)
}

func (p *Provider) findInstance(ctx context.Context, name string) (*oxide.Instance, bool) {
	view, err := p.ox.InstanceView(ctx, oxide.InstanceViewParams{
		Instance: oxide.NameOrId(name),
		Project:  oxide.NameOrId(p.project),
	})
	if err != nil {
		if !errors.Is(err, oxide.ErrObjectNotFound) {
			log.FromContext(ctx).V(1).Info(
				"unable to look up an instance whose create gave no answer",
				"instance", name,
				"reason", err.Error(),
			)
		}
		return nil, false
	}
	return view, true
}

const sendFailure = "error sending request:"

func ambiguous(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), sendFailure)
}

func (p *Provider) projectFor(ref string) oxide.NameOrId {
	if _, err := uuid.Parse(ref); err == nil {
		return ""
	}
	return oxide.NameOrId(p.project)
}

func deletable(state oxide.InstanceState) bool {
	switch state {
	case oxide.InstanceStateStopped,
		oxide.InstanceStateFailed,
		oxide.InstanceStateDestroyed:
		return true
	default:
		return false
	}
}

// Teardown is how a delete finds the resources a launch created.
type Teardown struct {
	// Owner is the expected owner of the instance and its NodeClaim-owned disks.
	Owner ownership.Marker
	// BootDiskName is the name the boot disk was created with, for a launch whose boot disk is not under DiskPrefix.
	BootDiskName string
	// DiskPrefix is the prefix this launch named its disks with.
	DiskPrefix string
}

func (p *Provider) rejectOwnership(
	ctx context.Context,
	operation string,
	resourceKind string,
	id string,
	name string,
	err error,
) {
	conflict, ok := ownership.Conflict(err)
	if !ok {
		return
	}
	metrics.OwnershipRejections.WithLabelValues(
		operation, resourceKind, string(conflict.Reason),
	).Inc()
	log.FromContext(ctx).Info(
		"rejected an Oxide resource whose ownership could not be verified",
		"operation", operation,
		"resource-kind", resourceKind,
		"resource-id", id,
		"resource", name,
		"reason", conflict.Reason,
		"expected-cluster-uid", conflict.Expected.ClusterUID,
		"observed-cluster-uid", conflict.Observed.ClusterUID,
		"expected-nodeclaim", conflict.Expected.NodeClaimName,
		"observed-nodeclaim", conflict.Observed.NodeClaimName,
		"expected-nodeclaim-uid", conflict.Expected.NodeClaimUID,
		"observed-nodeclaim-uid", conflict.Observed.NodeClaimUID,
		"expected-kind", conflict.Expected.Kind,
		"observed-kind", conflict.Observed.Kind,
	)
}
