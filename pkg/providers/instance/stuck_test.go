package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/oxidecomputer/oxide.go/oxide"
)

func shortDeadlines() Deadlines {
	return Deadlines{
		Launch:           5 * time.Second,
		Cleanup:          time.Second,
		DiskCreating:     10 * time.Millisecond,
		DiskDeleting:     10 * time.Millisecond,
		InstanceCreating: 10 * time.Millisecond,
		InstanceStarting: time.Minute,
		InstanceStopping: 10 * time.Millisecond,
	}
}

func TestCreateAbandonsAStuckDiskWithoutDeletingIt(t *testing.T) {
	api := &launchAPI{stuckDisk: true}
	p := NewProvider(api.server(t), "project", nil, WithDeadlines(shortDeadlines()))

	_, err := p.Create(context.Background(), launchParams(), testOptions())
	if err == nil {
		t.Fatal("Create() succeeded, want it abandoned over the stuck disk")
	}

	var stuck *StuckLaunchResourceError
	if !errors.As(err, &stuck) {
		t.Fatalf("Create() error = %v, want a StuckLaunchResourceError", err)
	}
	if stuck.Kind != KindDisk {
		t.Errorf("stuck kind = %q, want %q", stuck.Kind, KindDisk)
	}
	if stuck.State != string(oxide.DiskStateStateCreating) {
		t.Errorf("stuck state = %q, want %q", stuck.State, oxide.DiskStateStateCreating)
	}
	if stuck.Name != "karpenter-node" {
		t.Errorf("stuck name = %q, want the disk the launch created", stuck.Name)
	}

	if len(api.diskDeletes) != 0 {
		t.Errorf("disk deletes = %v, want the stuck disk left for the reaper", api.diskDeletes)
	}

	if api.instanceCreates != 0 {
		t.Errorf("instance creates = %d, want none after the disk failed", api.instanceCreates)
	}
}

func TestCreateStillCleansUpAfterARefusal(t *testing.T) {
	api := &launchAPI{startError: "InsufficientCapacity"}
	p := NewProvider(api.server(t), "project", nil, WithDeadlines(shortDeadlines()))

	_, err := p.Create(context.Background(), launchParams(), testOptions())
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("Create() error = %v, want an insufficient capacity refusal", err)
	}
	if errors.Is(err, ErrStuckLaunchResource) {
		t.Error("a refusal was reported as a stuck resource, want the two kept apart")
	}
	if len(api.diskDeletes) == 0 {
		t.Error("disk deletes = none, want a refused launch to clean up after itself")
	}
}

func TestCreateCleansDisksAfterTheParentIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	api := &launchAPI{instanceCreateError: "Internal", cancelOnInstanceCreate: cancel}
	p := NewProvider(api.server(t), "project", nil, WithDeadlines(shortDeadlines()))

	if _, err := p.Create(ctx, launchParams(), testOptions()); err == nil {
		t.Fatal("Create() succeeded, want the canceled launch to fail")
	}
	if len(api.diskDeletes) == 0 {
		t.Fatal("disk cleanup was skipped after the parent context was canceled")
	}
	if api.cleanupContextCanceled {
		t.Fatal("a cleanup request used an already-canceled context")
	}
}

func TestCreateCleansAnInstanceAfterTheParentIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	api := &launchAPI{startError: "Internal", cancelOnStart: cancel}
	p := NewProvider(api.server(t), "project", nil, WithDeadlines(shortDeadlines()))

	if _, err := p.Create(ctx, launchParams(), testOptions()); err == nil {
		t.Fatal("Create() succeeded, want the canceled launch to fail")
	}
	if api.stops == 0 || api.instanceDeletes == 0 {
		t.Fatalf(
			"cleanup made %d stop and %d delete requests, want both after cancellation",
			api.stops,
			api.instanceDeletes,
		)
	}
	if len(api.diskDeletes) == 0 {
		t.Fatal("instance cleanup left the launch disk behind")
	}
	if api.cleanupContextCanceled {
		t.Fatal("a cleanup request used an already-canceled context")
	}
}

func TestCleanupPreservesValuesAndUsesItsOwnDeadline(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "value"))
	cancel()
	deadlines := shortDeadlines()
	deadlines.Cleanup = 20 * time.Millisecond
	p := NewProvider(nil, "project", nil, WithDeadlines(deadlines))

	started := time.Now()
	p.cleanup(parent, func(ctx context.Context) {
		if got := ctx.Value(key{}); got != "value" {
			t.Errorf("cleanup context value = %v, want value", got)
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("cleanup context started canceled: %v", err)
		}
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Errorf("cleanup context ended with %v, want its own deadline", ctx.Err())
		}
	})
	if elapsed := time.Since(started); elapsed < deadlines.Cleanup {
		t.Errorf("cleanup stopped after %s, before its %s deadline", elapsed, deadlines.Cleanup)
	}
}

func TestCreateAbandonsAnInstanceStuckInCreating(t *testing.T) {
	api := &launchAPI{createdRunState: oxide.InstanceStateCreating}
	p := NewProvider(api.server(t), "project", nil, WithDeadlines(shortDeadlines()))

	_, err := p.Create(context.Background(), launchParams(), testOptions())

	var stuck *StuckLaunchResourceError
	if !errors.As(err, &stuck) {
		t.Fatalf("Create() error = %v, want a StuckLaunchResourceError", err)
	}
	if stuck.Kind != KindInstance {
		t.Errorf("stuck kind = %q, want %q", stuck.Kind, KindInstance)
	}
	if stuck.State != string(oxide.InstanceStateCreating) {
		t.Errorf("stuck state = %q, want %q", stuck.State, oxide.InstanceStateCreating)
	}

	if api.starts != 0 {
		t.Errorf("starts = %d, want the instance never started", api.starts)
	}
}

func TestAdoptWaitsForACreatingInstanceToSettle(t *testing.T) {
	api := &launchAPI{}
	client := api.server(t)
	p := NewProvider(client, "project", nil, WithDeadlines(shortDeadlines()))

	api.instance = runningInstance()
	api.instance.RunState = oxide.InstanceStateCreating
	api.instanceCreateError = "ObjectAlreadyExists"

	_, err := p.Create(context.Background(), launchParams(), testOptions())

	var stuck *StuckLaunchResourceError
	if !errors.As(err, &stuck) {
		t.Fatalf("Create() error = %v, want the adoption to report a stuck instance", err)
	}
	if api.starts != 0 {
		t.Errorf("starts = %d, want an unsettled instance never started", api.starts)
	}
}

func TestDeadlinesValidate(t *testing.T) {
	if err := DefaultDeadlines().Validate(); err != nil {
		t.Fatalf("DefaultDeadlines().Validate() = %v, want the defaults to be usable", err)
	}

	zero := DefaultDeadlines()
	zero.DiskDeleting = 0
	if err := zero.Validate(); err == nil {
		t.Error("Validate() accepted a zero deadline, want it refused")
	}

	inverted := DefaultDeadlines()
	inverted.InstanceCreating = inverted.Launch + time.Minute
	if err := inverted.Validate(); err == nil {
		t.Error("Validate() accepted a per-state deadline longer than the launch, want it refused")
	}
}

func TestCreateRemovesItsOwnDisksWhenItAdoptsAnotherAttemptsInstance(t *testing.T) {
	api := &launchAPI{}
	p := NewProvider(api.server(t), "project", nil, WithDeadlines(shortDeadlines()))

	api.instance = runningInstance()
	api.instanceCreateError = "ObjectAlreadyExists"

	adopted, err := p.Create(context.Background(), launchParams(), testOptions())
	if err != nil {
		t.Fatalf("Create() error = %v, want the running instance adopted", err)
	}
	if adopted == nil {
		t.Fatal("Create() returned no instance")
	}
	if len(api.diskDeletes) != 1 || api.diskDeletes[0] != "d15c0000-0000-4000-8000-000000000001" {
		t.Errorf(
			"disk deletes = %v, want the disks this attempt created removed as unused",
			api.diskDeletes,
		)
	}
}

func TestCreateKeepsItsDisksWhenItRecoversItsOwnInstance(t *testing.T) {
	api := &launchAPI{}
	p := NewProvider(api.server(t), "project", nil, WithDeadlines(shortDeadlines()))

	api.instance = runningInstance()
	api.instanceCreateTimeout = true

	recovered, err := p.Create(context.Background(), launchParams(), testOptions())
	if err != nil {
		t.Fatalf("Create() error = %v, want the launch's own instance recovered", err)
	}
	if recovered == nil {
		t.Fatal("Create() returned no instance")
	}
	if len(api.diskDeletes) != 0 {
		t.Errorf(
			"disk deletes = %v, want the disks of the instance being returned left alone",
			api.diskDeletes,
		)
	}
}

func TestAmbiguousRecognizesTheSDKsLostAnswer(t *testing.T) {
	live := context.Background()

	for _, err := range []error{
		fmt.Errorf("error sending request: Post \"http://rack/v1/instances\": EOF"),
		fmt.Errorf(
			"error sending request: Post \"http://rack/v1/instances\": context deadline exceeded",
		),
		context.DeadlineExceeded,
		io.ErrUnexpectedEOF,
	} {
		if !ambiguous(live, err) {
			t.Errorf("ambiguous(%v) = false, want a lost answer resolved by looking it up", err)
		}
	}

	if ambiguous(live, oxide.ErrObjectNotFound) {
		t.Error("an API error was treated as ambiguous, want it taken at its word")
	}

	done, cancel := context.WithCancel(context.Background())
	cancel()
	if ambiguous(done, io.EOF) {
		t.Error("a lost answer was treated as ambiguous after the launch context ended")
	}
}
