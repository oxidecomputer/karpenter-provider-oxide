package availability

import (
	"context"
	"testing"
)

func TestQuotaReasonWinsOverCapacity(t *testing.T) {
	c := New(nil)
	ctx := context.Background()

	c.RecordICE(ctx, Shape{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)
	c.RecordICE(ctx, Shape{Ncpus: 4, MemInGiB: 8}, ReasonQuota)

	if _, reason := unavailableFor(c, Shape{Ncpus: 16, MemInGiB: 32}); reason != ReasonQuota {
		t.Errorf("reason = %q, want %q when both cover the shape", reason, ReasonQuota)
	}
}

func TestRecordICESubsumesAcrossReasons(t *testing.T) {
	c := New(nil)
	ctx := context.Background()

	c.RecordICE(ctx, Shape{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordICE(ctx, Shape{Ncpus: 8, MemInGiB: 16}, ReasonQuota)

	if got := len(c.Snapshot().Entries()); got != 1 {
		t.Fatalf("snapshot has %d entries, want the larger capacity entry subsumed", got)
	}
	unavailable, reason := unavailableFor(c, Shape{Ncpus: 16, MemInGiB: 32})
	if !unavailable || reason != ReasonQuota {
		t.Errorf("Check = (%v, %q), want (true, %q)", unavailable, reason, ReasonQuota)
	}
}
