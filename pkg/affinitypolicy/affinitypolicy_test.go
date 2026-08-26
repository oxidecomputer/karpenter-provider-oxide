package affinitypolicy

import (
	"context"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
)

func trackerAt(now *time.Time) *Tracker {
	t := NewTracker()
	t.now = func() time.Time { return *now }
	t.jitter = func(time.Duration) time.Duration { return 0 }
	return t
}

func gate(
	t *testing.T,
	tracker *Tracker,
	now *time.Time,
	nodeClass string,
	message string,
) time.Duration {
	t.Helper()
	first, gated := tracker.RecordRefusal(context.Background(), nodeClass, message)
	if gated {
		t.Fatalf("the first refusal gated %s, want a grace window", nodeClass)
	}
	*now = now.Add(first)
	window, gated := tracker.RecordRefusal(context.Background(), nodeClass, message)
	if !gated {
		t.Fatalf("the second consecutive refusal did not gate %s", nodeClass)
	}
	return window
}

func TestAttributable(t *testing.T) {
	tests := []struct {
		name   string
		groups []v1alpha1.OxideAffinityGroup
		want   bool
	}{
		{
			name: "a nodeclass with no groups cannot have been refused by one",
		},
		{
			name:   "an allow policy places the instance anyway, so it never refuses a launch",
			groups: []v1alpha1.OxideAffinityGroup{{ID: "a", Policy: "allow"}},
			want:   false,
		},
		{
			name:   "a fail policy is the one that refuses",
			groups: []v1alpha1.OxideAffinityGroup{{ID: "a", Policy: "fail"}},
			want:   true,
		},
		{
			name: "one fail policy among several is enough",
			groups: []v1alpha1.OxideAffinityGroup{
				{ID: "a", Policy: "allow"},
				{ID: "b", Policy: "fail"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Attributable(tt.groups); got != tt.want {
				t.Errorf("Attributable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecordRefusalBacksOffExponentially(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	for i, want := range []time.Duration{
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		5 * time.Minute,
		5 * time.Minute,
	} {
		got, gated := tracker.RecordRefusal(ctx, "default", "refused")
		if got != want {
			t.Errorf("refusal %d opened a %s window, want %s", i+1, got, want)
		}

		if wantGated := i+1 >= gateThreshold; gated != wantGated {
			t.Errorf("refusal %d gated = %v, want %v", i+1, gated, wantGated)
		}

		now = now.Add(want + time.Second)
	}
}

func TestRecordRefusalDoesNotLengthenAnOpenWindow(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	first, _ := tracker.RecordRefusal(ctx, "default", "refused")
	if first != 30*time.Second {
		t.Fatalf("first backoff = %s, want 30s", first)
	}
	now = now.Add(time.Second)
	for range 5 {
		if got, _ := tracker.RecordRefusal(ctx, "default", "refused"); got > first {
			t.Errorf("a refusal inside the window lengthened the wait to %s", got)
		}
	}

	now = now.Add(first)
	if got, _ := tracker.RecordRefusal(ctx, "default", "refused"); got != time.Minute {
		t.Errorf("backoff after the window = %s, want 1m", got)
	}
}

func TestStateReportsTheBackoffOnlyWhileItRuns(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("a nodeclass with no history is backing off")
	}

	backoff := gate(t, tracker, &now, "default", "the rack could not spread it")
	state, backingOff := tracker.State("default")
	if !backingOff {
		t.Fatal("a nodeclass refused twice running is not backing off")
	}
	if state.Message != "the rack could not spread it" {
		t.Errorf("message = %q, want the diagnosis", state.Message)
	}
	if got := state.Remaining(now); got != backoff {
		t.Errorf("remaining = %s, want %s", got, backoff)
	}

	now = now.Add(backoff)
	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("still backing off after the wait elapsed")
	}

	if got, _ := tracker.RecordRefusal(ctx, "default", "refused"); got != 2*time.Minute {
		t.Errorf("backoff after an elapsed wait = %s, want 2m: history should survive", got)
	}
}

func TestRecordSuccessClearsTheHistory(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	gate(t, tracker, &now, "default", "refused")
	tracker.RecordSuccess(ctx, "default")

	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("still backing off after a launch succeeded")
	}
	if got, gated := tracker.RecordRefusal(ctx, "default", "refused"); got != baseBackoff || gated {
		t.Errorf("after a success: backoff %s gated %v, want the base wait and no gate", got, gated)
	}
}

func TestHistoryAgesOut(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	gate(t, tracker, &now, "default", "refused")
	now = now.Add(historyTTL + time.Minute)

	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("a refusal from beyond the history TTL is still backing off")
	}
	if got, gated := tracker.RecordRefusal(ctx, "default", "refused"); got != baseBackoff || gated {
		t.Errorf(
			"after ageing out: backoff %s gated %v, want the base wait and no gate",
			got,
			gated,
		)
	}
}

func TestBackoffIsScopedToTheNodeclass(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)

	gate(t, tracker, &now, "spread", "refused")

	if _, backingOff := tracker.State("spread"); !backingOff {
		t.Error("the refused nodeclass is not backing off")
	}
	if _, backingOff := tracker.State("other"); backingOff {
		t.Error("an unrelated nodeclass was backed off by another's refusal")
	}
}

func TestForgetDropsTheHistory(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)

	gate(t, tracker, &now, "default", "refused")
	tracker.Forget("default")

	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("still backing off after being forgotten")
	}
}

func TestOneRefusalDoesNotGate(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)

	window, gated := tracker.RecordRefusal(context.Background(), "default", "refused")
	if gated {
		t.Error("the first refusal held the nodeclass out of provisioning")
	}
	if window != baseBackoff {
		t.Errorf("first window = %s, want the base wait", window)
	}
	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("State reports a back-off after a single refusal")
	}
}

func TestARacingRefusalNeverGates(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	if _, gated := tracker.RecordRefusal(ctx, "default", "refused"); gated {
		t.Fatal("the first refusal gated")
	}

	tracker.RecordSuccess(ctx, "default")

	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("backing off after the refusal turned out to be a race")
	}

	if _, gated := tracker.RecordRefusal(ctx, "default", "refused"); gated {
		t.Error("a later first refusal gated, so the race was counted against it")
	}
}

func TestABurstDoesNotSatisfyTheThreshold(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	for range 10 {
		if _, gated := tracker.RecordRefusal(ctx, "default", "refused"); gated {
			t.Fatal("a burst of simultaneous refusals gated the nodeclass")
		}
		now = now.Add(time.Second)
	}
	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("State reports a back-off from a single burst")
	}
}
