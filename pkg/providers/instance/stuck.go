package instance

import (
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
)

// Kinds of resource a launch can be left waiting on.
const (
	KindDisk     = "disk"
	KindInstance = "instance"
)

// cleanupMargin is the slack left between Launch+Cleanup and lifecycle.LaunchTimeout.
const cleanupMargin = 30 * time.Second

// Deadlines bound long-running provider operations.
type Deadlines struct {
	// Launch bounds the whole of one Create, across every call it makes.
	Launch time.Duration
	// Teardown bounds the whole of one Delete, across every call it makes.
	Teardown time.Duration
	// Cleanup independently bounds post-failure cleanup after a launch context ends.
	Cleanup time.Duration
}

// DefaultDeadlines are the deadlines a provider uses when none are configured.
func DefaultDeadlines() Deadlines {
	launch := 3 * time.Minute
	return Deadlines{
		Launch:   launch,
		Teardown: 90 * time.Second,
		Cleanup:  lifecycle.LaunchTimeout - launch - cleanupMargin,
	}
}

// Validate refuses a set of deadlines that could not do its job.
func (d Deadlines) Validate() error {
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"launch deadline", d.Launch},
		{"teardown deadline", d.Teardown},
		{"cleanup deadline", d.Cleanup},
	} {
		if field.value <= 0 {
			return fmt.Errorf("the %s must be positive, got %s", field.name, field.value)
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
