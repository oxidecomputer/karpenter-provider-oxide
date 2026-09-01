package storagerefusal

import (
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
)

func TestConsecutiveRefusalsAccumulate(t *testing.T) {
	tracker := NewTracker()

	tracker.Record(t.Context(), availability.ReasonCapacity, "first")
	tracker.Record(t.Context(), availability.ReasonQuota, "second")

	refusal, ok := tracker.Last()
	if !ok {
		t.Fatal("no refusal is being reported after two were recorded")
	}
	if refusal.Consecutive != 2 {
		t.Errorf("consecutive = %d, want 2", refusal.Consecutive)
	}
	if refusal.Reason != availability.ReasonQuota {
		t.Errorf("reason = %q, want the most recent diagnosis %q",
			refusal.Reason, availability.ReasonQuota)
	}
	if refusal.Message != "second" {
		t.Errorf("message = %q, want %q", refusal.Message, "second")
	}
}

func TestClearForgetsTheRun(t *testing.T) {
	tracker := NewTracker()
	tracker.Record(t.Context(), availability.ReasonCapacity, "refused")

	tracker.Clear(t.Context())
	if _, ok := tracker.Last(); ok {
		t.Fatal("a refusal is still reported after a launch provisioned a boot disk")
	}

	tracker.Record(t.Context(), availability.ReasonCapacity, "refused again")
	refusal, _ := tracker.Last()
	if refusal.Consecutive != 1 {
		t.Errorf("consecutive = %d, want the run to restart at 1", refusal.Consecutive)
	}
}

func TestARefusalAgesOut(t *testing.T) {
	now := time.Now()
	tracker := NewTracker()
	tracker.now = func() time.Time { return now }

	tracker.Record(t.Context(), availability.ReasonCapacity, "stale")

	now = now.Add(historyTTL + time.Minute)
	if _, ok := tracker.Last(); ok {
		t.Fatal("a refusal older than the history window is still being reported")
	}
}

func TestNothingToReportBeforeAnyRefusal(t *testing.T) {
	if _, ok := NewTracker().Last(); ok {
		t.Error("a refusal is reported before any launch was refused")
	}
}
