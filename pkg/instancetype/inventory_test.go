package instancetype

import (
	"context"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
)

// inventoryFreshFor is the window the fake rack readings are built with. Tests either stay well
// inside it or step well past it.
const inventoryFreshFor = 10 * time.Minute

// fakeInventory is a rack reading a test can replace, standing in for the sled provider.
type fakeInventory struct {
	snapshot capacity.Snapshot
}

func (f *fakeInventory) Capacity() capacity.Snapshot {
	return f.snapshot
}

// read builds a reading of a rack of identically sized sleds with the given free resources on each,
// taken at takenAt. Generation is bumped by the caller through successive calls.
func read(takenAt time.Time, generation uint64, sleds ...capacity.Sled) capacity.Snapshot {
	return capacity.NewSnapshot(sleds, takenAt, inventoryFreshFor, generation)
}

// sledWithFree is a provisionable sled with exactly this much room left on it.
func sledWithFree(cpus, memGiB int) capacity.Sled {
	return capacity.Sled{
		ID:            "sled",
		Capacity:      capacity.Resources{CPUs: cpus, Memory: uint64(memGiB) * GiB},
		Provisionable: true,
	}
}

func newInventoryManager(now *time.Time, inventory *fakeInventory) *CapacityManager {
	c := NewCapacityManager(inventory)
	c.now = func() time.Time { return *now }
	c.jitter = func(time.Duration) time.Duration { return 0 }
	return c
}

// The behaviour that a fixed retry interval gets wrong. A shape no sled has room for is not worth
// offering at all, and no amount of waiting changes that, so it stays out of consideration past the
// point where a backoff would have reopened it.
func TestInventoryRulesOutShapesIndefinitely(t *testing.T) {
	now := time.Now()
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(8, 16))}
	c := newInventoryManager(&now, inventory)

	// No refusal has ever been recorded. The rack alone is enough.
	if unavailable, reason := c.IsUnavailable(Specs{Ncpus: 16, MemInGiB: 32}); !unavailable ||
		reason != ReasonCapacity {
		t.Errorf("IsUnavailable = (%v, %q), want (true, %q)", unavailable, reason, ReasonCapacity)
	}
	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 8, MemInGiB: 16}); unavailable {
		t.Error("a shape that fits exactly was ruled out")
	}

	// Well past every backoff this breaker would ever set, and still ruled out. The reading is kept
	// fresh, which is what makes the claim keep holding.
	for range 20 {
		now = now.Add(inventoryFreshFor / 2)
		inventory.snapshot = read(now, inventory.snapshot.Generation()+1, sledWithFree(8, 16))
		if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 16, MemInGiB: 32}); !unavailable {
			t.Fatalf("the shape became available again while the rack still had no room for it")
		}
	}
}

// The rack gaining room for a shape it had none for is the signal to try it now rather than serve out
// a wait that was set when the rack looked different.
func TestInventoryGainMakesABackedOffShapeHalfOpen(t *testing.T) {
	now := time.Now()
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(4, 8))}
	c := newInventoryManager(&now, inventory)
	ctx := context.Background()

	// Refused repeatedly, so it is sitting on a long wait.
	for range 4 {
		c.RecordICE(ctx, specs, ReasonCapacity)
		now = now.Add(retryAfterOf(t, c, specs).Sub(now))
		inventory.snapshot = read(now, inventory.snapshot.Generation()+1, sledWithFree(4, 8))
	}
	c.RecordICE(ctx, specs, ReasonCapacity)
	if got := retryAfterOf(t, c, specs).Sub(now); got != 30*time.Minute {
		t.Fatalf("backoff = %v, want the capped 30m to be shortened by the rack", got)
	}

	now = now.Add(time.Minute)
	inventory.snapshot = read(now, 99, sledWithFree(16, 32))

	if state := stateOf(t, c, specs); state != StateHalfOpen {
		t.Errorf("state = %q, want %q once the rack had room", state, StateHalfOpen)
	}
	if unavailable, _ := c.IsUnavailable(specs); unavailable {
		t.Error("the shape is still ruled out after the rack made room for it")
	}
	if got := ices(t, c, specs); got != 5 {
		t.Errorf("consecutive ICEs = %d, want the history kept across the reopen", got)
	}
	// One probe, as with every other way a shape reopens.
	if !c.TryAcquire(specs) {
		t.Fatal("the reopened shape could not be probed")
	}
	if c.TryAcquire(specs) {
		t.Error("the rack gaining room opened the shape to the whole burst")
	}
}

// A rack that changed somewhere irrelevant is not evidence about this shape. Reopening on any change
// at all would hand every backed off shape a probe on every refresh and quietly undo the backoff.
func TestUnrelatedInventoryChangesDoNotReopen(t *testing.T) {
	now := time.Now()
	specs := Specs{Ncpus: 32, MemInGiB: 64}
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(8, 16))}
	c := newInventoryManager(&now, inventory)

	c.RecordICE(context.Background(), specs, ReasonCapacity)

	// Room appeared, but nowhere near enough for this shape.
	now = now.Add(time.Minute)
	inventory.snapshot = read(now, 2, sledWithFree(16, 32))

	if state := stateOf(t, c, specs); state != StateBackoff {
		t.Errorf("state = %q, want the shape still backed off", state)
	}
}

// A shape that was already feasible and was refused anyway was refused for something the reading
// cannot see, so more of the same room is not new evidence and must not cut the backoff short.
func TestMoreRoomForAnAlreadyFeasibleShapeDoesNotReopen(t *testing.T) {
	now := time.Now()
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(16, 32))}
	c := newInventoryManager(&now, inventory)

	c.RecordICE(context.Background(), specs, ReasonCapacity)

	now = now.Add(time.Minute)
	inventory.snapshot = read(now, 2, sledWithFree(64, 128))

	if state := stateOf(t, c, specs); state != StateBackoff {
		t.Errorf("state = %q, want the backoff to survive room the shape already had", state)
	}
}

// The first reading has nothing to be compared against, so it must not read as the rack having gained
// anything. Otherwise every backed off shape is reopened the moment inventory comes up.
func TestTheFirstReadingReopensNothing(t *testing.T) {
	now := time.Now()
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	inventory := &fakeInventory{}
	c := newInventoryManager(&now, inventory)

	c.RecordICE(context.Background(), specs, ReasonCapacity)

	now = now.Add(time.Minute)
	inventory.snapshot = read(now, 1, sledWithFree(64, 128))

	if state := stateOf(t, c, specs); state != StateBackoff {
		t.Errorf("state = %q, want the backoff to survive the first reading of the rack", state)
	}
}

// The most important failure mode to get right. An inventory controller that has stopped refreshing,
// because its token lost fleet access or the system API is down, must not hold provisioning at a
// standstill on the strength of a reading from an hour ago.
func TestStaleInventoryStopsRulingShapesOut(t *testing.T) {
	now := time.Now()
	specs := Specs{Ncpus: 64, MemInGiB: 128}
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(8, 16))}
	c := newInventoryManager(&now, inventory)

	if unavailable, _ := c.IsUnavailable(specs); !unavailable {
		t.Fatal("a shape the rack has no room for was offered while the reading was fresh")
	}

	// Refreshes have stopped. The reading is unchanged and simply ages out.
	now = now.Add(inventoryFreshFor + time.Second)

	if unavailable, _ := c.IsUnavailable(specs); unavailable {
		t.Error("a stale reading is still ruling shapes out, so provisioning is frozen")
	}
	// And the breaker is back in charge on its own terms.
	c.RecordICE(context.Background(), specs, ReasonCapacity)
	if unavailable, reason := c.IsUnavailable(specs); !unavailable || reason != ReasonCapacity {
		t.Errorf("IsUnavailable = (%v, %q), want the refusal history to rule again",
			unavailable, reason)
	}
	now = now.Add(3 * time.Minute)
	if unavailable, _ := c.IsUnavailable(specs); unavailable {
		t.Error("the shape did not reopen on its backoff, so nothing will ever probe it")
	}
}

// Offerings are only rebuilt when the generation moves, so both a new reading and a reading going
// stale have to move it. A stale reading that did not would leave shapes suppressed in a list nobody
// rebuilds.
func TestInventoryChangesMoveTheGeneration(t *testing.T) {
	now := time.Now()
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(8, 16))}
	c := newInventoryManager(&now, inventory)

	first := c.Generation()
	if got := c.Generation(); got != first {
		t.Fatalf("generation = %d, want it settled at %d with nothing changing", got, first)
	}

	now = now.Add(time.Minute)
	inventory.snapshot = read(now, 2, sledWithFree(16, 32))
	afterRead := c.Generation()
	if afterRead == first {
		t.Error("the generation did not move when the rack was read again")
	}

	now = now.Add(inventoryFreshFor + time.Second)
	if got := c.Generation(); got == afterRead {
		t.Error("the generation did not move when the reading went stale")
	}
}

// With inventory switched off nothing about the breaker changes, which is what makes inventory
// something an operator can decline to grant fleet access for.
func TestNoInventoryLeavesEveryShapeAvailable(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)

	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 256, MemInGiB: 512}); unavailable {
		t.Error("a shape was ruled out with no inventory to rule it out")
	}
	if got := c.Generation(); got != c.Generation() {
		t.Error("the generation is moving with no inventory to move it")
	}
}

// The rack has to reach the offerings the scheduler actually reads, not just the breaker's own view.
func TestInventoryConstrainsTheOfferedInstanceTypes(t *testing.T) {
	now := time.Now()
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(8, 16))}
	p := NewProvider(nil, inventory)
	p.capacity = newInventoryManager(&now, inventory)

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"8-16", true},
		{"4-8", true},
		{"16-32", false},
		{"8-64", false},
	} {
		it, ok := p.Get(tc.name)
		if !ok {
			t.Fatalf("instance type %s does not exist", tc.name)
		}
		if got := it.Offerings[0].Available; got != tc.want {
			t.Errorf("instance type %s available = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A rack that has grown has to show up in the same list.
	now = now.Add(time.Minute)
	inventory.snapshot = read(now, 2, sledWithFree(32, 64))

	if it, _ := p.Get("16-32"); !it.Offerings[0].Available {
		t.Error("16-32 is still unavailable after the rack made room for it")
	}
}
