// Package instance manages Oxide instances.
package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/google/uuid"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	pollInterval = 2 * time.Second

	diskDeleteTimeout = 90 * time.Second

	diskDeleteAttemptTimeout = 10 * time.Second
)

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

	deadlines Deadlines

	onRelease func(context.Context, Released)
}

// Option configures a provider at construction.
type Option func(*Provider)

// WithDeadlines replaces the default transitional-state deadlines.
func WithDeadlines(deadlines Deadlines) Option {
	return func(p *Provider) { p.deadlines = deadlines }
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

func (p *Provider) ensureDisks(
	ctx context.Context,
	body *oxide.InstanceCreate,
	owner ownership.Marker,
) error {
	specs := diskCreateSpecs(body)
	pre := make([]*oxide.InstanceDiskAttachmentCreate, 0, len(specs))
	for _, spec := range specs {
		if distributed(spec) {
			pre = append(pre, spec)
		}
	}
	if len(pre) == 0 {
		return nil
	}

	group, groupCtx := errgroup.WithContext(ctx)
	for _, spec := range pre {
		group.Go(func() error { return p.ensureDisk(groupCtx, spec, string(body.Name), owner) })
	}
	if err := group.Wait(); err != nil {
		return err
	}

	if spec, ok := diskCreateSpec(body.BootDisk); ok && distributed(spec) {
		body.BootDisk = oxide.InstanceDiskAttachment{
			Value: &oxide.InstanceDiskAttachmentAttach{Name: spec.Name},
		}
	}
	for i, attachment := range body.Disks {
		if spec, ok := diskCreateSpec(attachment); ok && distributed(spec) {
			body.Disks[i] = oxide.InstanceDiskAttachment{
				Value: &oxide.InstanceDiskAttachmentAttach{Name: spec.Name},
			}
		}
	}
	return nil
}

func (p *Provider) ensureDisk(
	ctx context.Context,
	spec *oxide.InstanceDiskAttachmentCreate,
	instanceName string,
	owner ownership.Marker,
) error {
	name := string(spec.Name)
	expected, err := expectedDiskMarker(spec.Description, owner)
	if err != nil {
		return err
	}

	for attempt := 0; ; attempt++ {
		log.FromContext(ctx).V(1).Info("Calling DiskCreate", "disk", spec.Name)
		created, err := p.ox.DiskCreate(ctx, oxide.DiskCreateParams{
			Project: oxide.NameOrId(p.project),
			Body: &oxide.DiskCreate{
				Name:        spec.Name,
				Description: spec.Description,
				Size:        spec.Size,
				DiskBackend: spec.DiskBackend,
			},
		})
		if err == nil {
			if err := ownership.Verify(created.Description, expected); err != nil {
				p.rejectOwnership(ctx, "adopt", "disk", created.Id, string(created.Name), err)
				return err
			}
			if err := p.waitForDiskReady(ctx, created); err != nil {

				return err
			}
			return nil
		}
		log.FromContext(ctx).V(1).Info("Called DiskCreate", "disk", spec.Name)

		switch {
		case errors.Is(err, oxide.ErrInsufficientCapacity):
			return fmt.Errorf(
				"unable to create disk %s, %w, %w", name, ErrInsufficientStorage, err,
			)

		case ambiguous(ctx, err):

			existing, found := p.findDisk(ctx, name)
			if !found {
				return fmt.Errorf("unable to create disk %s, %w", name, err)
			}
			if err := ownership.Verify(existing.Description, expected); err != nil {
				p.rejectOwnership(ctx, "adopt", "disk", existing.Id, string(existing.Name), err)
				return err
			}
			return p.waitForDiskReady(ctx, existing)

		case errors.Is(err, oxide.ErrObjectAlreadyExists) && attempt == 0:
			replace, err := p.adoptDisk(ctx, spec, instanceName, expected)
			if err != nil {
				return err
			}
			if !replace {
				return nil
			}

		default:
			return fmt.Errorf("unable to create disk %s, %w", name, err)
		}
	}
}

func (p *Provider) adoptDisk(
	ctx context.Context,
	spec *oxide.InstanceDiskAttachmentCreate,
	instanceName string,
	expected ownership.Marker,
) (bool, error) {
	name := string(spec.Name)
	log.FromContext(ctx).V(1).Info("disk already exists, adopting it", "disk", name)

	existing, err := p.ox.DiskView(ctx, oxide.DiskViewParams{
		Disk:    oxide.NameOrId(name),
		Project: oxide.NameOrId(p.project),
	})
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {

			return false, fmt.Errorf(
				"disk %s already exists but was deleted before it could be adopted", name,
			)
		}
		return false, fmt.Errorf("unable to view existing disk %s, %w", name, err)
	}
	if err := ownership.Verify(existing.Description, expected); err != nil {
		p.rejectOwnership(ctx, "adopt", "disk", existing.Id, string(existing.Name), err)
		return false, err
	}

	if attached, ok := existing.State.AsAttached(); ok && attached.Instance != "" {
		if !p.instanceIs(ctx, attached.Instance, instanceName) {
			return false, fmt.Errorf(
				"disk %s already exists and is attached to instance %s rather than to %s, so it is not this launch's to adopt",
				name,
				attached.Instance,
				instanceName,
			)
		}
	}

	switch state := existing.State.State(); state {

	case oxide.DiskStateStateCreating,
		oxide.DiskStateStateDetached,
		oxide.DiskStateStateAttaching,
		oxide.DiskStateStateAttached:
		return false, p.waitForDiskReady(ctx, existing)

	default:
		if err := p.DeleteOwnedDisk(ctx, existing.Id, expected); err != nil {
			return false, fmt.Errorf(
				"disk %s already exists in state %q and could not be removed, %w",
				name, state, err,
			)
		}
		return true, nil
	}
}

func (p *Provider) waitForDiskReady(ctx context.Context, disk *oxide.Disk) error {
	if diskAttachable(disk.State.State()) {
		return nil
	}

	started := time.Now()
	pollCtx, cancel := context.WithTimeout(ctx, p.deadlines.DiskCreating)
	defer cancel()

	lastState := disk.State.State()
	err := wait.PollUntilContextCancel(
		pollCtx,
		pollInterval,
		false,
		func(ctx context.Context) (bool, error) {
			view, err := p.ox.DiskView(
				ctx,
				oxide.DiskViewParams{Disk: oxide.NameOrId(disk.Id)},
			)
			if err != nil {
				if errors.Is(err, oxide.ErrObjectNotFound) {
					return false, fmt.Errorf(
						"disk %s was deleted before it became usable", disk.Name,
					)
				}
				return false, fmt.Errorf("unable to view disk %s, %w", disk.Name, err)
			}
			lastState = view.State.State()

			switch {
			case diskAttachable(lastState):
				return true, nil

			case lastState == oxide.DiskStateStateFaulted,
				lastState == oxide.DiskStateStateDestroyed:
				return false, fmt.Errorf(
					"disk %s entered state %q instead of becoming usable",
					disk.Name,
					lastState,
				)

			default:
				return false, nil
			}
		},
	)
	if err == nil {
		return nil
	}
	if wait.Interrupted(err) {

		return &StuckLaunchResourceError{
			Kind:     KindDisk,
			ID:       disk.Id,
			Name:     string(disk.Name),
			State:    string(lastState),
			StuckFor: time.Since(started),
		}
	}
	return err
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

func (p *Provider) abandonDisks(ctx context.Context, disks []OwnedDisk, cause error) {
	for _, disk := range disks {
		if disk.ID == "" {
			continue
		}
		if err := p.DeleteOwnedDisk(ctx, disk.ID, disk.Marker); err != nil {
			log.FromContext(ctx).Error(
				err,
				"unable to clean up a disk whose instance did not launch",
				"disk", disk.ID,
				"cause", cause.Error(),
			)
		}
	}
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
		if err := p.Delete(ctx, instance.Id, Teardown{Owner: opts.Owner}); err != nil &&
			!errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w, and it could not be terminated, %w", abandonErr, err)
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
	p.abandonDisks(ctx, disks, cause)
}

func (p *Provider) abandonUnusedDisks(ctx context.Context, disks []OwnedDisk, sameLaunch bool) {
	if sameLaunch || len(disks) == 0 {
		return
	}
	log.FromContext(ctx).V(1).Info(
		"removing the disks this attempt created, having adopted an instance attached to another attempt's",
		"disks", diskIDs(disks),
	)
	p.abandonDisks(ctx, disks, errors.New("the launch adopted an existing instance"))
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

func (p *Provider) findDisk(ctx context.Context, name string) (*oxide.Disk, bool) {
	view, err := p.ox.DiskView(ctx, oxide.DiskViewParams{
		Disk:    oxide.NameOrId(name),
		Project: oxide.NameOrId(p.project),
	})
	if err != nil {
		if !errors.Is(err, oxide.ErrObjectNotFound) {
			log.FromContext(ctx).V(1).Info(
				"unable to look up a disk whose create gave no answer",
				"disk", name,
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

func (p *Provider) ownedAttachedDisks(
	ctx context.Context,
	instance *oxide.Instance,
	expected ownership.Marker,
) ([]OwnedDisk, error) {
	attached, err := p.ox.InstanceDiskListAllPages(
		ctx,
		oxide.InstanceDiskListParams{Instance: oxide.NameOrId(instance.Id)},
	)
	if err != nil {
		return nil, fmt.Errorf("unable to list disks attached to instance %s, %w", instance.Id, err)
	}
	var owned []OwnedDisk
	for _, disk := range attached {
		marker, verifyErr := verifyNodeClaimDisk(disk.Description, expected)
		if verifyErr != nil {
			p.rejectOwnership(ctx, "cleanup", "disk", disk.Id, string(disk.Name), verifyErr)
			log.FromContext(ctx).Info(
				"leaving an attached disk intact because its ownership could not be verified",
				"disk-id", disk.Id,
				"disk", disk.Name,
				"instance-id", instance.Id,
			)
			continue
		}
		owned = appendOwnedDisks(owned, OwnedDisk{ID: disk.Id, Marker: marker})
	}
	return owned, nil
}

func (p *Provider) deleteOwnedDisks(ctx context.Context, disks []OwnedDisk) error {
	var errs []error
	for _, disk := range disks {
		if err := p.DeleteOwnedDisk(ctx, disk.ID, disk.Marker); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DeleteOwnedDisk re-reads and verifies a disk immediately before deleting it by immutable ID.
func (p *Provider) DeleteOwnedDisk(
	ctx context.Context,
	ref string,
	expected ownership.Marker,
) error {
	if ref == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, diskDeleteTimeout)
	defer cancel()

	var lastErr error
	err := wait.PollUntilContextCancel(
		ctx,
		pollInterval,
		true,
		func(ctx context.Context) (bool, error) {
			attemptCtx, cancelAttempt := context.WithTimeout(ctx, diskDeleteAttemptTimeout)
			defer cancelAttempt()
			disk, err := p.ox.DiskView(attemptCtx, oxide.DiskViewParams{
				Disk: oxide.NameOrId(ref), Project: p.projectFor(ref),
			})
			if errors.Is(err, oxide.ErrObjectNotFound) {
				return true, nil
			}
			if err != nil {
				lastErr = err
				return false, nil
			}
			if err := ownership.Verify(disk.Description, expected); err != nil {
				p.rejectOwnership(ctx, "delete", "disk", disk.Id, string(disk.Name), err)
				return false, err
			}

			err = p.ox.DiskDelete(attemptCtx, oxide.DiskDeleteParams{
				Disk: oxide.NameOrId(disk.Id),
			})

			if err == nil || errors.Is(err, oxide.ErrObjectNotFound) {
				return true, nil
			}
			lastErr = err
			log.FromContext(ctx).V(1).Info(
				"disk deletion failed, retrying",
				"disk", ref,
				"reason", err.Error(),
			)
			return false, nil
		},
	)
	if err != nil {
		if wait.Interrupted(err) && lastErr != nil {
			err = lastErr
		}
		return fmt.Errorf("unable to delete disk %s, %w", ref, err)
	}
	return nil
}

func (p *Provider) projectFor(ref string) oxide.NameOrId {
	if _, err := uuid.Parse(ref); err == nil {
		return ""
	}
	return oxide.NameOrId(p.project)
}

func diskNames(body *oxide.InstanceCreate) []string {
	specs := diskCreateSpecs(body)
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, string(spec.Name))
	}
	return names
}

func diskCreateSpecs(body *oxide.InstanceCreate) []*oxide.InstanceDiskAttachmentCreate {
	var specs []*oxide.InstanceDiskAttachmentCreate
	if spec, ok := diskCreateSpec(body.BootDisk); ok {
		specs = append(specs, spec)
	}
	for _, attachment := range body.Disks {
		if spec, ok := diskCreateSpec(attachment); ok {
			specs = append(specs, spec)
		}
	}
	return specs
}

func diskCreateSpec(
	attachment oxide.InstanceDiskAttachment,
) (*oxide.InstanceDiskAttachmentCreate, bool) {
	switch spec := attachment.Value.(type) {
	case *oxide.InstanceDiskAttachmentCreate:
		return spec, true
	case oxide.InstanceDiskAttachmentCreate:
		return &spec, true
	default:
		return nil, false
	}
}

func distributed(spec *oxide.InstanceDiskAttachmentCreate) bool {
	_, local := spec.DiskBackend.AsLocal()
	return !local
}

func diskAttachable(state oxide.DiskStateState) bool {
	switch state {
	case oxide.DiskStateStateDetached,
		oxide.DiskStateStateAttaching,
		oxide.DiskStateStateAttached:
		return true
	default:
		return false
	}
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

func (p *Provider) ownedTeardownDisks(
	ctx context.Context,
	teardown Teardown,
) ([]OwnedDisk, error) {
	var candidates []oxide.Disk
	if teardown.BootDiskName != "" {
		disk, err := p.ox.DiskView(ctx, oxide.DiskViewParams{
			Disk: oxide.NameOrId(teardown.BootDiskName), Project: oxide.NameOrId(p.project),
		})
		if err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
			return nil, fmt.Errorf("unable to view boot disk candidate %s, %w", teardown.BootDiskName, err)
		}
		if err == nil {
			candidates = append(candidates, *disk)
		}
	}
	if teardown.DiskPrefix == "" {
		return p.verifyDiskCandidates(ctx, candidates, teardown.Owner), nil
	}
	disks, err := p.ox.DiskListAllPages(ctx, oxide.DiskListParams{
		Project: oxide.NameOrId(p.project),
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list disk candidates with prefix %s, %w", teardown.DiskPrefix, err)
	}
	for _, disk := range disks {
		if !strings.HasPrefix(string(disk.Name), teardown.DiskPrefix) {
			continue
		}
		candidates = append(candidates, disk)
	}
	return p.verifyDiskCandidates(ctx, candidates, teardown.Owner), nil
}

func (p *Provider) verifyDiskCandidates(
	ctx context.Context,
	candidates []oxide.Disk,
	expected ownership.Marker,
) []OwnedDisk {
	var owned []OwnedDisk
	for i := range candidates {
		disk := &candidates[i]
		marker, err := verifyNodeClaimDisk(disk.Description, expected)
		if err != nil {
			p.rejectOwnership(ctx, "cleanup", "disk", disk.Id, string(disk.Name), err)
			continue
		}
		owned = appendOwnedDisks(owned, OwnedDisk{ID: disk.Id, Marker: marker})
	}
	return owned
}

func plannedOwnedDisks(
	body *oxide.InstanceCreate,
	owner ownership.Marker,
) ([]OwnedDisk, error) {
	var owned []OwnedDisk
	for _, spec := range diskCreateSpecs(body) {
		marker, err := expectedDiskMarker(spec.Description, owner)
		if err != nil {
			return nil, err
		}
		owned = appendOwnedDisks(owned, OwnedDisk{ID: string(spec.Name), Marker: marker})
	}
	return owned, nil
}

func expectedDiskMarker(description string, owner ownership.Marker) (ownership.Marker, error) {
	return verifyNodeClaimDisk(description, owner)
}

func verifyNodeClaimDisk(
	description string,
	owner ownership.Marker,
) (ownership.Marker, error) {
	marker, ok := ownership.Parse(description)
	if !ok {
		expected := owner
		expected.Kind = ownership.KindBootDisk
		return ownership.Marker{}, ownership.Verify(description, expected)
	}
	if marker.Kind != ownership.KindBootDisk && marker.Kind != ownership.KindDataDisk {
		return ownership.Marker{}, &ownership.ErrOwnershipConflict{
			Reason: ownership.ReasonWrongKind, Expected: owner, Observed: marker,
		}
	}
	expected := owner
	expected.Kind = marker.Kind
	if err := ownership.Verify(description, expected); err != nil {
		return ownership.Marker{}, err
	}
	return marker, nil
}

func appendOwnedDisks(dst []OwnedDisk, disks ...OwnedDisk) []OwnedDisk {
	seen := make(map[string]struct{}, len(dst)+len(disks))
	for _, disk := range dst {
		seen[disk.ID] = struct{}{}
	}
	for _, disk := range disks {
		if disk.ID == "" {
			continue
		}
		if _, ok := seen[disk.ID]; ok {
			continue
		}
		seen[disk.ID] = struct{}{}
		dst = append(dst, disk)
	}
	return dst
}

func diskIDs(disks []OwnedDisk) []string {
	ids := make([]string, 0, len(disks))
	for _, disk := range disks {
		ids = append(ids, disk.ID)
	}
	return ids
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

func (p *Provider) instanceIs(ctx context.Context, id, name string) bool {
	if id == "" || name == "" {
		return false
	}
	view, err := p.ox.InstanceView(ctx, oxide.InstanceViewParams{Instance: oxide.NameOrId(id)})
	if err != nil {
		log.FromContext(ctx).V(1).Info(
			"unable to view the instance a disk is attached to, so the disk is not adopted",
			"instance-id", id,
			"reason", err.Error(),
		)
		return false
	}
	return string(view.Name) == name
}
