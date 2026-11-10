package availability

import (
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
)

func TestDecisionExplainsQuotaAndPreservesTheObservation(t *testing.T) {
	now := time.Now()
	m := newTestManager(&now)
	shape := Shape{Ncpus: 8, MemInGiB: 16}
	m.RecordICE(t.Context(), shape, Unpinned, ReasonCapacity)
	quota := utilization.Snapshot{
		Known:   true,
		TakenAt: now.Add(-time.Minute),
		Stale:   true,
		Quota: utilization.Quota{
			CapacityCPUs:    4,
			CapacityMemory:  64 * GiB,
			CapacityStorage: 100 * GiB,
		},
	}
	reading := m.Snapshot().WithQuota(quota)
	decision := reading.Evaluate(Request{Shape: shape})
	if decision.Available || decision.Source != SourceQuota || decision.Reason != ReasonQuota ||
		decision.Resource != utilization.ResourceCPUs || decision.Scope != RefusalScopeGlobal ||
		!decision.Stale || !decision.ObservedAt.Equal(quota.TakenAt) || decision.Message == "" {
		t.Fatalf("quota decision lost its evidence: %+v", decision)
	}
	m.RecordSuccess(t.Context(), shape, Unpinned)
	quota.Quota.CapacityCPUs = 64
	if got := reading.Evaluate(Request{Shape: shape}); got != decision {
		t.Fatalf("a previously captured observation changed: %+v, want %+v", got, decision)
	}
	if !m.Snapshot().WithQuota(quota).Evaluate(Request{Shape: shape}).Available {
		t.Fatal("the new observation did not see recovery")
	}
}

func TestStorageQuotaDoesNotSuppressShapesThatNeedNoNewStorage(t *testing.T) {
	reading := New(nil).Snapshot().WithQuota(utilization.Snapshot{Known: true,
		Quota: utilization.Quota{CapacityCPUs: 64, CapacityMemory: 256 * GiB}})
	request := Request{Shape: Shape{Ncpus: 4, MemInGiB: 8}}
	if !reading.Evaluate(request).Available {
		t.Fatal("an exhausted storage quota suppressed the compute offering")
	}
	request.Storage = GiB
	got := reading.Evaluate(request)
	if got.Available || got.Source != SourceQuota || got.Resource != utilization.ResourceStorage {
		t.Fatalf("storage request was not diagnosed: %+v", got)
	}
}

func TestPlacementObservationSeparatesHistoryFromGating(t *testing.T) {
	now := time.Now()
	m := newTestManager(&now)
	request := Request{Shape: Shape{Ncpus: 8, MemInGiB: 16}, Placement: "aa:group"}
	m.RecordPlacementRefusal(
		t.Context(),
		request.Placement,
		request.Shape,
		"cannot satisfy placement",
	)
	first := m.Snapshot()
	if len(first.Placements()) != 1 || first.Placements()[0].Gated ||
		!first.Evaluate(request).Available {
		t.Fatal("the first placement refusal must be visible without gating launches")
	}
	now = first.Placements()[0].RetryAfter.Add(time.Second)
	m.RecordPlacementRefusal(
		t.Context(),
		request.Placement,
		request.Shape,
		"cannot satisfy placement",
	)
	second := m.Snapshot()
	got := second.Evaluate(request)
	if got.Available || got.Scope != RefusalScopePlacement || got.Source != SourceRefusal ||
		got.Reason != ReasonAffinityPolicy || got.RetryAfter.IsZero() || got.Message == "" {
		t.Fatalf("placement refusal lost its evidence: %+v", got)
	}
	request.Placement = "aa:other"
	if !second.Evaluate(request).Available {
		t.Fatal("a placement refusal suppressed an unrelated NodeClass")
	}
	if first.Placements()[0].Gated {
		t.Fatal("recording another refusal mutated the previous observation")
	}
	now = got.RetryAfter.Add(time.Second)
	request.Placement = "aa:group"
	if !m.Snapshot().Evaluate(request).Available {
		t.Fatal("an elapsed placement backoff did not permit a probe")
	}
}

func TestPlacementSnapshotDistinguishesHistoryBackoffAndAReservedProbe(t *testing.T) {
	now := time.Now()
	m := newTestManager(&now)
	request := Request{
		Shape:     Shape{Ncpus: 8, MemInGiB: 16},
		Placement: NewPlacement(Constraint{Kind: "AntiAffinity", ID: "group"}),
	}
	m.RecordPlacementRefusal(t.Context(), request.Placement, request.Shape, "denied")
	first := m.Snapshot().Placements()[0]
	if first.HasBackoff() {
		t.Fatal("the first refusal was classified as a backoff")
	}
	now = first.RetryAfter.Add(time.Second)
	m.RecordPlacementRefusal(t.Context(), request.Placement, request.Shape, "denied again")
	backoff := m.Snapshot().Placements()[0]
	if !backoff.HasBackoff() || !backoff.Gated {
		t.Fatal("the second refusal was not a backoff")
	}
	now = backoff.RetryAfter.Add(time.Second)
	eligible := m.Snapshot().Placements()[0]
	if !eligible.HasBackoff() || eligible.Gated || eligible.ProbeInFlight {
		t.Fatal("expired backoff was not probe eligible")
	}
	probe, ok := m.Acquire(request)
	if !ok {
		t.Fatal("probe was not admitted")
	}
	defer probe.Release()
	if !m.Snapshot().Placements()[0].ProbeInFlight {
		t.Fatal("snapshot hid the reserved probe")
	}
	if eligible.ProbeInFlight {
		t.Fatal("probe acquisition mutated a previous observation")
	}
	probe.Release()
	if m.Snapshot().Placements()[0].ProbeInFlight {
		t.Fatal("released probe remains in flight")
	}
}
