package sled

import (
	"errors"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/oxide.go/oxide"
)

func TestPlacementForFlattensInServiceSled(t *testing.T) {
	threads := 128
	revision := 2
	got := placementFor(oxide.Sled{
		Id:     "sled-1",
		RackId: "rack-1",
		Baseboard: oxide.Baseboard{
			Part:     "913-0000019",
			Serial:   "BRM42220004",
			Revision: &revision,
		},
		State: oxide.SledStateActive,
		Policy: oxide.SledPolicy{
			Value: oxide.SledPolicyInService{ProvisionPolicy: "provisionable"},
		},
		UsableHardwareThreads: &threads,
		UsablePhysicalRam:     oxide.ByteCount(1099511627776),
	})

	want := Placement{
		SledID:                "sled-1",
		RackID:                "rack-1",
		Serial:                "BRM42220004",
		Part:                  "913-0000019",
		Revision:              2,
		State:                 "active",
		Policy:                "in_service",
		ProvisionPolicy:       "provisionable",
		UsableHardwareThreads: 128,
		UsablePhysicalRam:     1099511627776,
	}
	if got != want {
		t.Errorf("placementFor() = %+v, want %+v", got, want)
	}
}

func TestPlacementForExpungedSledHasNoProvisionPolicy(t *testing.T) {
	got := placementFor(oxide.Sled{
		Id:     "sled-1",
		State:  oxide.SledStateDecommissioned,
		Policy: oxide.SledPolicy{Value: oxide.SledPolicyExpunged{}},
	})

	if got.Policy != string(oxide.SledPolicyKindExpunged) {
		t.Errorf("Policy = %q, want %q", got.Policy, oxide.SledPolicyKindExpunged)
	}
	if got.ProvisionPolicy != "" {
		t.Errorf("ProvisionPolicy = %q, want it empty on an expunged sled", got.ProvisionPolicy)
	}
	if got.State != string(oxide.SledStateDecommissioned) {
		t.Errorf("State = %q, want %q", got.State, oxide.SledStateDecommissioned)
	}
}

func TestPlacementForUnwrapsPointerVariant(t *testing.T) {
	got := placementFor(oxide.Sled{
		Policy: oxide.SledPolicy{
			Value: &oxide.SledPolicyInService{ProvisionPolicy: "non_provisionable"},
		},
	})

	if got.ProvisionPolicy != "non_provisionable" {
		t.Errorf("ProvisionPolicy = %q, want %q", got.ProvisionPolicy, "non_provisionable")
	}
}

func TestPlacementUnhealthy(t *testing.T) {
	for _, tc := range []struct {
		name            string
		policy          string
		provisionPolicy string
		state           string
		wantUnhealthy   bool
		wantReason      UnhealthyReason
	}{
		{
			name: "an in-service provisionable sled is healthy", policy: "in_service",
			provisionPolicy: "provisionable", state: "active",
		},
		{
			name: "an expunged sled is unhealthy", policy: "expunged", state: "decommissioned",
			wantUnhealthy: true, wantReason: ReasonExpunged,
		},
		{
			name: "a sled closed to new placements is unhealthy", policy: "in_service",
			provisionPolicy: "non_provisionable", state: "active",
			wantUnhealthy: true, wantReason: ReasonNonProvisionable,
		},
		{

			name: "an expunged sled reports expunged over non-provisionable", policy: "expunged",
			provisionPolicy: "non_provisionable",
			wantUnhealthy:   true, wantReason: ReasonExpunged,
		},
		{

			name: "an unrecognised policy is healthy", policy: "some_future_policy",
			provisionPolicy: "some_future_provision_policy", state: "some_future_state",
		},
		{
			name: "a placement with nothing populated is healthy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Placement{
				Policy:          tc.policy,
				ProvisionPolicy: tc.provisionPolicy,
				State:           tc.state,
			}
			unhealthy, reason, message := p.Unhealthy()

			if unhealthy != tc.wantUnhealthy {
				t.Errorf("unhealthy = %v, want %v", unhealthy, tc.wantUnhealthy)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if tc.wantUnhealthy && message == "" {
				t.Error("an unhealthy placement returned no message")
			}
			if !tc.wantUnhealthy && message != "" {
				t.Errorf("a healthy placement returned message %q", message)
			}
		})
	}
}

func TestPlacementForReportsMissingSnapshotDistinctly(t *testing.T) {
	p := NewProvider(nil, "project", time.Minute)

	if _, _, err := p.PlacementFor("instance-1"); !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("PlacementFor before any refresh returned %v, want ErrNoSnapshot", err)
	}
	if _, err := p.Sleds(); !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("Sleds before any refresh returned %v, want ErrNoSnapshot", err)
	}
	if _, ok := p.SnapshotAge(time.Now()); ok {
		t.Error("SnapshotAge reported a snapshot before any refresh")
	}

	p.snap = &snapshot{placements: map[string]Placement{}, takenAt: time.Now()}
	placement, found, err := p.PlacementFor("instance-1")
	if err != nil {
		t.Errorf("PlacementFor with a snapshot returned %v, want no error", err)
	}
	if found {
		t.Errorf("PlacementFor reported %+v for an instance not in the snapshot", placement)
	}
}

func TestRefreshRateLimitsUnforcedCalls(t *testing.T) {
	p := NewProvider(nil, "project", time.Hour)
	p.lastAttempt = time.Now()

	refreshed, err := p.Refresh(t.Context(), false)
	if err != nil {
		t.Errorf("Refresh returned %v, want no error when skipped", err)
	}
	if refreshed {
		t.Error("Refresh reported that it refreshed despite being inside the rate limit floor")
	}
}

func TestRefreshIntervalsAreDerivedFromThePeriod(t *testing.T) {
	p := NewProvider(nil, "project", 8*time.Minute)

	if p.minRefreshInterval >= 8*time.Minute {
		t.Errorf("minRefreshInterval = %v, want it tighter than the period", p.minRefreshInterval)
	}
	if p.freshFor <= 8*time.Minute {
		t.Errorf("freshFor = %v, want it looser than the period", p.freshFor)
	}
}

func TestCapacityBeforeAnyRefreshIsUnknown(t *testing.T) {
	p := NewProvider(nil, "project", time.Minute)

	if p.Capacity().Known() {
		t.Error("Capacity reported a reading of the rack before one was taken")
	}
}

func TestCapacityOfDeratesMemoryButNotCPUs(t *testing.T) {
	const oneTiB = 1024 * (1 << 30)
	got := capacityOf(Placement{UsableHardwareThreads: 128, UsablePhysicalRam: oneTiB})

	if got.CPUs != 128 {
		t.Errorf("CPUs = %d, want the sled's hardware threads", got.CPUs)
	}
	if got.Memory >= oneTiB {
		t.Errorf("memory = %d, want less than the sled's physical RAM of %d", got.Memory, oneTiB)
	}
	if inGiB := got.Memory / (1 << 30); inGiB != 890 {
		t.Errorf("memory = %d GiB, want 87%% of a 1 TiB sled", inGiB)
	}
}

func TestCapacityOfNeverExceedsPhysicalRAM(t *testing.T) {
	for _, ram := range []uint64{0, 1, 99, 1 << 20, 512 * (1 << 30), 1024 * (1 << 30)} {
		if got := capacityOf(Placement{UsablePhysicalRam: ram}).Memory; got > ram {
			t.Errorf("a sled with %d bytes of RAM offers %d bytes of memory", ram, got)
		}
	}
}

func TestCapacityOfGrowsWithRAM(t *testing.T) {
	half := capacityOf(Placement{UsablePhysicalRam: 512 * (1 << 30)}).Memory
	whole := capacityOf(Placement{UsablePhysicalRam: 1024 * (1 << 30)}).Memory

	if half == 0 || whole <= half {
		t.Errorf("memory of %d and %d does not increase with physical RAM", half, whole)
	}
}

func TestProvisionableFollowsSledPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		placement Placement
		want      bool
	}{
		{
			name:      "in service and provisionable",
			placement: Placement{State: "active", Policy: "in_service", ProvisionPolicy: "provisionable"},
			want:      true,
		},
		{
			name:      "closed to new placements",
			placement: Placement{State: "active", Policy: "in_service", ProvisionPolicy: "non_provisionable"},
		},
		{
			name:      "expunged from the rack",
			placement: Placement{State: "decommissioned", Policy: "expunged"},
		},
		{
			name:      "not active",
			placement: Placement{State: "decommissioned", Policy: "in_service", ProvisionPolicy: "provisionable"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := provisionable(tc.placement); got != tc.want {
				t.Errorf("provisionable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHoldsCountsOnlyResidentInstances(t *testing.T) {
	cpus, memory := 8, 16*(1<<30)

	resident := holds(oxide.SledInstance{
		State:  oxide.InstanceStateRunning,
		Ncpus:  &cpus,
		Memory: &memory,
	})
	if resident.CPUs != 8 || resident.Memory != uint64(memory) {
		t.Errorf("holds(running) = %+v, want the instance's own size", resident)
	}

	for _, state := range []oxide.InstanceState{
		oxide.InstanceStateStopped,
		oxide.InstanceStateFailed,
		oxide.InstanceStateDestroyed,
	} {
		got := holds(oxide.SledInstance{State: state, Ncpus: &cpus, Memory: &memory})
		if got != (capacity.Resources{}) {
			t.Errorf("holds(%s) = %+v, want nothing held", state, got)
		}
	}

	sizeless := holds(oxide.SledInstance{State: oxide.InstanceStateRunning})
	if sizeless != (capacity.Resources{}) {
		t.Errorf("holds(sizeless) = %+v, want nothing held", sizeless)
	}
}
