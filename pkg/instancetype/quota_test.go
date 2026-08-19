package instancetype

import (
	"context"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
)

// fakeQuotas is a QuotaProvider whose answer and generation are set by the test.
type fakeQuotas struct {
	quota utilization.Quota
	err   error
	gen   uint64
	// refreshes counts the re-reads asked for, so a test can assert that a refusal triggered one.
	refreshes int
}

func (f *fakeQuotas) Quota() (utilization.Quota, error) { return f.quota, f.err }
func (f *fakeQuotas) Generation() uint64                { return f.gen }

func (f *fakeQuotas) Refresh(context.Context, bool) (bool, error) {
	f.refreshes++
	return true, nil
}

// A refused launch is evidence the snapshot overstated the headroom, so the provider has to be able to
// ask for a re-read, and must tolerate having no quota provider at all.
func TestRefreshQuota(t *testing.T) {
	quotas := &fakeQuotas{quota: smallQuota()}
	NewProvider(quotas, nil).RefreshQuota(context.Background())

	if quotas.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", quotas.refreshes)
	}

	// Must not panic with quota awareness switched off.
	NewProvider(nil, nil).RefreshQuota(context.Background())
}

// A silo entitled to 16 vCPUs and 64 GiB, with nothing provisioned yet.
func smallQuota() utilization.Quota {
	return utilization.Quota{
		CapacityCPUs:    16,
		CapacityMemory:  64 * GiB,
		CapacityStorage: 1024 * GiB,
	}
}

// The point of reading the quota at all: without it the scheduler is offered shapes up to a whole
// sled, and a silo entitled to 16 vCPUs would have Karpenter pick a 256 vCPU node and fail.
func TestQuotaMarksOversizedOfferingsUnavailable(t *testing.T) {
	p := NewProvider(&fakeQuotas{quota: smallQuota()}, nil)

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"8-16", true},   // inside both dimensions
		{"16-32", true},  // exactly at the CPU limit
		{"17-34", false}, // one vCPU over
		{"8-64", true},   // exactly at the memory limit
		{"8-128", false}, // over on memory alone
		{"64-128", false},
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

// A quota that cannot be read must constrain nothing. Failing closed would turn a permissions problem
// or an API blip into a cluster that cannot scale.
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

			it, ok := p.Get("256-512")
			if !ok {
				t.Fatal("instance type 256-512 does not exist")
			}
			if !it.Offerings[0].Available {
				t.Error("a shape was ruled out despite the quota being unknown")
			}
			if unavailable, _ := p.IsUnavailable(Specs{Ncpus: 256, MemInGiB: 512}); unavailable {
				t.Error("IsUnavailable ruled out a shape despite the quota being unknown")
			}
		})
	}
}

// Raising a quota has to take effect. The built list is cached, so a quota change that did not move
// the generation would leave the old offerings in place indefinitely.
func TestQuotaChangeRebuildsOfferings(t *testing.T) {
	quotas := &fakeQuotas{quota: smallQuota(), gen: 1}
	p := NewProvider(quotas, nil)

	if it, _ := p.Get("32-64"); it.Offerings[0].Available {
		t.Fatal("32-64 was available under a 16 vCPU quota")
	}

	quotas.quota = utilization.Quota{
		CapacityCPUs:    64,
		CapacityMemory:  256 * GiB,
		CapacityStorage: 1024 * GiB,
	}
	quotas.gen = 2

	if it, _ := p.Get("32-64"); !it.Offerings[0].Available {
		t.Error("32-64 is still unavailable after the quota was raised")
	}
}

// Both sources of unavailability have to keep working independently, and a refusal must still register
// while the quota has plenty of room.
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

// Quota wins when both reasons cover a shape. It is the more useful thing to report: rack capacity is
// transient and Karpenter retries into it, whereas a quota keeps refusing until somebody changes it.
func TestQuotaReasonWinsOverCapacity(t *testing.T) {
	c := NewCapacityManager(nil)
	ctx := context.Background()

	c.RecordICE(ctx, Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)
	c.RecordICE(ctx, Specs{Ncpus: 4, MemInGiB: 8}, ReasonQuota)

	if _, reason := c.IsUnavailable(Specs{Ncpus: 16, MemInGiB: 32}); reason != ReasonQuota {
		t.Errorf("reason = %q, want %q when both cover the shape", reason, ReasonQuota)
	}
}

// Recording a refusal ignores the reason when subsuming, because coverage is what an offering is filtered on. A
// smaller shape refused for quota genuinely does explain a larger one refused for capacity.
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

// Storage cannot be a property of an instance type: the boot disk size comes from the OxideNodeClass,
// so two nodes of identical shape can consume different storage. requestFor must claim zero rather
// than guess, leaving the storage check to launch time where the disk size is known.
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
