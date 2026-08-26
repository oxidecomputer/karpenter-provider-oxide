package instancetype

import (
	"context"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
)

type fakeQuotas struct {
	quota utilization.Quota
	err   error
	gen   uint64

	refreshes int
}

func (f *fakeQuotas) Quota() (utilization.Quota, error) { return f.quota, f.err }
func (f *fakeQuotas) Generation() uint64                { return f.gen }

func (f *fakeQuotas) Refresh(context.Context, bool) (bool, error) {
	f.refreshes++
	return true, nil
}

func TestRefreshQuota(t *testing.T) {
	quotas := &fakeQuotas{quota: smallQuota()}
	NewProvider(quotas, nil).RefreshQuota(context.Background())

	if quotas.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", quotas.refreshes)
	}

	NewProvider(nil, nil).RefreshQuota(context.Background())
}

func smallQuota() utilization.Quota {
	return utilization.Quota{
		CapacityCPUs:    16,
		CapacityMemory:  64 * GiB,
		CapacityStorage: 1024 * GiB,
	}
}

func TestQuotaMarksOversizedOfferingsUnavailable(t *testing.T) {
	p := NewProvider(&fakeQuotas{quota: smallQuota()}, nil)

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"8-16", true},
		{"16-30", true},
		{"18-33", false},
		{"8-64", true},
		{"8-70", false},
		{"67-137", false},
	} {
		it, ok := p.Get(tc.name)
		if !ok {
			t.Fatalf("instance type %s does not exist", tc.name)
		}
		if got := it.Offerings[0].Available; got != tc.want {
			t.Errorf("instance type %s available = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestUnreadableQuotaConstrainsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		quotas QuotaProvider
	}{
		{"no quota provider at all", nil},
		{"a provider with no snapshot", &fakeQuotas{err: utilization.ErrNoSnapshot}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProvider(tc.quotas, nil)

			it, ok := p.Get("252-517")
			if !ok {
				t.Fatal("instance type 252-517 does not exist")
			}
			if !it.Offerings[0].Available {
				t.Error("a shape was ruled out despite the quota being unknown")
			}
			if unavailable, _ := p.IsUnavailable(Specs{Ncpus: 252, MemInGiB: 517}); unavailable {
				t.Error("IsUnavailable ruled out a shape despite the quota being unknown")
			}
		})
	}
}

func TestQuotaChangeRebuildsOfferings(t *testing.T) {
	quotas := &fakeQuotas{quota: smallQuota(), gen: 1}
	p := NewProvider(quotas, nil)

	if it, _ := p.Get("31-64"); it.Offerings[0].Available {
		t.Fatal("31-64 was available under a 16 vCPU quota")
	}

	quotas.quota = utilization.Quota{
		CapacityCPUs:    64,
		CapacityMemory:  256 * GiB,
		CapacityStorage: 1024 * GiB,
	}
	quotas.gen = 2

	if it, _ := p.Get("31-64"); !it.Offerings[0].Available {
		t.Error("31-64 is still unavailable after the quota was raised")
	}
}

func TestQuotaAndRefusalsCombine(t *testing.T) {
	p := NewProvider(&fakeQuotas{quota: smallQuota()}, nil)

	p.RecordICE(context.Background(), Specs{Ncpus: 4, MemInGiB: 8}, ReasonCapacity)

	for _, tc := range []struct {
		name   string
		specs  Specs
		reason Reason
	}{
		{"refused shape", Specs{Ncpus: 4, MemInGiB: 8}, ReasonCapacity},
		{"larger than the refused shape", Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity},
		{"over quota only", Specs{Ncpus: 32, MemInGiB: 2}, ReasonQuota},
		{"under both", Specs{Ncpus: 2, MemInGiB: 4}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unavailable, reason := p.IsUnavailable(tc.specs)
			if unavailable != (tc.reason != "") {
				t.Errorf("unavailable = %v, want %v", unavailable, tc.reason != "")
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

func TestQuotaReasonWinsOverCapacity(t *testing.T) {
	c := NewCapacityManager(nil)
	ctx := context.Background()

	c.RecordICE(ctx, Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)
	c.RecordICE(ctx, Specs{Ncpus: 4, MemInGiB: 8}, ReasonQuota)

	if _, reason := c.IsUnavailable(Specs{Ncpus: 16, MemInGiB: 32}); reason != ReasonQuota {
		t.Errorf("reason = %q, want %q when both cover the shape", reason, ReasonQuota)
	}
}

func TestRecordICESubsumesAcrossReasons(t *testing.T) {
	c := NewCapacityManager(nil)
	ctx := context.Background()

	c.RecordICE(ctx, Specs{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordICE(ctx, Specs{Ncpus: 8, MemInGiB: 16}, ReasonQuota)

	if got := len(c.Snapshot()); got != 1 {
		t.Fatalf("snapshot has %d entries, want the larger capacity entry subsumed", got)
	}
	unavailable, reason := c.IsUnavailable(Specs{Ncpus: 16, MemInGiB: 32})
	if !unavailable || reason != ReasonQuota {
		t.Errorf("IsUnavailable = (%v, %q), want (true, %q)", unavailable, reason, ReasonQuota)
	}
}

func TestRequestForClaimsNoStorage(t *testing.T) {
	got := requestFor(Specs{Ncpus: 8, MemInGiB: 16})

	if got.CPUs != 8 {
		t.Errorf("CPUs = %d, want 8", got.CPUs)
	}
	if got.Memory != 16*GiB {
		t.Errorf("Memory = %d, want %d", got.Memory, uint64(16*GiB))
	}
	if got.Storage != 0 {
		t.Errorf("Storage = %d, want 0", got.Storage)
	}
}
