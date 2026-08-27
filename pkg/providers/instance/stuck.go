package instance

import (
	"errors"
	"fmt"
	"time"
)

// Kinds of resource a launch can be left waiting on.
const (
	KindDisk     = "disk"
	KindInstance = "instance"
)

// Deadlines bound every transitional state a launch can be caught in.
type Deadlines struct {
	// Launch bounds the whole of one Create, across every call it makes.
	Launch time.Duration
	// Cleanup independently bounds post-failure cleanup after a launch context ends.
	Cleanup time.Duration
	// DiskCreating is how long a disk may sit in creating.
	DiskCreating time.Duration
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
		Launch:           10 * time.Minute,
		Cleanup:          5 * time.Minute,
		DiskCreating:     2 * time.Minute,
		InstanceCreating: 2 * time.Minute,
		InstanceStarting: 5 * time.Minute,
		InstanceStopping: 3 * time.Minute,
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
