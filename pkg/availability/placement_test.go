package availability

import (
	"context"
	"testing"
	"time"
)

func placementManagerAt(now *time.Time) *Manager {
	c := New(nil)
	c.now = func() time.Time { return *now }
	c.jitter = func(time.Duration) time.Duration { return 0 }
	return c
}

// The placement scope under test; the helpers below share it so their
// signatures stay about the shape being asked for.
const testPlacement = Placement("Affinity:a")

func gatedPlacement(
	t *testing.T,
	c *Manager,
	now *time.Time,
	specs Shape,
) time.Duration {
	t.Helper()
	first, gated := c.RecordPlacementRefusal(
		context.Background(), testPlacement, specs, "refused",
	)
	if gated {
		t.Fatal("the first refusal gated the placement")
	}
	*now = now.Add(first)
	window, gated := c.RecordPlacementRefusal(
		context.Background(), testPlacement, specs, "refused again",
	)
	if !gated {
		t.Fatal("the second consecutive refusal did not gate the placement")
	}
	return window
}

func TestPlacementIsCanonical(t *testing.T) {
	constraints := []Constraint{
		{ID: "b", Kind: "AntiAffinity"},
		{ID: "a", Kind: "Affinity"},
		{ID: "a", Kind: "Affinity"},
	}
	want := Placement("Affinity:a,AntiAffinity:b")
	if got := NewPlacement(constraints...); got != want {
		t.Fatalf("NewPlacement() = %q, want %q", got, want)
	}

	constraints[0], constraints[1] = constraints[1], constraints[0]
	if got := NewPlacement(constraints...); got != want {
		t.Errorf("reordered NewPlacement() = %q, want %q", got, want)
	}
}

func TestEmptyPlacementHasNoScope(t *testing.T) {
	if got := NewPlacement(); got != "" {
		t.Errorf("NewPlacement() = %q, want no placement scope", got)
	}
}

func TestPlacementRefusalBacksOffLargerShapesInTheSameScope(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	c := placementManagerAt(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}

	window := gatedPlacement(t, c, &now, specs)
	if window != time.Minute {
		t.Fatalf("second backoff = %s, want 1m", window)
	}
	if !placementUnavailable(c, testPlacement, specs) {
		t.Error("the refused placement and shape are available during their backoff")
	}
	if !placementUnavailable(c, testPlacement, Shape{Ncpus: 16, MemInGiB: 32}) {
		t.Error("a larger shape was not covered by the placement refusal")
	}
	if placementUnavailable(c, testPlacement, Shape{Ncpus: 4, MemInGiB: 8}) {
		t.Error("a smaller shape was covered by a larger placement refusal")
	}
	if placementUnavailable(c, "Affinity:b", specs) {
		t.Error("an unrelated placement was covered by another placement's refusal")
	}
	if unavailable, _ := unavailableFor(c, specs); unavailable {
		t.Error("a placement refusal leaked into global shape availability")
	}
}

func TestPlacementRefusalUsesGraceWindow(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	c := placementManagerAt(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}

	first, gated := c.RecordPlacementRefusal(t.Context(), testPlacement, specs, "refused")
	if first != 30*time.Second || gated {
		t.Fatalf("first refusal = (%s, %v), want (30s, false)", first, gated)
	}
	for range 5 {
		if _, gated := c.RecordPlacementRefusal(t.Context(), testPlacement, specs, "burst"); gated {
			t.Fatal("a burst inside the grace window gated the placement")
		}
	}
	if placementUnavailable(c, testPlacement, specs) {
		t.Error("the first-refusal grace window made the placement unavailable")
	}
}

func TestLargerPlacementRefusalInheritsCoveredHistory(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	c := placementManagerAt(&now)
	small := Shape{Ncpus: 8, MemInGiB: 16}
	large := Shape{Ncpus: 16, MemInGiB: 32}

	window, gated := c.RecordPlacementRefusal(t.Context(), testPlacement, small, "small refused")
	if gated {
		t.Fatal("the first refusal gated the smaller shape")
	}
	now = now.Add(window)
	window, gated = c.RecordPlacementRefusal(t.Context(), testPlacement, large, "large refused")
	if !gated || window != time.Minute {
		t.Fatalf("covered refusal = (%s, %v), want (1m, true)", window, gated)
	}
}

func TestPlacementSuccessClearsOnlyExactEntry(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	c := placementManagerAt(&now)
	refused := Shape{Ncpus: 8, MemInGiB: 16}
	other := Shape{Ncpus: 16, MemInGiB: 32}

	gatedPlacement(t, c, &now, other)
	gatedPlacement(t, c, &now, refused)
	c.RecordPlacementSuccess(t.Context(), testPlacement, refused)

	if placementUnavailable(c, testPlacement, refused) {
		t.Error("a successful exact placement remained unavailable")
	}
	if !placementUnavailable(c, testPlacement, other) {
		t.Error("success on one shape cleared another shape's placement refusal")
	}
}

func TestLargerPlacementSuccessClearsCoveredRefusals(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	c := placementManagerAt(&now)
	small := Shape{Ncpus: 8, MemInGiB: 16}
	large := Shape{Ncpus: 16, MemInGiB: 32}

	gatedPlacement(t, c, &now, large)
	gatedPlacement(t, c, &now, small)
	c.RecordPlacementSuccess(t.Context(), testPlacement, large)

	if placementUnavailable(c, testPlacement, small) {
		t.Error("a successful larger shape did not clear the smaller refusal")
	}
	if placementUnavailable(c, testPlacement, large) {
		t.Error("a successful larger shape did not clear its refusal")
	}
}

func TestPlacementBackoffAllowsOneHalfOpenProbe(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	c := placementManagerAt(&now)
	refused := Shape{Ncpus: 8, MemInGiB: 16}
	probe := Shape{Ncpus: 16, MemInGiB: 32}

	window := gatedPlacement(t, c, &now, refused)
	if acquirePlacement(c, probe) {
		t.Fatal("a probe was acquired during the placement backoff")
	}
	now = now.Add(window)
	if !acquirePlacement(c, probe) {
		t.Fatal("the first half-open placement probe was rejected")
	}
	if acquirePlacement(c, probe) {
		t.Fatal("a second half-open placement probe was acquired concurrently")
	}
	releasePlacement(probe)
	if !acquirePlacement(c, probe) {
		t.Fatal("the placement probe could not be reacquired after release")
	}
}

func TestPlacementHistoryAgesOut(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	c := placementManagerAt(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}

	gatedPlacement(t, c, &now, specs)
	now = now.Add(placementHistoryTTL + time.Minute)
	window, gated := c.RecordPlacementRefusal(t.Context(), testPlacement, specs, "new history")
	if window != placementBaseBackoff || gated {
		t.Errorf("refusal after history expiry = (%s, %v), want (%s, false)",
			window, gated, placementBaseBackoff)
	}
}
