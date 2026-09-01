package instancetype

import (
	"context"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
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
	NewProvider(quotas, availability.New(nil)).RefreshQuota(context.Background())

	if quotas.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", quotas.refreshes)
	}

	NewProvider(nil, availability.New(nil)).RefreshQuota(context.Background())
}

func smallQuota() utilization.Quota {
	return utilization.Quota{
		CapacityCPUs:    16,
		CapacityMemory:  64 * GiB,
		CapacityStorage: 1024 * GiB,
	}
}

func TestQuotaMarksOversizedOfferingsUnavailable(t *testing.T) {
	p := NewProvider(&fakeQuotas{quota: smallQuota()}, availability.New(nil))

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
		it, ok := p.Get(tc.name, 0)
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
			p := NewProvider(tc.quotas, availability.New(nil))

			it, ok := p.Get("252-517", 0)
			if !ok {
				t.Fatal("instance type 252-517 does not exist")
			}
			if !it.Offerings[0].Available {
				t.Error("a shape was ruled out despite the quota being unknown")
			}
			if p.IsOverQuota(Specs{Ncpus: 252, MemInGiB: 517}) {
				t.Error("IsOverQuota ruled out a shape despite the quota being unknown")
			}
		})
	}
}

func TestQuotaChangeRebuildsOfferings(t *testing.T) {
	quotas := &fakeQuotas{quota: smallQuota(), gen: 1}
	p := NewProvider(quotas, availability.New(nil))

	if it, _ := p.Get("31-64", 0); it.Offerings[0].Available {
		t.Fatal("31-64 was available under a 16 vCPU quota")
	}

	quotas.quota = utilization.Quota{
		CapacityCPUs:    64,
		CapacityMemory:  256 * GiB,
		CapacityStorage: 1024 * GiB,
	}
	quotas.gen = 2

	if it, _ := p.Get("31-64", 0); !it.Offerings[0].Available {
		t.Error("31-64 is still unavailable after the quota was raised")
	}
}

func TestQuotaAndRefusalsCombine(t *testing.T) {
	manager := availability.New(nil)
	p := providerUsing(t, &fakeQuotas{quota: smallQuota()}, manager)

	manager.RecordICE(
		context.Background(),
		Specs{Ncpus: 4, MemInGiB: 8},
		availability.ReasonCapacity,
	)

	for _, tc := range []struct {
		name   string
		specs  Specs
		reason availability.Reason
	}{
		{"refused shape", Specs{Ncpus: 4, MemInGiB: 8}, availability.ReasonCapacity},
		{"larger than the refused shape", Specs{Ncpus: 8, MemInGiB: 16}, availability.ReasonCapacity},
		{"over quota only", Specs{Ncpus: 32, MemInGiB: 2}, availability.ReasonQuota},
		{"under both", Specs{Ncpus: 2, MemInGiB: 4}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := manager.Check(availability.Request{Shape: tc.specs})
			unavailable, reason := !decision.Available, decision.Reason
			if p.IsOverQuota(tc.specs) {
				unavailable, reason = true, availability.ReasonQuota
			}
			if unavailable != (tc.reason != "") {
				t.Errorf("unavailable = %v, want %v", unavailable, tc.reason != "")
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}
		})
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
