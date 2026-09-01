package availability

import (
	"context"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
)

const inventoryFreshFor = 10 * time.Minute

type fakeInventory struct {
	snapshot capacity.Snapshot
}

func (f *fakeInventory) Capacity() capacity.Snapshot {
	return f.snapshot
}

func read(takenAt time.Time, generation uint64, sleds ...capacity.Sled) capacity.Snapshot {
	return capacity.NewSnapshot(sleds, takenAt, inventoryFreshFor, generation)
}

func sledWithFree(cpus, memGiB int) capacity.Sled {
	return capacity.Sled{
		ID:            "sled",
		Capacity:      capacity.Resources{CPUs: cpus, Memory: uint64(memGiB) * GiB},
		Provisionable: true,
	}
}

func newInventoryManager(now *time.Time, inventory *fakeInventory) *Manager {
	c := New(inventory)
	c.now = func() time.Time { return *now }
	c.jitter = func(time.Duration) time.Duration { return 0 }
	return c
}

func TestInventoryRulesOutShapesIndefinitely(t *testing.T) {
	now := time.Now()
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(8, 16))}
	c := newInventoryManager(&now, inventory)

	if unavailable, reason := unavailableFor(c, Shape{Ncpus: 16, MemInGiB: 32}); !unavailable ||
		reason != ReasonCapacity {
		t.Errorf("Check = (%v, %q), want (true, %q)", unavailable, reason, ReasonCapacity)
	}
	if unavailable, _ := unavailableFor(c, Shape{Ncpus: 8, MemInGiB: 16}); unavailable {
		t.Error("a shape that fits exactly was ruled out")
	}

	for range 20 {
		now = now.Add(inventoryFreshFor / 2)
		inventory.snapshot = read(now, inventory.snapshot.Generation()+1, sledWithFree(8, 16))
		if unavailable, _ := unavailableFor(c, Shape{Ncpus: 16, MemInGiB: 32}); !unavailable {
			t.Fatalf("the shape became available again while the rack still had no room for it")
		}
	}
}

func TestInventoryGainMakesABackedOffShapeHalfOpen(t *testing.T) {
	now := time.Now()
	specs := Shape{Ncpus: 8, MemInGiB: 16}
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(4, 8))}
	c := newInventoryManager(&now, inventory)
	ctx := context.Background()

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
	if unavailable, _ := unavailableFor(c, specs); unavailable {
		t.Error("the shape is still ruled out after the rack made room for it")
	}
	if got := ices(t, c, specs); got != 5 {
		t.Errorf("consecutive ICEs = %d, want the history kept across the reopen", got)
	}

	if !acquireShape(c, specs) {
		t.Fatal("the reopened shape could not be probed")
	}
	if acquireShape(c, specs) {
		t.Error("the rack gaining room opened the shape to the whole burst")
	}
}

func TestUnrelatedInventoryChangesDoNotReopen(t *testing.T) {
	now := time.Now()
	specs := Shape{Ncpus: 32, MemInGiB: 64}
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(8, 16))}
	c := newInventoryManager(&now, inventory)

	c.RecordICE(context.Background(), specs, ReasonCapacity)

	now = now.Add(time.Minute)
	inventory.snapshot = read(now, 2, sledWithFree(16, 32))

	if state := stateOf(t, c, specs); state != StateBackoff {
		t.Errorf("state = %q, want the shape still backed off", state)
	}
}

func TestMoreRoomForAnAlreadyFeasibleShapeDoesNotReopen(t *testing.T) {
	now := time.Now()
	specs := Shape{Ncpus: 8, MemInGiB: 16}
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(16, 32))}
	c := newInventoryManager(&now, inventory)

	c.RecordICE(context.Background(), specs, ReasonCapacity)

	now = now.Add(time.Minute)
	inventory.snapshot = read(now, 2, sledWithFree(64, 128))

	if state := stateOf(t, c, specs); state != StateBackoff {
		t.Errorf("state = %q, want the backoff to survive room the shape already had", state)
	}
}

func TestTheFirstReadingReopensNothing(t *testing.T) {
	now := time.Now()
	specs := Shape{Ncpus: 8, MemInGiB: 16}
	inventory := &fakeInventory{}
	c := newInventoryManager(&now, inventory)

	c.RecordICE(context.Background(), specs, ReasonCapacity)

	now = now.Add(time.Minute)
	inventory.snapshot = read(now, 1, sledWithFree(64, 128))

	if state := stateOf(t, c, specs); state != StateBackoff {
		t.Errorf("state = %q, want the backoff to survive the first reading of the rack", state)
	}
}

func TestStaleInventoryStopsRulingShapesOut(t *testing.T) {
	now := time.Now()
	specs := Shape{Ncpus: 64, MemInGiB: 128}
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(8, 16))}
	c := newInventoryManager(&now, inventory)

	if unavailable, _ := unavailableFor(c, specs); !unavailable {
		t.Fatal("a shape the rack has no room for was offered while the reading was fresh")
	}

	now = now.Add(inventoryFreshFor + time.Second)

	if unavailable, _ := unavailableFor(c, specs); unavailable {
		t.Error("a stale reading is still ruling shapes out, so provisioning is frozen")
	}

	c.RecordICE(context.Background(), specs, ReasonCapacity)
	if unavailable, reason := unavailableFor(c, specs); !unavailable || reason != ReasonCapacity {
		t.Errorf("Check = (%v, %q), want the refusal history to rule again",
			unavailable, reason)
	}
	now = now.Add(3 * time.Minute)
	if unavailable, _ := unavailableFor(c, specs); unavailable {
		t.Error("the shape did not reopen on its backoff, so nothing will ever probe it")
	}
}

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

func TestNoInventoryLeavesEveryShapeAvailable(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)

	if unavailable, _ := unavailableFor(c, Shape{Ncpus: 254, MemInGiB: 512}); unavailable {
		t.Error("a shape was ruled out with no inventory to rule it out")
	}
	if got := c.Generation(); got != c.Generation() {
		t.Error("the generation is moving with no inventory to move it")
	}
}
