package instance

import (
	"errors"
	"fmt"
	"time"

	diskprovider "github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/disk"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
)

// Kinds of resource a launch can be left waiting on.
const (
	KindDisk     = "disk"
	KindInstance = "instance"
)

// Deadlines bound long-running provider operations and transitional states.
type Deadlines struct {
	// Launch bounds the whole of one Create, across every call it makes.
	Launch time.Duration
	// Cleanup independently bounds post-failure cleanup after a launch context ends.
	Cleanup time.Duration
	// DiskCreating is how long a disk may sit in creating.
	DiskCreating time.Duration
	// DiskDeleting is how long deleting one disk may be retried.
	DiskDeleting time.Duration
	// InstanceCreating is how long an instance may sit in creating.
	InstanceCreating time.Duration
	// InstanceStarting is how long an instance may sit in starting after it has been placed.
	InstanceStarting time.Duration
	// InstanceStopping is how long a teardown waits for an instance to reach a state the API accepts a delete in.
	InstanceStopping time.Duration
}

// DefaultDeadlines are the deadlines a provider uses when none are configured.
func DefaultDeadlines() Deadlines {
	return Deadlines{
		Launch:           3 * time.Minute,
		Cleanup:          90 * time.Second,
		DiskCreating:     time.Minute,
		DiskDeleting:     diskprovider.DefaultDeleteTimeout,
		InstanceCreating: time.Minute,
		InstanceStarting: 5 * time.Minute,
		InstanceStopping: time.Minute,
	}
}

// Validate refuses a set of deadlines that could not do its job.
func (d Deadlines) Validate() error {
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"launch deadline", d.Launch},
		{"cleanup deadline", d.Cleanup},
		{"disk creating deadline", d.DiskCreating},
		{"disk deleting deadline", d.DiskDeleting},
		{"instance creating deadline", d.InstanceCreating},
		{"instance starting deadline", d.InstanceStarting},
		{"instance stopping deadline", d.InstanceStopping},
	} {
		if field.value <= 0 {
			return fmt.Errorf("the %s must be positive, got %s", field.name, field.value)
		}
	}

	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"disk creating deadline", d.DiskCreating},
		{"instance creating deadline", d.InstanceCreating},
	} {
		if field.value >= d.Launch {
			return fmt.Errorf(
				"the %s (%s) must be shorter than the launch deadline (%s)",
				field.name, field.value, d.Launch,
			)
		}
	}

	// A failing Create runs its cleanup before it returns, so this is the wall time upstream sees.
	// Overrunning lifecycle.LaunchTimeout gets the NodeClaim deleted without a ProviderID, and its
	// finalizer then skips the cloud Delete entirely, orphaning whatever the launch created.
	if total := d.Launch + d.Cleanup; total >= lifecycle.LaunchTimeout {
		return fmt.Errorf(
			"the launch (%s) and cleanup (%s) deadlines total %s, which must be shorter than upstream's launch timeout (%s)",
			d.Launch,
			d.Cleanup,
			total,
			lifecycle.LaunchTimeout,
		)
	}
	return nil
}

// ErrStuckLaunchResource is what every stuck resource matches with errors.Is.
var ErrStuckLaunchResource = errors.New("launch resource stuck in a transitional state")

// StuckLaunchResourceError reports an Oxide resource that a launch gave up waiting on.
type StuckLaunchResourceError struct {
	// Kind is KindDisk or KindInstance.
	Kind string
	// ID is the Oxide UUID, empty when the resource was never viewed successfully.
	ID string
	// Name is the deterministic name the launch created it under, which is how it is found again when there is no ID.
	Name string
	// State is the transitional state it was last observed in.
	State string
	// StuckFor is how long it was observed in that state before the launch gave up.
	StuckFor time.Duration
}

func (e *StuckLaunchResourceError) Error() string {
	return fmt.Sprintf(
		"%s %s (id %q) has been in state %q for %s and the launch is being abandoned; "+
			"it is left for the orphan reaper to remove",
		e.Kind, e.Name, e.ID, e.State, e.StuckFor.Round(time.Second),
	)
}

// Is makes every stuck resource match ErrStuckLaunchResource.
func (e *StuckLaunchResourceError) Is(target error) bool {
	return target == ErrStuckLaunchResource
}
