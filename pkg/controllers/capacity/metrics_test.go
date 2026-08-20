package capacity

import (
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A shape that is no longer unavailable has no value to write, so its series has to go. Leaving the
// last one behind would report a countdown that finished long ago as though it were still running,
// which is worse than reporting nothing.
func TestPublishMetricsClearsShapesThatCameBack(t *testing.T) {
	c, _ := newController(t, nil)
	c.instanceTypes.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 8, MemInGiB: 16},
		instancetype.ReasonCapacity,
	)

	rack, inventory := c.instanceTypes.Rack()
	publishMetrics(c.render(rack, inventory), rack, inventory)

	if got := testutil.CollectAndCount(metrics.BackoffSeconds); got != 1 {
		t.Fatalf("%d backoff series published, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.UnavailableShapes); got != 1 {
		t.Fatalf("unavailable shapes = %v, want 1", got)
	}

	// The shape launched successfully, so it is not unavailable anymore.
	c.instanceTypes.RecordSuccess(t.Context(), instancetype.Specs{Ncpus: 8, MemInGiB: 16})
	rack, inventory = c.instanceTypes.Rack()
	publishMetrics(c.render(rack, inventory), rack, inventory)

	if got := testutil.CollectAndCount(metrics.BackoffSeconds); got != 0 {
		t.Errorf("%d backoff series still published for a shape that came back", got)
	}
	if got := testutil.ToFloat64(metrics.UnavailableShapes); got != 0 {
		t.Errorf("unavailable shapes = %v, want 0", got)
	}
}

// The rack numbers only mean anything while the reading is being acted on, and freshness is the gauge
// an operator alerts on: a reading that stops being refreshed stops constraining anything.
func TestPublishMetricsReportsInventoryFreshness(t *testing.T) {
	fresh := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 8, 16, true)},
		time.Now().Add(-30*time.Second),
		time.Hour,
		1,
	)}
	c, _ := newController(t, fresh)

	rack, inventory := c.instanceTypes.Rack()
	publishMetrics(c.render(rack, inventory), rack, inventory)

	if got := testutil.ToFloat64(metrics.SnapshotFresh); got != 1 {
		t.Errorf("snapshot fresh = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.SnapshotAgeSeconds); got < 25 || got > 60 {
		t.Errorf("snapshot age = %v seconds, want roughly 30", got)
	}
	if got := testutil.CollectAndCount(metrics.SledFreeCPUs); got != 1 {
		t.Errorf("%d sled series published, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.LargestFeasibleCPUs); got != 8 {
		t.Errorf("largest feasible cpus = %v, want 8", got)
	}

	// Nothing has refreshed it since, so it ages out and stops being believed.
	fresh.snapshot = capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 8, 16, true)},
		time.Now().Add(-2*time.Hour),
		time.Hour,
		2,
	)
	rack, inventory = c.instanceTypes.Rack()
	publishMetrics(c.render(rack, inventory), rack, inventory)

	if got := testutil.ToFloat64(metrics.SnapshotFresh); got != 0 {
		t.Errorf("snapshot fresh = %v, want 0 for a reading nobody is believing", got)
	}
}

// The rack disagreeing with a refusal is the interesting case, so the feasible count is published
// alongside the shapes that are out rather than for every shape the provider offers.
func TestPublishMetricsReportsFeasibilityForUnavailableShapes(t *testing.T) {
	inventory := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 16, 32, true)},
		time.Now(),
		time.Hour,
		1,
	)}
	c, _ := newController(t, inventory)
	c.instanceTypes.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 8, MemInGiB: 16},
		instancetype.ReasonCapacity,
	)

	rack, state := c.instanceTypes.Rack()
	if state != instancetype.InventoryFresh {
		t.Fatalf("inventory state = %q, want the reading to be fresh", state)
	}
	publishMetrics(c.render(rack, state), rack, state)

	// Two 8-16 instances fit in a sled with 16 CPUs and 32 GiB free, which says the refusal came from
	// something the reading cannot see.
	series := testutil.ToFloat64(metrics.FeasibleInstances.WithLabelValues("8-16"))
	if series != 2 {
		t.Errorf("feasible instances of 8-16 = %v, want 2", series)
	}
	if got := testutil.CollectAndCount(metrics.FeasibleInstances); got != 1 {
		t.Errorf("%d feasibility series published, want only the unavailable shape", got)
	}
}

// The view exists to be looked at, so the labels on it have to stay bounded no matter what the provider
// is doing. Nothing here may be labeled per shape unless the shape is actually out.
func TestPublishMetricsStaysBoundedWithNothingWrong(t *testing.T) {
	c, _ := newController(t, nil)

	rack, inventory := c.instanceTypes.Rack()
	publishMetrics(c.render(rack, inventory), rack, inventory)

	if got := testutil.CollectAndCount(metrics.BackoffSeconds); got != 0 {
		t.Errorf("%d backoff series with nothing refused", got)
	}
	if got := testutil.CollectAndCount(metrics.ConsecutiveICEs); got != 0 {
		t.Errorf("%d refusal-count series with nothing refused", got)
	}
	if got := testutil.CollectAndCount(metrics.FeasibleInstances); got != 0 {
		t.Errorf("%d feasibility series with no inventory and nothing refused", got)
	}
	if got := testutil.CollectAndCount(metrics.SledFreeCPUs); got != 0 {
		t.Errorf("%d sled series with no inventory", got)
	}
}
