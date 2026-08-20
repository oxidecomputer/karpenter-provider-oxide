package affinitypolicy

import (
	"context"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
)

// trackerAt builds a tracker on a clock the test drives, with jitter switched off so waits can be
// compared exactly.
func trackerAt(now *time.Time) *Tracker {
	t := NewTracker()
	t.now = func() time.Time { return *now }
	t.jitter = func(time.Duration) time.Duration { return 0 }
	return t
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

// The wait doubles with each window that opens with a refusal, and caps.
func TestRecordRefusalBacksOffExponentially(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	for i, want := range []time.Duration{
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		5 * time.Minute, // capped
		5 * time.Minute,
	} {
		if got := tracker.RecordRefusal(ctx, "default", "refused"); got != want {
			t.Errorf("refusal %d backed off %s, want %s", i+1, got, want)
		}
		// Past the window, so the next refusal opens a new one.
		now = now.Add(want + time.Second)
	}
}

// Karpenter provisions in bursts, so one unsatisfiable policy produces several refusals within a
// moment of each other. Counting each would jump straight to the cap on the first incident.
func TestRecordRefusalDoesNotLengthenAnOpenWindow(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	first := tracker.RecordRefusal(ctx, "default", "refused")
	if first != 30*time.Second {
		t.Fatalf("first backoff = %s, want 30s", first)
	}
	now = now.Add(time.Second)
	for range 5 {
		if got := tracker.RecordRefusal(ctx, "default", "refused"); got > first {
			t.Errorf("a refusal inside the window lengthened the wait to %s", got)
		}
	}

	// The window elapses and the next refusal is the second, not the seventh.
	now = now.Add(first)
	if got := tracker.RecordRefusal(ctx, "default", "refused"); got != time.Minute {
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

	backoff := tracker.RecordRefusal(ctx, "default", "the rack could not spread it")
	state, backingOff := tracker.State("default")
	if !backingOff {
		t.Fatal("a nodeclass just refused is not backing off")
	}
	if state.Message != "the rack could not spread it" {
		t.Errorf("message = %q, want the diagnosis", state.Message)
	}
	if got := state.Remaining(now); got != backoff {
		t.Errorf("remaining = %s, want %s", got, backoff)
	}

	// The wait elapses: Ready again, so one launch probes the policy.
	now = now.Add(backoff)
	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("still backing off after the wait elapsed")
	}
	// The history survives, which is what makes the next wait longer rather than resetting to base.
	if got := tracker.RecordRefusal(ctx, "default", "refused"); got != time.Minute {
		t.Errorf("backoff after an elapsed wait = %s, want 1m: history should survive", got)
	}
}

// A launch that placed is the only proof the policy is satisfiable, and it contradicts everything
// recorded before it -- including the count that would have made the next wait longer.
func TestRecordSuccessClearsTheHistory(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	tracker.RecordRefusal(ctx, "default", "refused")
	tracker.RecordSuccess(ctx, "default")

	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("still backing off after a launch succeeded")
	}
	if got := tracker.RecordRefusal(ctx, "default", "refused"); got != baseBackoff {
		t.Errorf("backoff after a success = %s, want the base wait", got)
	}
}

func TestHistoryAgesOut(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	tracker.RecordRefusal(ctx, "default", "refused")
	now = now.Add(historyTTL + time.Minute)

	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("a refusal from beyond the history TTL is still backing off")
	}
	if got := tracker.RecordRefusal(ctx, "default", "refused"); got != baseBackoff {
		t.Errorf("backoff after the history aged out = %s, want the base wait", got)
	}
}

// The whole point of keying by nodeclass: one nodeclass's unsatisfiable policy says nothing about
// another's, and must not hold it out of provisioning.
func TestBackoffIsScopedToTheNodeclass(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tracker := trackerAt(&now)
	ctx := context.Background()

	tracker.RecordRefusal(ctx, "spread", "refused")

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

	tracker.RecordRefusal(context.Background(), "default", "refused")
	tracker.Forget("default")

	if _, backingOff := tracker.State("default"); backingOff {
		t.Error("still backing off after being forgotten")
	}
}
