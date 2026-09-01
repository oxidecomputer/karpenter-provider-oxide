package capacity

import (
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPublishMetricsClearsShapesThatCameBack(t *testing.T) {
	c, _ := newController(t, nil)
	c.availability.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 8, MemInGiB: 16},
		availability.ReasonCapacity,
	)

	reading := c.availability.Snapshot()
	rack, inventory := reading.Rack()
	publishMetrics(c.render(reading), rack, inventory)

	if got := testutil.CollectAndCount(metrics.BackoffSeconds); got != 1 {
		t.Fatalf("%d backoff series published, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.UnavailableShapes); got != 1 {
		t.Fatalf("unavailable shapes = %v, want 1", got)
	}

	c.availability.RecordSuccess(t.Context(), instancetype.Specs{Ncpus: 8, MemInGiB: 16})
	reading = c.availability.Snapshot()
	rack, inventory = reading.Rack()
	publishMetrics(c.render(reading), rack, inventory)

	if got := testutil.CollectAndCount(metrics.BackoffSeconds); got != 0 {
		t.Errorf("%d backoff series still published for a shape that came back", got)
	}
	if got := testutil.ToFloat64(metrics.UnavailableShapes); got != 0 {
		t.Errorf("unavailable shapes = %v, want 0", got)
	}
}

func TestPublishMetricsReportsInventoryFreshness(t *testing.T) {
	fresh := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 8, 16, true)},
		time.Now().Add(-30*time.Second),
		time.Hour,
		1,
	)}
	c, _ := newController(t, fresh)

	reading := c.availability.Snapshot()
	rack, inventory := reading.Rack()
	publishMetrics(c.render(reading), rack, inventory)

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

	fresh.snapshot = capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 8, 16, true)},
		time.Now().Add(-2*time.Hour),
		time.Hour,
		2,
	)
	reading = c.availability.Snapshot()
	rack, inventory = reading.Rack()
	publishMetrics(c.render(reading), rack, inventory)

	if got := testutil.ToFloat64(metrics.SnapshotFresh); got != 0 {
		t.Errorf("snapshot fresh = %v, want 0 for a reading nobody is believing", got)
	}
}

func TestPublishMetricsReportsFeasibilityForUnavailableShapes(t *testing.T) {
	inventory := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 16, 32, true)},
		time.Now(),
		time.Hour,
		1,
	)}
	c, _ := newController(t, inventory)
	c.availability.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 8, MemInGiB: 16},
		availability.ReasonCapacity,
	)

	reading := c.availability.Snapshot()
	rack, state := reading.Rack()
	if state != availability.InventoryFresh {
		t.Fatalf("inventory state = %q, want the reading to be fresh", state)
	}
	publishMetrics(c.render(reading), rack, state)

	series := testutil.ToFloat64(metrics.FeasibleInstances.WithLabelValues("8-16"))
	if series != 2 {
		t.Errorf("feasible instances of 8-16 = %v, want 2", series)
	}
	if got := testutil.CollectAndCount(metrics.FeasibleInstances); got != 1 {
		t.Errorf("%d feasibility series published, want only the unavailable shape", got)
	}
}

func TestPublishMetricsStaysBoundedWithNothingWrong(t *testing.T) {
	c, _ := newController(t, nil)

	reading := c.availability.Snapshot()
	rack, inventory := reading.Rack()
	publishMetrics(c.render(reading), rack, inventory)

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
