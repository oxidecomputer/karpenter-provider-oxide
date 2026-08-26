package capacity

import (
	"testing"
	"time"
)

const giB = 1 << 30

func sled(id string, cpus int, memGiB uint64, usedCPUs int, usedMemGiB uint64) Sled {
	return Sled{
		ID:            id,
		Capacity:      Resources{CPUs: cpus, Memory: memGiB * giB},
		Used:          Resources{CPUs: usedCPUs, Memory: usedMemGiB * giB},
		Provisionable: true,
	}
}

func snapshotOf(sleds ...Sled) Snapshot {
	return NewSnapshot(sleds, time.Now(), time.Hour, 1)
}

func request(cpus int, memGiB uint64) Resources {
	return Resources{CPUs: cpus, Memory: memGiB * giB}
}

func TestFragmentationIsNotHiddenByRackTotals(t *testing.T) {
	rack := snapshotOf(
		sled("a", 8, 64, 4, 32),
		sled("b", 8, 64, 4, 32),
	)

	if rack.CanFit(request(8, 16)) {
		t.Error("an 8 cpu instance was judged placeable across two sleds with 4 free cpus each")
	}
	if !rack.CanFit(request(4, 16)) {
		t.Error("a 4 cpu instance does not fit on a sled with exactly 4 free cpus")
	}
}

func TestFeasibleCountIsTheTighterOfTheTwoResources(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rack    Snapshot
		request Resources
		want    int
	}{
		{
			name:    "cpu bound",
			rack:    snapshotOf(sled("a", 16, 256, 0, 0)),
			request: request(4, 8),
			want:    4,
		},
		{
			name:    "memory bound",
			rack:    snapshotOf(sled("a", 64, 32, 0, 0)),
			request: request(4, 8),
			want:    4,
		},
		{
			name:    "summed across sleds",
			rack:    snapshotOf(sled("a", 8, 16, 0, 0), sled("b", 16, 32, 8, 16)),
			request: request(4, 8),
			want:    4,
		},
		{
			name:    "nothing fits",
			rack:    snapshotOf(sled("a", 8, 16, 8, 16)),
			request: request(1, 2),
			want:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rack.FeasibleCount(tc.request); got != tc.want {
				t.Errorf("FeasibleCount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSledsThatWillNotAcceptInstancesDoNotCount(t *testing.T) {
	draining := sled("a", 64, 256, 0, 0)
	draining.Provisionable = false
	rack := snapshotOf(draining)

	if rack.CanFit(request(1, 2)) {
		t.Error("an empty but non-provisionable sled was counted as room")
	}
	if got := rack.FeasibleCount(request(1, 2)); got != 0 {
		t.Errorf("FeasibleCount = %d, want 0 on a sled that accepts nothing", got)
	}
}

func TestOversubscribedSledReportsNoFreeSpace(t *testing.T) {
	free := sled("a", 8, 16, 12, 32).Free()

	if free.CPUs != 0 || free.Memory != 0 {
		t.Errorf("Free() = %+v, want zero in both dimensions", free)
	}
}

func TestFreshnessBoundsWhatAReadingMayClaim(t *testing.T) {
	takenAt := time.Now()
	rack := NewSnapshot([]Sled{sled("a", 8, 16, 0, 0)}, takenAt, 10*time.Minute, 1)

	if !rack.Known() {
		t.Error("a snapshot of a real reading reports itself as unknown")
	}
	if !rack.Fresh(takenAt.Add(9 * time.Minute)) {
		t.Error("a reading inside its freshness window is not fresh")
	}
	if rack.Fresh(takenAt.Add(11 * time.Minute)) {
		t.Error("a reading past its freshness window is still fresh")
	}
}

func TestZeroSnapshotIsUnknownRatherThanEmpty(t *testing.T) {
	var rack Snapshot

	if rack.Known() {
		t.Error("the zero snapshot reports itself as a reading")
	}
	if rack.Fresh(time.Now()) {
		t.Error("the zero snapshot reports itself as fresh, which would rule out every shape")
	}
}
