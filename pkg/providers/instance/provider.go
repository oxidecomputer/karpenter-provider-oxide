// Package instance wraps the two Oxide instance workflows Karpenter drives: launching an instance
// and tearing one down along with the resources created with it.
//
// Neither is a single API call. A launch is three, and the split is the whole design: the boot disk
// is created, then the instance is created stopped, then the instance is started. A delete is
// rejected until the instance has fully stopped and leaves the boot disk behind once it succeeds.
// Both are also resumed rather than restarted: Karpenter retries the same NodeClaim, so every step
// tolerates the work having been half done by an earlier attempt.
//
// A launch is split into three calls because each one asks the rack a different question and answers
// it before returning, which is what lets a refusal be attributed to the thing that was actually
// refused.
//
//   - Creating the boot disk asks whether there is storage. Crucible allocates the disk's regions
//     before the call returns, so a rack with nowhere to put them refuses here.
//   - Creating the instance stopped asks nothing of any sled. It only writes down what the instance
//     is going to be.
//   - Starting the instance is the placement decision: this is where a sled is chosen and reserved,
//     and where a rack with no room for the shape refuses.
//
// The gap between the create and the start is also the only place an affinity group membership can be
// added: the API has no create-time field for one, and after the start the placement it was supposed
// to shape has already been made.
//
// The alternative the API offers is one create that provisions the disk and starts the instance
// together. It is rejected here for two separate reasons. A create that provisions the disk too
// refuses a launch the same way whether the rack ran out of storage or the sleds ran out of CPU and
// memory: both come back as one InsufficientCapacity, and the message that would tell them apart is
// deliberately not part of the API contract. The two call for opposite responses -- a sled that
// cannot fit this shape is a reason to try a different shape, whereas a rack that cannot place a
// Crucible region is not, since regions are allocated across three sleds independently of wherever
// the instance lands and no shape is any smaller in the dimension that ran out.
//
// And a create that starts the instance does not hold the response until it has been placed. The
// refusal arrives afterwards, leaving nothing to report it on: the caller is handed an instance that
// looks fine and has to discover by watching that it never came up. Which is not merely slower. The
// state an unplaced instance sits in is stopped, and so is the state an instance passes through on
// its way up, so a caller watching for one finds the other, and reports healthy launches as a rack
// with no room for the shape. Asking for the start explicitly costs one more round trip and gets a
// synchronous, unambiguous answer, which is worth considerably more.
//
// It sits under pkg/providers with the resolving providers so that everything talking to the Oxide
// API about one resource is one directory, and nothing else about it is shared with them: a launch is
// a write driven by a NodeClaim rather than a reading of a nodeclass's selectors, so there is nothing
// here to cache and nothing here to drift.
//
// Nothing here waits for the guest to boot. Placement is what the launch has to establish, and the
// start establishes it; whether the node then registers is Karpenter's to time out on, and it
// already does.
package instance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/google/uuid"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// pollInterval is how often an instance's run state is re-read while waiting for a transition to
	// complete. Only teardown waits on one; a launch is answered by the calls it makes.
	pollInterval = 2 * time.Second
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
	// diskReadyTimeout bounds how long Create waits for a boot disk to become attachable. The create
	// allocates the disk's regions before it returns, so this only covers the tail of a disk that
	// came back still settling.
	diskReadyTimeout = 2 * time.Minute
)

var (
	// ErrNotFound reports that the instance no longer exists on the rack. Delete returns it once the
	// instance and its disks are all gone, which is the terminal state its caller is waiting for.
	ErrNotFound = errors.New("instance no longer exists")

	// ErrInsufficientCapacity reports that no sled has room for the instance's CPU and memory.
	// Callers are expected to give up on the launch rather than retry it, since nothing about a retry
	// changes how much capacity the rack has. It comes from the start, which is the call that places
	// the instance, so it is only ever about the shape and never about its boot disk.
	ErrInsufficientCapacity = errors.New("insufficient capacity")

	// ErrInsufficientStorage reports that the boot disk could not be provisioned for want of room,
	// either in the silo's storage quota or across the rack's zpools.
	//
	// Deliberately not wrapping ErrInsufficientCapacity: the two are disjoint so that a caller
	// choosing an instance shape cannot mistake one for the other. A storage refusal says nothing
	// about the shape being launched, so treating it as one would take shapes out of consideration
	// that were never the problem, and would leave the shape that could not be placed for want of
	// CPU still in it.
	ErrInsufficientStorage = errors.New("insufficient storage capacity")
)

// Options is the per-launch configuration a create cannot carry in the Oxide API's own create shape.
// It is a parameter rather than a field on Provider because every value in it belongs to one launch.
type Options struct {
	// AffinityGroups are the affinity groups, by ID, the instance joins between the create and the
	// start. Anti-affinity groups do not appear here: they have a create-time field, which is free,
	// atomic, and already in place before the start makes the placement decision.
	AffinityGroups []string
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

// Create launches an instance and returns it once the rack has placed it on a sled.
//
// params is the Oxide API's own create shape, with the project defaulted to the provider's and the
// instance explicitly not started. The start is issued as its own call afterwards, because that is
// the one that places the instance and the only one that reports a rack with no room for the shape
// while the caller can still do something about it. See the package comment.
//
// A boot disk the params ask to have created is created first, and the params are rewritten to
// attach it by name. A boot disk the params ask to attach is left alone: it already exists, and it
// is not this launch's to provision or to clean up.
//
// An instance or boot disk left over under the same name from an earlier attempt is adopted where it
// can be, and removed where it cannot so the next attempt starts clean. Anything this call created
// for a launch that then failed is torn down before returning, since the caller is being told the
// launch failed and will never reference any of it again.
//
// It returns as soon as the instance has been placed, not once it is running. Placement is what a
// launch has to establish and what the caller is choosing a shape against; an instance that is
// placed and then fails to boot is a node that never registers, which Karpenter already times out
// on and which no shape the caller could pick instead would avoid.
//
// The returned error wraps ErrInsufficientStorage when the boot disk could not be provisioned for
// want of room, and ErrInsufficientCapacity when no sled had room for the instance. The two are
// never both present: they come from different calls, and the first to fail ends the launch.
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
	// Not left to the API's default, which is to start. A create that starts the instance does not
	// wait for it to be placed, so the refusal this launch exists to catch would arrive after the
	// call had already returned success. Creating it stopped keeps placement in a call of its own.
	params.Body.Start = new(false)
	name := string(params.Body.Name)
	// Every disk name this launch owns, which is the only handle on them left once the instance is
	// gone and can no longer be asked what was attached to it. Resolved before anything is
	// provisioned, because provisioning rewrites the params to attach the distributed disks instead
	// of creating them, and asking to create a disk is what makes it this launch's to delete.
	//
	// Local disks are in this set too. They are created by the instance create rather than ahead of
	// it, but they are still named by the plan and still this launch's to remove.
	diskNames := diskNames(params.Body)

	if err := p.ensureDisks(ctx, params.Body, diskNames); err != nil {
		return nil, err
	}

	created, err := p.ox.InstanceCreate(ctx, params)
	if err != nil {
		switch {
		// Not expected of a create that starts nothing, since no sled is asked for anything. Handled
		// because the silo's CPU and memory are charged here rather than on the start, so a quota
		// with no headroom is refused by this call, under the same error code the rack uses.
		case errors.Is(err, oxide.ErrInsufficientCapacity):
			// The boot disk exists and this launch is over, so it has to go. Karpenter reschedules an
			// insufficient capacity failure onto a fresh NodeClaim under a different name, which would
			// never come looking for this disk.
			p.abandonDisks(ctx, diskNames, err)
			return nil, fmt.Errorf(
				"unable to create instance %s, %w, %w", name, ErrInsufficientCapacity, err,
			)
		case errors.Is(err, oxide.ErrObjectAlreadyExists):
			// An adopted instance is already started, or has been started by the adoption, so it has
			// had its placement decided and does not go through the start below again.
			return p.adopt(ctx, name, diskNames, opts)
		default:
			p.abandonDisks(ctx, diskNames, err)
			return nil, fmt.Errorf("unable to create instance %s, %w", name, err)
		}
	}

	// Affinity groups are joined between the create and the start because the start is what places
	// the instance. The Oxide API has no create-time field for them the way it does for
	// anti-affinity, so a membership added after the start would be recorded against a placement it
	// did not influence.
	if err := p.joinAffinityGroups(ctx, created.Id, opts.AffinityGroups); err != nil {
		// The same path a failed start takes: nothing else comes back for this instance, so it would
		// otherwise hold rack capacity, its boot disk and its name indefinitely.
		p.abandon(ctx, created.Id, diskNames, err)
		return nil, err
	}

	started, err := p.start(ctx, created.Id)
	if err != nil {
		// Nothing else will come back for this instance: the caller is about to be told the launch
		// failed, and an insufficient capacity failure is rescheduled onto a NodeClaim under a
		// different name. Left behind, it would hold its boot disk and its name indefinitely.
		p.abandon(ctx, created.Id, diskNames, err)
		return nil, err
	}
	return started, nil
}

// Delete tears down an instance and every resource it owns that outlives it, and returns
// ErrNotFound once all of them are gone.
//
// The Oxide API only accepts a delete once an instance has fully stopped, so this stops the
// instance, waits for the run state to settle, deletes the instance, and then deletes the disks it
// was attached to. Only disks outlive the instance: its network interfaces, ephemeral external IPs
// and affinity and anti-affinity group memberships are all released by the instance delete itself.
//
// The provider's onRelease is called once the API has confirmed the instance itself is gone, which
// is before the disks are dealt with: the CPU and memory are back on the sled at that point, and a
// disk that will not delete holds neither.
//
// It is safe to call repeatedly, and each step tolerates its resource already being gone. ref is a
// name or an ID, so a launch that failed before it learned the ID can still clean up by name, and
// teardown carries the handles needed to finish the cleanup on a retry that runs after the instance
// itself is gone and can no longer be asked what was attached to it.
func (p *Provider) Delete(ctx context.Context, ref string, teardown Teardown) error {
	instance, err := p.ox.InstanceView(ctx, oxide.InstanceViewParams{
		Instance: oxide.NameOrId(ref),
		Project:  p.projectFor(ref),
	})
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {
			// The instance is gone, but an earlier attempt may have died before deleting its disks.
			// Names are the only handle left on them, and deleting one that is already gone is a
			// no-op.
			if diskErr := p.deleteDisks(ctx, p.ownedDisks(ctx, teardown)); diskErr != nil {
				return diskErr
			}
			return ErrNotFound
		}
		return fmt.Errorf("unable to view the instance, %w", err)
	}

	// Collected before the delete: the instance is the only thing that knows what is attached to it,
	// and deleting it takes that knowledge with it.
	disks := p.attachedDisks(ctx, instance, teardown)

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

// ensureDisks provisions every distributed disk an instance create asked for and rewrites the create
// to attach them, leaving local disks for the instance create itself to make.
//
// Distributed disks are pre-created because the Oxide API allocates their regions before the create
// returns, so a rack with nowhere to put them fails here rather than leaving a disk to fault later.
// That is what makes this the call that answers "is there storage", and it is reported as
// ErrInsufficientStorage so the caller does not read it as a verdict on the instance shape.
//
// Local disks are deliberately left alone. See distributed for why.
//
// They are provisioned concurrently. Seven disks waiting their turn through diskReadyTimeout each
// would run to a worst case longer than Karpenter's registration timeout, so a slow rack would look
// like a node that never came up.
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
		// Everything this launch owns goes, not just what this call managed to create. A name in the
		// owned set can only ever have been created by an earlier attempt at this same launch, and
		// this launch is over: Karpenter reschedules an insufficient capacity failure onto a fresh
		// NodeClaim under a different name, which would never come looking for any of it.
		p.abandonDisks(ctx, owned, err)
		return err
	}

	// Rewritten only once every pre-created disk exists, so a partial failure above leaves the create
	// params untouched rather than half converted.
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

// ensureDisk provisions one disk and returns once it is attachable, adopting or replacing one left
// behind by an earlier attempt.
func (p *Provider) ensureDisk(
	ctx context.Context,
	spec *oxide.InstanceDiskAttachmentCreate,
	instanceName string,
) error {
	name := string(spec.Name)

	// At most one replacement pass. The second create runs against a name this call just freed, so a
	// conflict there is a concurrent launch rather than leftover state, and no amount of retrying
	// within this launch resolves that.
	for attempt := 0; ; attempt++ {
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
				// Left for the caller to clean up along with everything else the launch owns; a
				// sibling disk may be in flight concurrently and is not this call's to remove.
				return err
			}
			return nil
		}

		switch {
		case errors.Is(err, oxide.ErrInsufficientCapacity):
			return fmt.Errorf(
				"unable to create disk %s, %w, %w", name, ErrInsufficientStorage, err,
			)

		case errors.Is(err, oxide.ErrObjectAlreadyExists) && attempt == 0:
			replace, err := p.adoptDisk(ctx, spec, instanceName)
			if err != nil {
				return err
			}
			if !replace {
				return nil
			}
			// The name is free now, so fall through and create it again.

		default:
			return fmt.Errorf("unable to create disk %s, %w", name, err)
		}
	}
}

// adoptDisk takes over a disk that already exists under the name a create tried to use, which is
// what an earlier attempt that died between creating the disk and creating the instance looks like.
// It reports whether the caller should create the disk again, having removed the one that was in the
// way.
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
			// Deleted between the create and this view. Something else is working on this name, and
			// creating it again here would race that rather than resolve it.
			return false, fmt.Errorf(
				"disk %s already exists but was deleted before it could be adopted", name,
			)
		}
		return false, fmt.Errorf("unable to view existing disk %s, %w", name, err)
	}

	// Never taken from another instance. A disk name embeds a fingerprint of the instance name, so a
	// name already attached elsewhere means that fingerprint collided -- astronomically unlikely, but
	// the difference between reporting it and acting on it is the difference between a failed launch
	// and somebody else's data. Refused rather than adopted or deleted.
	//
	// A disk attached to this launch's own instance is a different thing entirely, and is adopted: it
	// is what a retry of an attempt that got as far as creating the instance looks like.
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

	// Adopted on state alone. A disk whose size no longer matches the OxideNodeClass is adopted like
	// any other: it works, just not as the class now describes, which is drift and belongs to whatever
	// reconciles drift rather than to a launch in progress. Replacing it here would also make a disk
	// size edit destroy disks as a side effect of an unrelated retry.
	switch state := existing.State.State(); state {
	// Usable already, or on its way there under its own steam.
	case oxide.DiskStateStateCreating,
		oxide.DiskStateStateDetached,
		oxide.DiskStateStateAttaching,
		oxide.DiskStateStateAttached:
		return false, p.waitForDiskReady(ctx, existing)

	// Detaching, faulted, destroyed, or partway through an import this provider never starts. None of
	// these become a usable disk on their own, so the name has to be freed before a create under it
	// can succeed.
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

// waitForDiskReady waits for a disk to reach a state the instance create can attach it in.
//
// A disk that comes back already attachable is the common case: the create allocated its regions
// synchronously. This only covers the tail, and the states it gives up on are the ones a disk does
// not leave on its own.
func (p *Provider) waitForDiskReady(ctx context.Context, disk *oxide.Disk) error {
	if diskAttachable(disk.State.State()) {
		return nil
	}

	pollCtx, cancel := context.WithTimeout(ctx, diskReadyTimeout)
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
						"boot disk %s was deleted before it became usable", disk.Name,
					)
				}
				return false, fmt.Errorf("unable to view the boot disk, %w", err)
			}
			lastState = view.State.State()

			switch {
			case diskAttachable(lastState):
				return true, nil

			// Terminal. Reported as a plain failure rather than as insufficient storage: the create
			// already got past the point where a rack with no room refuses, so whatever went wrong
			// here is not something a smaller request would have avoided.
			case lastState == oxide.DiskStateStateFaulted,
				lastState == oxide.DiskStateStateDestroyed:
				return false, fmt.Errorf(
					"boot disk %s entered state %q instead of becoming usable",
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
		return fmt.Errorf(
			"boot disk %s did not become usable within %s, last observed state %q",
			disk.Name, diskReadyTimeout, lastState,
		)
	}
	return err
}

// abandonDisks removes the disks this launch created and is giving up on. The caller is about to be
// told the launch failed, and Karpenter retries insufficient capacity onto a NodeClaim with a
// different name, so a disk left behind here is one nothing ever comes back for. Failures are logged
// rather than returned: cause is why the launch is being abandoned and is the more useful error to
// surface.
//
// It is given the whole set the launch owns rather than the disks that happened to be created,
// because a name in that set can only ever have been created by an attempt at this same launch, and
// deleting one that was never created is a no-op. An empty set means every disk was one the caller
// asked to attach, which this launch did not create and must not delete.
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

// adopt takes over an instance that already exists under the name a create tried to use, which is
// what an earlier attempt that died after creating the instance looks like.
//
// A stopped instance is one this launch can still pick up, so it is started, which is the same
// placement decision a fresh launch makes and fails the same way. An instance in any other
// non-running state is not adoptable, and is torn down so the next attempt creates it from scratch.
// The instance is addressed by name because a create that failed never reported an ID.
func (p *Provider) adopt(
	ctx context.Context,
	name string,
	diskNames []string,
	opts Options,
) (*oxide.Instance, error) {
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
	// Already placed. Whatever earlier attempt created this instance got as far as starting it, so
	// the rack has had room for the shape since, and there is nothing left for this launch to do.
	case oxide.InstanceStateRunning,
		oxide.InstanceStateStarting,
		oxide.InstanceStateRebooting,
		oxide.InstanceStateMigrating,
		oxide.InstanceStateCreating:
		return instance, nil

	case oxide.InstanceStateStopped:
		// Re-issued rather than assumed: an earlier attempt may have died between the create and the
		// joins. A membership already in place comes back as a conflict, which counts as success.
		if err := p.joinAffinityGroups(ctx, instance.Id, opts.AffinityGroups); err != nil {
			p.abandon(ctx, instance.Id, diskNames, err)
			return nil, err
		}
		started, err := p.start(ctx, instance.Id)
		if err != nil {
			// Whatever the reason, this instance is not going to run, and its user-data holds a join
			// credential from the earlier attempt that may already have expired. Tear it down so the
			// next attempt builds a new one.
			p.abandon(ctx, instance.Id, diskNames, err)
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
		if err := p.Delete(ctx, instance.Id, teardownFor(diskNames)); err != nil &&
			!errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w, and it could not be terminated, %w", abandonErr, err)
		}
		return nil, fmt.Errorf("%w, so it was terminated", abandonErr)
	}
}

// joinAffinityGroups adds an instance to every affinity group a launch asks for, before it is
// started.
//
// This exists only because affinity groups have no create-time field the way anti-affinity groups do.
// It has to run before the start, which is the call that chooses a sled, or the membership records a
// preference against a placement that was made without it.
//
// A membership already in place is success: an earlier attempt at this launch may have added it
// before dying, and this call is asking for a state, not for a transition.
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

// start places a stopped instance on a sled, and is the call a launch turns on.
//
// Starting is the placement decision, and the API makes it before answering: a rack with no sled
// that has room for the instance's CPU and memory refuses here, synchronously, while the caller can
// still choose a different shape. That is the whole reason instances are created stopped, and it is
// why nothing downstream has to infer capacity from a run state.
//
// A refusal is about the shape and nothing else. The boot disk was provisioned before the instance
// was created, so storage has already been established by the time this runs.
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
	// Unioned with the attachment listing rather than used only as a fallback. A disk created between
	// the disk provisioning and the instance create is this launch's, is named from the same plan, and
	// is attached to nothing, so no attachment listing can report it.
	for _, name := range p.disksMatchingPrefix(ctx, teardown.DataPrefix) {
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

// diskNames returns every disk name a create is asking to have made, which is the whole set the
// launch owns and therefore the whole set cleanup has to consider. A disk being attached rather than
// created is not in it, since such a disk is not this launch's to delete.
func diskNames(body *oxide.InstanceCreate) []string {
	specs := diskCreateSpecs(body)
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, string(spec.Name))
	}
	return names
}

// diskCreateSpecs returns every disk a create is asking to have made, the boot disk first, in the
// order the create lists them.
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

// diskCreateSpec returns the disk an attachment is asking to have made, if it is asking for one. The
// API accepts either variant by value or by pointer, so both are matched.
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

// distributed reports whether a disk create asks for a distributed disk rather than a sled-local one.
//
// It is what decides whether the disk is provisioned ahead of the instance. A distributed disk's
// regions are allocated across three sleds chosen independently of where the instance lands, so a
// refusal is about storage and is worth establishing before anything else is created. A local disk's
// capacity belongs to the one sled the instance is placed on, so a refusal is a placement refusal:
// creating it ahead of time would decide the sled before the instance was placed, which is not a
// decision this provider is in a position to make.
func distributed(spec *oxide.InstanceDiskAttachmentCreate) bool {
	_, local := spec.DiskBackend.AsLocal()
	return !local
}

// diskAttachable reports whether an instance create can attach a disk in this state. Attached counts:
// it is what a disk the instance being adopted already booted from looks like, and asking for it
// again is not what fails such a launch.
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

// Teardown is how a delete finds the resources a launch created, once the instance itself is gone and
// can no longer be asked what was attached to it.
//
// Both fields are derived from the NodeClaim name alone, deliberately: the OxideNodeClass may have
// been edited or deleted between the launch and the teardown, so nothing here may depend on it. That
// is what makes cleaning up N disks tractable without a per-launch record of what was created.
type Teardown struct {
	// BootDiskName is the name the boot disk was created with.
	BootDiskName string
	// DataPrefix is the prefix every data disk of this launch was named with. It is what finds a disk
	// that was created but never attached, which no attachment listing can report.
	DataPrefix string
}

// teardownFor builds a Teardown from the disk names a launch owns, for the internal callers that
// already have them. The boot disk is the first name the plan produces.
func teardownFor(diskNames []string) Teardown {
	teardown := Teardown{}
	if len(diskNames) > 0 {
		teardown.BootDiskName = diskNames[0]
	}
	return teardown
}

// ownedDisks is every disk name a teardown can find, used on the path where the instance is already
// gone and its attachment list is no longer available.
func (p *Provider) ownedDisks(ctx context.Context, teardown Teardown) []string {
	names := []string{}
	if teardown.BootDiskName != "" {
		names = append(names, teardown.BootDiskName)
	}
	return append(names, p.disksMatchingPrefix(ctx, teardown.DataPrefix)...)
}

// disksMatchingPrefix lists the project's disks and returns those a launch named with this prefix.
//
// A project-wide list is the price of there being no other way to find a disk that was created but
// never attached: the Oxide API has no name-prefix filter, and an unattached disk appears in no
// instance's attachment list. It runs once per teardown.
//
// A disk attached to some other instance is never returned. A data disk name embeds a fingerprint of
// the instance name, so one attached elsewhere means that fingerprint collided, and the difference
// between skipping it and deleting it is the difference between a leaked disk and somebody else's
// data.
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
			// Attached disks are already covered by the instance's own attachment list on the path
			// that has one, and on the path that does not, one attached elsewhere is not ours.
			continue
		}
		names = append(names, disk.Id)
	}
	return names
}

// instanceIs reports whether the instance with this ID is the one named. It is used to tell a disk
// left attached by an earlier attempt at this launch, which is adoptable, from one belonging to
// another instance, which is not.
//
// An instance that cannot be viewed is treated as not being this one. That is the safe direction: it
// refuses the adoption and fails the launch rather than taking a disk whose owner it could not
// establish.
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
