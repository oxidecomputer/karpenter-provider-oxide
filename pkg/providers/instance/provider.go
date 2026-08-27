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
	// AffinityGroups are the affinity groups, by ID, the instance joins between the create and the start.
	AffinityGroups []string
	// OwnedDisks are attached disks this launch must delete on failure.
	OwnedDisks []string
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

	createdDiskNames := diskNames(params.Body)
	ownedDiskNames := append([]string{}, createdDiskNames...)
	ownedDiskNames = appendUnique(ownedDiskNames, opts.OwnedDisks...)

	log.FromContext(ctx).V(1).Info("creating disks", "disks", createdDiskNames)
	if err := p.ensureDisks(ctx, params.Body, createdDiskNames); err != nil {
		p.abandonDisks(ctx, opts.OwnedDisks, err)
		return nil, err
	}
	log.FromContext(ctx).V(1).Info("disks created", "disks", createdDiskNames)

	log.FromContext(ctx).V(1).Info("creating instance", "instance", params.Body.Name)
	created, err := p.ox.InstanceCreate(ctx, params)
	if err != nil {
		switch {

		case errors.Is(err, oxide.ErrInsufficientCapacity):

			p.abandonDisks(ctx, ownedDiskNames, err)
			return nil, fmt.Errorf(
				"unable to create instance %s, %w, %w", name, ErrInsufficientCapacity, err,
			)
		case errors.Is(err, oxide.ErrObjectAlreadyExists):

			return p.adopt(ctx, name, createdDiskNames, ownedDiskNames, opts)
		case ambiguous(ctx, err):

			created, found := p.findInstance(ctx, name)
			if !found {
				p.abandonDisks(ctx, ownedDiskNames, err)
				return nil, fmt.Errorf("unable to create instance %s, %w", name, err)
			}

			return p.adoptInstance(
				ctx, created, createdDiskNames, ownedDiskNames, opts, true,
			)
		default:
			p.abandonDisks(ctx, ownedDiskNames, err)
			return nil, fmt.Errorf("unable to create instance %s, %w", name, err)
		}
	}
	log.FromContext(ctx).V(1).Info("instance created", "instance", params.Body.Name)

	settled, err := p.awaitSettled(ctx, created)
	if err != nil {
		p.abandonUnlessStuck(ctx, created.Id, ownedDiskNames, err)
		return nil, err
	}
	created = settled

	if err := p.joinAffinityGroups(ctx, created.Id, opts.AffinityGroups); err != nil {

		p.abandon(ctx, created.Id, ownedDiskNames, err)
		return nil, err
	}

	log.FromContext(ctx).V(1).Info("starting instance", "instance", params.Body.Name)
	started, err := p.start(ctx, created.Id)
	if err != nil {
		p.abandon(ctx, created.Id, ownedDiskNames, err)
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

			if diskErr := p.deleteDisks(ctx, p.ownedDisks(ctx, teardown)); diskErr != nil {
				return diskErr
			}
			return ErrNotFound
		}
		return fmt.Errorf("unable to view the instance, %w", err)
	}

	disks := p.attachedDisks(ctx, instance, teardown)

	if err := p.stop(ctx, instance); err != nil {
		if errors.Is(err, ErrNotFound) {

			p.notifyReleased(ctx, instance)
			if diskErr := p.deleteDisks(ctx, disks); diskErr != nil {
				return diskErr
			}
			return ErrNotFound
		}
		return err
	}

	if err := p.ox.InstanceDelete(ctx, oxide.InstanceDeleteParams{
		Instance: oxide.NameOrId(instance.Id),
	}); err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
		return fmt.Errorf("unable to delete the instance, %w", err)
	}

	p.notifyReleased(ctx, instance)

	return p.deleteDisks(ctx, disks)
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
	owned []string,
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
		group.Go(func() error { return p.ensureDisk(groupCtx, spec, string(body.Name)) })
	}
	if err := group.Wait(); err != nil {

		if !errors.Is(err, ErrStuckLaunchResource) {
			p.abandonDisks(ctx, owned, err)
		}
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
) error {
	name := string(spec.Name)

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
			return p.waitForDiskReady(ctx, existing)

		case errors.Is(err, oxide.ErrObjectAlreadyExists) && attempt == 0:
			replace, err := p.adoptDisk(ctx, spec, instanceName)
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
		if err := p.deleteDisk(ctx, existing.Id); err != nil {
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

func (p *Provider) abandonDisks(ctx context.Context, names []string, cause error) {
	for _, name := range names {
		if name == "" {
			continue
		}
		if err := p.deleteDisk(ctx, name); err != nil {
			log.FromContext(ctx).Error(
				err,
				"unable to clean up a disk whose instance did not launch",
				"disk", name,
				"cause", cause.Error(),
			)
		}
	}
}

func (p *Provider) adopt(
	ctx context.Context,
	name string,
	createdDiskNames []string,
	ownedDiskNames []string,
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

	return p.adoptInstance(ctx, instance, createdDiskNames, ownedDiskNames, opts, false)
}

func (p *Provider) adoptInstance(
	ctx context.Context,
	instance *oxide.Instance,
	createdDiskNames []string,
	ownedDiskNames []string,
	opts Options,
	sameLaunch bool,
) (*oxide.Instance, error) {
	name := string(instance.Name)

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
		p.abandonUnusedDisks(ctx, createdDiskNames, sameLaunch)
		return instance, nil

	case oxide.InstanceStateStopped:

		if err := p.joinAffinityGroups(ctx, instance.Id, opts.AffinityGroups); err != nil {
			p.abandon(ctx, instance.Id, ownedDiskNames, err)
			return nil, err
		}
		started, err := p.start(ctx, instance.Id)
		if err != nil {

			p.abandon(ctx, instance.Id, ownedDiskNames, err)
			return nil, err
		}
		p.abandonUnusedDisks(ctx, createdDiskNames, sameLaunch)
		return started, nil

	default:
		abandonErr := fmt.Errorf(
			"instance %s already exists in run state %q and cannot be adopted",
			name, instance.RunState,
		)
		if err := p.Delete(ctx, instance.Id, teardownFor(ownedDiskNames)); err != nil &&
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

func (p *Provider) abandon(ctx context.Context, ref string, diskNames []string, cause error) {
	if err := p.Delete(
		ctx,
		ref,
		teardownFor(diskNames),
	); err != nil &&
		!errors.Is(err, ErrNotFound) {
		log.FromContext(ctx).Error(
			err,
			"unable to clean up an instance that did not launch",
			"instance", ref,
			"cause", cause.Error(),
		)
	}
}

func (p *Provider) abandonUnusedDisks(ctx context.Context, diskNames []string, sameLaunch bool) {
	if sameLaunch || len(diskNames) == 0 {
		return
	}
	log.FromContext(ctx).V(1).Info(
		"removing the disks this attempt created, having adopted an instance attached to another attempt's",
		"disks", diskNames,
	)
	p.abandonDisks(ctx, diskNames, errors.New("the launch adopted an existing instance"))
}

func (p *Provider) abandonUnlessStuck(
	ctx context.Context,
	ref string,
	diskNames []string,
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
	p.abandon(ctx, ref, diskNames, cause)
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

// DeleteDisks removes disks by name or ID, reporting the failures together.
func (p *Provider) DeleteDisks(ctx context.Context, disks []string) error {
	return p.deleteDisks(ctx, disks)
}

func (p *Provider) attachedDisks(
	ctx context.Context,
	instance *oxide.Instance,
	teardown Teardown,
) []string {
	refs := []string{}
	seen := map[string]struct{}{}
	add := func(ref string) {
		if ref == "" {
			return
		}
		if _, ok := seen[ref]; ok {
			return
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}

	if instance.BootDiskId != "" {
		add(instance.BootDiskId)
	} else {
		add(teardown.BootDiskName)
	}

	for _, name := range p.disksMatchingPrefix(ctx, teardown.DiskPrefix) {
		add(name)
	}

	attached, err := p.ox.InstanceDiskListAllPages(
		ctx,
		oxide.InstanceDiskListParams{Instance: oxide.NameOrId(instance.Id)},
	)
	if err != nil {
		log.FromContext(ctx).V(1).Info(
			"unable to list the disks attached to the instance, deleting the disks named by the plan only",
			"instance-id", instance.Id,
			"reason", err.Error(),
		)
		return refs
	}
	for _, disk := range attached {
		add(disk.Id)
	}
	return refs
}

func (p *Provider) deleteDisks(ctx context.Context, disks []string) error {
	var errs []error
	for _, disk := range disks {
		if err := p.deleteDisk(ctx, disk); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Provider) deleteDisk(ctx context.Context, disk string) error {
	if disk == "" {
		return nil
	}

	params := oxide.DiskDeleteParams{
		Disk:    oxide.NameOrId(disk),
		Project: p.projectFor(disk),
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

			err := p.ox.DiskDelete(attemptCtx, params)

			if err == nil || errors.Is(err, oxide.ErrObjectNotFound) {
				return true, nil
			}
			lastErr = err
			log.FromContext(ctx).V(1).Info(
				"disk deletion failed, retrying",
				"disk", disk,
				"reason", err.Error(),
			)
			return false, nil
		},
	)
	if err != nil {
		if wait.Interrupted(err) && lastErr != nil {
			err = lastErr
		}
		return fmt.Errorf("unable to delete disk %s, %w", disk, err)
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

func appendUnique(dst []string, refs ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(refs))
	for _, ref := range dst {
		seen[ref] = struct{}{}
	}
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		dst = append(dst, ref)
	}
	return dst
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
	// BootDiskName is the name the boot disk was created with, for a launch whose boot disk is not under DiskPrefix.
	BootDiskName string
	// DiskPrefix is the prefix this launch named its disks with.
	DiskPrefix string
}

func teardownFor(diskNames []string) Teardown {
	teardown := Teardown{}
	if len(diskNames) > 0 {
		teardown.BootDiskName = diskNames[0]
	}
	return teardown
}

func (p *Provider) ownedDisks(ctx context.Context, teardown Teardown) []string {
	names := []string{}
	if teardown.BootDiskName != "" {
		names = append(names, teardown.BootDiskName)
	}
	return append(names, p.disksMatchingPrefix(ctx, teardown.DiskPrefix)...)
}

func (p *Provider) disksMatchingPrefix(ctx context.Context, prefix string) []string {
	if prefix == "" {
		return nil
	}
	disks, err := p.ox.DiskListAllPages(ctx, oxide.DiskListParams{
		Project: oxide.NameOrId(p.project),
	})
	if err != nil {
		log.FromContext(ctx).V(1).Info(
			"unable to list the project's disks, so any disk this launch created but never attached is left behind",
			"prefix", prefix,
			"reason", err.Error(),
		)
		return nil
	}

	var names []string
	for _, disk := range disks {
		if !strings.HasPrefix(string(disk.Name), prefix) {
			continue
		}
		if attached, ok := disk.State.AsAttached(); ok && attached.Instance != "" {

			continue
		}
		names = append(names, disk.Id)
	}
	return names
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
