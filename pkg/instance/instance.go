// Package instance wraps the two Oxide instance workflows Karpenter drives: launching an instance
// and tearing one down along with the resources created with it.
//
// Neither is a single API call. A create that starts the instance places it, but the guest still has
// to boot before the instance is any use, and a delete is rejected until the instance has fully
// stopped and leaves the boot disk behind once it succeeds. Both are also resumed rather than
// restarted: Karpenter retries the same NodeClaim, so every step tolerates the work having been
// half done by an earlier attempt.
package instance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// pollInterval is how often an instance's run state is re-read while waiting for a transition to
	// complete.
	pollInterval = 2 * time.Second
	// runTimeout bounds how long Create waits for an instance to reach the running state. The create
	// itself places the instance, so this only covers the transition out of creating and starting.
	runTimeout = 3 * time.Minute
	// stopTimeout bounds how long a single Delete call blocks waiting for an instance to reach a
	// state the API will accept a delete in. Exceeding it is not fatal: the error is returned so the
	// caller retries and picks the wait back up.
	stopTimeout = 2 * time.Minute
	// diskDeleteTimeout bounds how long a single Delete call retries one disk deletion. Disks are
	// detached asynchronously when the instance is deleted, so the first attempts can be rejected,
	// or block, while that is still in flight. Exceeding it is not fatal: the error is returned so
	// the caller retries, and the retry finds the disk by name.
	diskDeleteTimeout = 90 * time.Second
	// diskDeleteAttemptTimeout bounds a single disk delete request. The disk is already detached by
	// the time it is deleted, so the call should return promptly; a disk in a bad state can
	// otherwise hang the request indefinitely and burn the whole retry budget on one attempt.
	diskDeleteAttemptTimeout = 10 * time.Second
)

var (
	// ErrNotFound reports that the instance no longer exists on the rack. Delete returns it once the
	// instance and its disks are all gone, which is the terminal state its caller is waiting for.
	ErrNotFound = errors.New("instance no longer exists")

	// ErrInsufficientCapacity reports that the rack has no room for the instance. Callers are
	// expected to give up on the launch rather than retry it, since nothing about a retry changes
	// how much capacity the rack has.
	ErrInsufficientCapacity = errors.New("insufficient capacity")
)

// Released is the rack capacity an instance gave back when it was torn down.
type Released struct {
	Ncpus  int
	Memory uint64
}

// Provider creates and deletes instances in a single Oxide project.
type Provider struct {
	ox      *oxide.Client
	project string
	// onRelease is called once the rack has confirmed an instance is gone, with what it gave back. It
	// is how the caller learns that capacity it was told did not exist may now exist, without this
	// package knowing anything about instance shapes or scheduling. May be nil.
	onRelease func(context.Context, Released)
}

// NewProvider builds an instance provider. onRelease may be nil, in which case nothing is told about
// the capacity a teardown frees.
func NewProvider(
	ox *oxide.Client,
	project string,
	onRelease func(context.Context, Released),
) *Provider {
	return &Provider{
		ox:        ox,
		project:   project,
		onRelease: onRelease,
	}
}

// Create launches an instance and returns it once it is running.
//
// params is the Oxide API's own create shape, with the project defaulted to the provider's and the
// instance always started. Starting is what places the instance on a sled, so asking for it as part
// of the create is what makes a rack with no room for this instance size fail the create itself
// rather than leaving the instance sitting stopped for a later start to discover. The instance is
// still not reported as launched until its run state is actually running, since placement only gets
// it as far as booting.
//
// An instance left over under the same name from an earlier attempt is adopted where it can be, and
// removed where it cannot so the next attempt starts clean. An instance that will not run is torn
// down before returning, since the caller is being told the launch failed and will never reference
// it again.
//
// The returned error wraps ErrInsufficientCapacity when the rack had no room for the instance,
// whether that surfaced on the create or on the start of an adopted instance.
func (p *Provider) Create(
	ctx context.Context,
	params oxide.InstanceCreateParams,
) (*oxide.Instance, error) {
	if params.Body == nil || params.Body.Name == "" {
		return nil, errors.New("instance create requires a body with a name")
	}
	if params.Project == "" {
		params.Project = oxide.NameOrId(p.project)
	}
	// Not left to the API's default: an instance that is never asked to start is never placed, and a
	// launch that cannot be placed has to fail here, where the caller can still pick a different
	// shape, rather than one poll interval later.
	params.Body.Start = lo.ToPtr(true)
	name := string(params.Body.Name)
	// The name the boot disk is created with, which is the only handle on it left once the instance
	// is gone and can no longer be asked which disk it booted from.
	bootDiskName := bootDiskName(params.Body)

	created, err := p.ox.InstanceCreate(ctx, params)
	if err != nil {
		switch {
		// Nothing was created, so there is nothing to clean up.
		case errors.Is(err, oxide.ErrInsufficientCapacity):
			return nil, fmt.Errorf(
				"unable to create instance %s, %w, %w", name, ErrInsufficientCapacity, err,
			)
		case errors.Is(err, oxide.ErrObjectAlreadyExists):
			if created, err = p.adopt(ctx, name, bootDiskName); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unable to create instance %s, %w", name, err)
		}
	}

	running, err := p.waitForRunning(ctx, created)
	if err != nil {
		p.abandon(ctx, created.Id, bootDiskName, err)
		return nil, err
	}
	return running, nil
}

// Delete tears down an instance and every resource it owns that outlives it, and returns
// ErrNotFound once all of them are gone.
//
// The Oxide API only accepts a delete once an instance has fully stopped, so this stops the
// instance, waits for the run state to settle, deletes the instance, and then deletes the disks it
// was attached to. Only disks outlive the instance: its network interfaces, ephemeral external IPs
// and anti-affinity group memberships are all released by the instance delete itself.
//
// The provider's onRelease is called once the API has confirmed the instance itself is gone, which
// is before the disks are dealt with: the CPU and memory are back on the sled at that point, and a
// disk that will not delete holds neither.
//
// It is safe to call repeatedly, and each step tolerates its resource already being gone. ref is a
// name or an ID, so a launch that failed before it learned the ID can still clean up by name.
// bootDiskName is the name the boot disk was created with, used to finish the cleanup on a retry
// that runs after the instance itself is gone and can no longer be asked what was attached to it.
func (p *Provider) Delete(ctx context.Context, ref string, bootDiskName string) error {
	instance, err := p.ox.InstanceView(ctx, oxide.InstanceViewParams{
		Instance: oxide.NameOrId(ref),
		Project:  p.projectFor(ref),
	})
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {
			// The instance is gone, but an earlier attempt may have died before deleting its boot
			// disk. Deleting by name is the only handle left on it, and is a no-op once the disk is
			// gone too.
			if diskErr := p.deleteDisks(ctx, []string{bootDiskName}); diskErr != nil {
				return diskErr
			}
			return ErrNotFound
		}
		return fmt.Errorf("unable to view the instance, %w", err)
	}

	// Collected before the delete: the instance is the only thing that knows what is attached to it,
	// and deleting it takes that knowledge with it.
	disks := p.attachedDisks(ctx, instance, bootDiskName)

	if err := p.stop(ctx, instance); err != nil {
		if errors.Is(err, ErrNotFound) {
			// The instance disappeared while it was being stopped, so the rack has it back whether or
			// not this call is what removed it.
			p.notifyReleased(ctx, instance)
			if diskErr := p.deleteDisks(ctx, disks); diskErr != nil {
				return diskErr
			}
			return ErrNotFound
		}
		return err
	}

	// Addressed by ID from here on: the view above resolved whatever ref was handed in.
	if err := p.ox.InstanceDelete(ctx, oxide.InstanceDeleteParams{
		Instance: oxide.NameOrId(instance.Id),
	}); err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
		return fmt.Errorf("unable to delete the instance, %w", err)
	}
	// Announced here rather than at the end, because the instance's CPU and memory are back on its
	// sled at this point and a disk that will not delete does not hold either of them. A retry to
	// finish the disks finds the instance already gone and does not announce it twice.
	p.notifyReleased(ctx, instance)

	return p.deleteDisks(ctx, disks)
}

// notifyReleased tells the caller what an instance gave back to the rack. It is only ever called
// once the API has confirmed the instance is gone: a delete that has merely been asked for frees
// nothing, and reporting it would have callers believe in capacity that is still being held.
func (p *Provider) notifyReleased(ctx context.Context, instance *oxide.Instance) {
	if p.onRelease == nil {
		return
	}
	p.onRelease(ctx, Released{
		Ncpus:  int(instance.Ncpus),
		Memory: uint64(instance.Memory),
	})
}

// adopt takes over an instance that already exists under the name a create tried to use, which is
// what an earlier attempt that died after creating the instance looks like.
//
// A stopped instance is one this launch can still pick up, so it is started. An instance in any
// other non-running state is not adoptable, and is torn down so the next attempt creates it from
// scratch. The instance is addressed by name because a create that failed never reported an ID.
func (p *Provider) adopt(ctx context.Context, name, bootDiskName string) (*oxide.Instance, error) {
	log.FromContext(ctx).V(1).Info("instance already exists, adopting it", "instance", name)

	instance, err := p.ox.InstanceView(ctx, oxide.InstanceViewParams{
		Instance: oxide.NameOrId(name),
		Project:  oxide.NameOrId(p.project),
	})
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {
			// Deleted between the create and this view. Nothing is left to adopt or clean up, so let
			// the next attempt create it fresh.
			return nil, fmt.Errorf(
				"instance %s already exists but was deleted before it could be adopted", name,
			)
		}
		return nil, fmt.Errorf("unable to view existing instance %s, %w", name, err)
	}

	switch instance.RunState {
	// Already coming up on its own. The wait picks it up from here.
	case oxide.InstanceStateRunning,
		oxide.InstanceStateStarting,
		oxide.InstanceStateRebooting,
		oxide.InstanceStateMigrating,
		oxide.InstanceStateCreating:
		return instance, nil

	case oxide.InstanceStateStopped:
		started, err := p.start(ctx, instance.Id)
		if err != nil {
			// Whatever the reason, this instance is not going to run, and its user-data holds a join
			// credential from the earlier attempt that may already have expired. Tear it down so the
			// next attempt builds a new one.
			p.abandon(ctx, instance.Id, bootDiskName, err)
			return nil, err
		}
		return started, nil

	// Stopping, failed or destroyed. None of these can be started, so the name has to be freed
	// before another create under it can succeed.
	default:
		abandonErr := fmt.Errorf(
			"instance %s already exists in run state %q and cannot be adopted",
			name, instance.RunState,
		)
		if err := p.Delete(ctx, instance.Id, bootDiskName); err != nil &&
			!errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w, and it could not be terminated, %w", abandonErr, err)
		}
		return nil, fmt.Errorf("%w, so it was terminated", abandonErr)
	}
}

// waitForRunning waits for an instance to reach the running state and returns its final view.
func (p *Provider) waitForRunning(
	ctx context.Context,
	instance *oxide.Instance,
) (*oxide.Instance, error) {
	if instance.RunState == oxide.InstanceStateRunning {
		return instance, nil
	}

	pollCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	current := instance
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
				return false, fmt.Errorf("unable to view the instance, %w", err)
			}
			current = view

			switch view.RunState {
			case oxide.InstanceStateRunning:
				return true, nil

			// The create already placed this instance and asked it to start, so none of these are
			// states it comes back from on its own. Starting it again would be starting an instance
			// that has already given up once, and waiting out runTimeout on it only delays
			// rescheduling.
			case oxide.InstanceStateStopped,
				oxide.InstanceStateFailed,
				oxide.InstanceStateDestroyed:
				return false, errNotPlaced(view.RunState)

			// Still booting.
			default:
				return false, nil
			}
		},
	)
	if err == nil {
		return current, nil
	}

	switch {
	case errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("instance %s was deleted before it started running", instance.Id)
	case wait.Interrupted(err):
		return nil, fmt.Errorf(
			"instance %s did not start running within %s, last observed run state %q",
			instance.Id, runTimeout, current.RunState,
		)
	default:
		return nil, err
	}
}

// start starts a stopped instance left behind by an earlier attempt. Starting is the placement
// decision, so this is where an adoption of an instance the rack no longer has room for fails.
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

// stop stops an instance and waits until its run state is one the API will accept a delete in.
// Instances that are already stopping or terminating are only waited on.
func (p *Provider) stop(ctx context.Context, instance *oxide.Instance) error {
	if deletable(instance.RunState) {
		return nil
	}

	// Nothing to ask for while a stop or a delete started by an earlier attempt is in flight;
	// issuing another stop is rejected as an invalid request.
	if instance.RunState != oxide.InstanceStateStopping {
		if _, err := p.ox.InstanceStop(ctx, oxide.InstanceStopParams{
			Instance: oxide.NameOrId(instance.Id),
		}); err != nil {
			switch {
			case errors.Is(err, oxide.ErrObjectNotFound):
				return ErrNotFound
			// The instance raced into stopping or terminating between the view and the stop. Both
			// are headed where we want, so fall through to the wait.
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

	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
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
			return fmt.Errorf(
				"instance %s did not stop within %s, last observed run state %q",
				instance.Id, stopTimeout, lastState,
			)
		}
		return err
	}
	return nil
}

// abandon tears down an instance a launch is giving up on. The caller is about to be told the
// launch failed and will never reference the instance again, so left behind it would hold rack
// capacity and a boot disk indefinitely. Cleanup failures are logged rather than returned: cause is
// why the launch is being abandoned and is the more useful error to surface.
func (p *Provider) abandon(ctx context.Context, ref, bootDiskName string, cause error) {
	if err := p.Delete(ctx, ref, bootDiskName); err != nil && !errors.Is(err, ErrNotFound) {
		log.FromContext(ctx).Error(
			err,
			"unable to clean up an instance that did not launch",
			"instance", ref,
			"cause", cause.Error(),
		)
	}
}

// attachedDisks returns references to every disk that has to be deleted along with the instance,
// deduplicated and preferring IDs over names.
//
// The boot disk is always included: these instances are created with one, and it is the only disk
// still identifiable once the instance is gone. The attachment list refines that, so a failure to
// read it falls back to the boot disk alone rather than failing the delete and leaving the instance
// running.
func (p *Provider) attachedDisks(
	ctx context.Context,
	instance *oxide.Instance,
	bootDiskName string,
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
		add(bootDiskName)
	}

	attached, err := p.ox.InstanceDiskListAllPages(
		ctx,
		oxide.InstanceDiskListParams{Instance: oxide.NameOrId(instance.Id)},
	)
	if err != nil {
		log.FromContext(ctx).V(1).Info(
			"unable to list the disks attached to the instance, deleting the boot disk only",
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

// deleteDisks deletes every disk it is given, and reports the failures together so one bad disk
// does not hide the rest or leave them behind.
func (p *Provider) deleteDisks(ctx context.Context, disks []string) error {
	var errs []error
	for _, disk := range disks {
		if err := p.deleteDisk(ctx, disk); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deleteDisk deletes a single disk by name or ID. Deletion is retried until it succeeds or the
// overall timeout expires, with each attempt individually bounded: the API rejects the request
// while the disk is still detaching from the instance that was just deleted, and a disk in a bad
// state can hang the request rather than return an error.
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
			// A missing disk means an earlier attempt already deleted it.
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

// projectFor returns the project a resource lookup should be scoped by. A project is what makes a
// name unambiguous, but the API rejects one passed alongside an ID, which is unambiguous already.
func (p *Provider) projectFor(ref string) oxide.NameOrId {
	if _, err := uuid.Parse(ref); err == nil {
		return ""
	}
	return oxide.NameOrId(p.project)
}

// bootDiskName returns the name the create is giving the instance's boot disk, which is the handle
// cleanup needs when the instance itself can no longer be asked for it. It is empty when the boot
// disk is an existing one being attached, since that disk is not this launch's to delete.
func bootDiskName(body *oxide.InstanceCreate) string {
	if create, ok := body.BootDisk.Value.(*oxide.InstanceDiskAttachmentCreate); ok {
		return string(create.Name)
	}
	if create, ok := body.BootDisk.Value.(oxide.InstanceDiskAttachmentCreate); ok {
		return string(create.Name)
	}
	return ""
}

// errNotPlaced builds the error a launch fails with when an instance that was created and asked to
// start settled somewhere other than running. It is reported as insufficient capacity so the
// NodeClaim is rescheduled onto a different shape immediately: an instance that gave up on its own
// is not going to come up on a retry under the same name, and waiting out the registration timeout
// to learn that only delays the pods.
func errNotPlaced(state oxide.InstanceState) error {
	return fmt.Errorf(
		"instance entered run state %q instead of running, "+
			"the rack likely could not accommodate it, %w",
		state, ErrInsufficientCapacity,
	)
}

// deletable reports whether the Oxide API accepts a delete for an instance in this run state.
// Anything else is either running or still transitioning.
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
