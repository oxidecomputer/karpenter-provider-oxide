package instancetype

import (
	"context"
	"slices"
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

func newInventoryManager(now *time.Time, inventory *fakeInventory) *CapacityManager {
	c := NewCapacityManager(inventory)
	c.now = func() time.Time { return *now }
	c.jitter = func(time.Duration) time.Duration { return 0 }
	return c
}

func TestInventoryRulesOutShapesIndefinitely(t *testing.T) {
	now := time.Now()
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(8, 16))}
	c := newInventoryManager(&now, inventory)

	if unavailable, reason := c.IsUnavailable(Specs{Ncpus: 16, MemInGiB: 32}); !unavailable ||
		reason != ReasonCapacity {
		t.Errorf("IsUnavailable = (%v, %q), want (true, %q)", unavailable, reason, ReasonCapacity)
	}
	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 8, MemInGiB: 16}); unavailable {
		t.Error("a shape that fits exactly was ruled out")
	}

	for range 20 {
		now = now.Add(inventoryFreshFor / 2)
		inventory.snapshot = read(now, inventory.snapshot.Generation()+1, sledWithFree(8, 16))
		if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 16, MemInGiB: 32}); !unavailable {
			t.Fatalf("the shape became available again while the rack still had no room for it")
		}
	}
}

func TestInventoryGainMakesABackedOffShapeHalfOpen(t *testing.T) {
	now := time.Now()
	specs := Specs{Ncpus: 8, MemInGiB: 16}
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
	if unavailable, _ := c.IsUnavailable(specs); unavailable {
		t.Error("the shape is still ruled out after the rack made room for it")
	}
	if got := ices(t, c, specs); got != 5 {
		t.Errorf("consecutive ICEs = %d, want the history kept across the reopen", got)
	}

	if !c.TryAcquire(specs) {
		t.Fatal("the reopened shape could not be probed")
	}
	if c.TryAcquire(specs) {
		t.Error("the rack gaining room opened the shape to the whole burst")
	}
}

func TestUnrelatedInventoryChangesDoNotReopen(t *testing.T) {
	now := time.Now()
	specs := Specs{Ncpus: 32, MemInGiB: 64}
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

func TestStaleInventoryStopsRulingShapesOut(t *testing.T) {
	now := time.Now()
	specs := Specs{Ncpus: 64, MemInGiB: 128}
	inventory := &fakeInventory{snapshot: read(now, 1, sledWithFree(8, 16))}
	c := newInventoryManager(&now, inventory)

	if unavailable, _ := c.IsUnavailable(specs); !unavailable {
		t.Fatal("a shape the rack has no room for was offered while the reading was fresh")
	}

	now = now.Add(inventoryFreshFor + time.Second)

	if unavailable, _ := c.IsUnavailable(specs); unavailable {
		t.Error("a stale reading is still ruling shapes out, so provisioning is frozen")
	}

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
	c := newTestCapacityManager(&now)

	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 256, MemInGiB: 512}); unavailable {
		t.Error("a shape was ruled out with no inventory to rule it out")
	}
	if got := c.Generation(); got != c.Generation() {
		t.Error("the generation is moving with no inventory to move it")
	}
}

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
		{"16-30", false},
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

	now = now.Add(time.Minute)
	inventory.snapshot = read(now, 2, sledWithFree(32, 64))

	if it, _ := p.Get("16-30"); !it.Offerings[0].Available {
		t.Error("16-30 is still unavailable after the rack made room for it")
	}
}

func TestLargestShapeThatFits(t *testing.T) {
	for _, tc := range []struct {
		name string
		free capacity.Resources
		want string
	}{
		{
			name: "bounded by memory",
			free: capacity.Resources{CPUs: 64, Memory: 16 * GiB},
			want: "31-16",
		},
		{
			name: "bounded by cpus",
			free: capacity.Resources{CPUs: 8, Memory: 512 * GiB},
			want: "8-470",
		},
		{
			name: "nothing fits",
			free: capacity.Resources{CPUs: 0, Memory: 0},
			want: "",
		},
		{
			name: "smaller than the smallest shape",
			free: capacity.Resources{CPUs: 1, Memory: 1 * GiB},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shape, ok := LargestShapeThatFits(tc.free)
			if !ok {
				if tc.want != "" {
					t.Fatalf("no shape fits, want %s", tc.want)
				}
				return
			}
			got := SpecsToInstanceTypeName(shape.Ncpus, shape.MemInGiB)
			if got != tc.want {
				t.Errorf("largest shape = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRackHeadroomMatchesWhatTheRackRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sleds []capacity.Sled
		want  []Specs
	}{
		{

			name:  "one sled",
			sleds: []capacity.Sled{sledWithFree(8, 16)},
			want:  []Specs{{Ncpus: 8, MemInGiB: 16}},
		},
		{

			name:  "sleds that bound different dimensions",
			sleds: []capacity.Sled{sledWithFree(64, 32), sledWithFree(4, 512)},
			want:  []Specs{{Ncpus: 4, MemInGiB: 512}, {Ncpus: 64, MemInGiB: 32}},
		},
		{

			name:  "a sled covered by another",
			sleds: []capacity.Sled{sledWithFree(8, 16), sledWithFree(4, 8)},
			want:  []Specs{{Ncpus: 8, MemInGiB: 16}},
		},
		{

			name:  "a rack larger than the catalog",
			sleds: []capacity.Sled{sledWithFree(maxSledCPUs, maxSledMemGiB)},
			want:  nil,
		},
		{

			name: "nothing provisionable",
			sleds: []capacity.Sled{func() capacity.Sled {
				drained := sledWithFree(64, 128)
				drained.Provisionable = false
				return drained
			}()},
			want: []Specs{{}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rack := read(time.Now(), 1, tc.sleds...)

			got := RackHeadroom(rack)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("headroom = %v, want %v", got, tc.want)
			}

			for _, specs := range shapes() {
				out := len(got) > 0
				for _, headroom := range got {
					if specs.Ncpus <= headroom.Ncpus && specs.MemInGiB <= headroom.MemInGiB {
						out = false
						break
					}
				}
				if fits := rack.CanFit(resourcesFor(specs)); fits == out {
					t.Fatalf("%s fits on the rack = %t, and the headroom says it is out = %t",
						SpecsToInstanceTypeName(specs.Ncpus, specs.MemInGiB), fits, out)
				}
			}
		})
	}
}

func TestLargestShapeOnRackIsPerSled(t *testing.T) {
	drained := sledWithFree(64, 128)
	drained.Provisionable = false
	rack := read(time.Now(), 1, sledWithFree(4, 8), sledWithFree(8, 16), drained)

	shape, ok := LargestShapeOnRack(rack)
	if !ok {
		t.Fatal("no shape fits on a rack with two provisionable sleds")
	}
	if got := SpecsToInstanceTypeName(shape.Ncpus, shape.MemInGiB); got != "8-16" {
		t.Errorf("largest shape on the rack = %s, want 8-16", got)
	}

	empty := read(time.Now(), 1, drained)
	if _, ok := LargestShapeOnRack(empty); ok {
		t.Error("a rack of sleds accepting nothing reported a feasible shape")
	}
}
