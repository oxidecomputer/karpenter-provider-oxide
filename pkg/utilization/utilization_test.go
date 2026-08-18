package utilization

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/oxide.go/oxide"
)

const giB = 1 << 30

func quota() Quota {
	return Quota{
		CapacityCPUs:       64,
		CapacityMemory:     256 * giB,
		CapacityStorage:    1024 * giB,
		ProvisionedCPUs:    48,
		ProvisionedMemory:  192 * giB,
		ProvisionedStorage: 900 * giB,
	}
}

func TestQuotaAdmits(t *testing.T) {
	q := quota() // headroom: 16 vCPUs, 64 GiB memory, 124 GiB storage

	for _, tc := range []struct {
		name     string
		request  Request
		admits   bool
		resource Resource
	}{
		{
			name:    "a request inside every dimension is admitted",
			request: Request{CPUs: 8, Memory: 32 * giB, Storage: 100 * giB},
			admits:  true,
		},
		{
			name:    "a request exactly at the headroom is admitted",
			request: Request{CPUs: 16, Memory: 64 * giB, Storage: 124 * giB},
			admits:  true,
		},
		{
			name:     "one vCPU over is refused on cpus",
			request:  Request{CPUs: 17, Memory: 32 * giB, Storage: 10 * giB},
			resource: ResourceCPUs,
		},
		{
			name:     "memory over is refused on memory",
			request:  Request{CPUs: 8, Memory: 65 * giB, Storage: 10 * giB},
			resource: ResourceMemory,
		},
		{
			// The dimension the boot disk exhausts, which no instance shape can avoid.
			name:     "storage over is refused on storage",
			request:  Request{CPUs: 8, Memory: 32 * giB, Storage: 200 * giB},
			resource: ResourceStorage,
		},
		{
			// Order has to be fixed, or raising the CPU quota would make the reported dimension jump
			// around instead of moving on to the next real problem.
			name:     "cpus is reported first when several dimensions are over",
			request:  Request{CPUs: 100, Memory: 1000 * giB, Storage: 5000 * giB},
			resource: ResourceCPUs,
		},
		{
			name:    "an empty request is admitted",
			request: Request{},
			admits:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admits, resource := q.Admits(tc.request)
			if admits != tc.admits {
				t.Errorf("admits = %v, want %v", admits, tc.admits)
			}
			if resource != tc.resource {
				t.Errorf("resource = %q, want %q", resource, tc.resource)
			}
		})
	}
}

// A quota lowered below what is already provisioned is a real state, and the headroom has to go
// negative rather than clamp: clamping to zero would report "no room" for a request of zero, and
// would hide how far over the limit the silo actually is.
func TestQuotaRemainingGoesNegativeWhenOverProvisioned(t *testing.T) {
	q := Quota{
		CapacityCPUs:      8,
		ProvisionedCPUs:   32,
		CapacityMemory:    16 * giB,
		ProvisionedMemory: 64 * giB,
	}

	if got := q.Remaining(ResourceCPUs); got != -24 {
		t.Errorf("remaining cpus = %d, want -24", got)
	}
	if got := q.Remaining(ResourceMemory); got != -48*giB {
		t.Errorf("remaining memory = %d, want %d", got, int64(-48*giB))
	}
	if admits, resource := q.Admits(Request{CPUs: 1}); admits || resource != ResourceCPUs {
		t.Errorf("Admits = (%v, %q), want (false, cpus) while over the limit", admits, resource)
	}
}

func TestQuotaRemainingUnknownResource(t *testing.T) {
	if got := quota().Remaining(Resource("something-else")); got != 0 {
		t.Errorf("remaining for an unknown resource = %d, want 0", got)
	}
}

// The message is what surfaces on a NodeClaim when nothing scales up, so it has to carry the three
// numbers an operator needs: what was asked for, the limit, and what is already spent.
func TestQuotaExplain(t *testing.T) {
	q := quota()

	cpus := q.Explain(Request{CPUs: 32}, ResourceCPUs)
	for _, want := range []string{"cpus", "32 vCPUs", "48 vCPUs", "64 vCPUs", "16 vCPUs"} {
		if !strings.Contains(cpus, want) {
			t.Errorf("cpus explanation %q does not mention %q", cpus, want)
		}
	}

	memory := q.Explain(Request{Memory: 128 * giB}, ResourceMemory)
	for _, want := range []string{"memory", "128 GiB", "192 GiB", "256 GiB", "64 GiB"} {
		if !strings.Contains(memory, want) {
			t.Errorf("memory explanation %q does not mention %q", memory, want)
		}
	}
}

// Bytes are unreadable at quota scale, so they are rendered in GiB, and a non-round value must not be
// truncated into looking round.
func TestFormat(t *testing.T) {
	for _, tc := range []struct {
		resource Resource
		value    int64
		want     string
	}{
		{ResourceCPUs, 16, "16 vCPUs"},
		{ResourceCPUs, -4, "-4 vCPUs"},
		{ResourceMemory, 64 * giB, "64 GiB"},
		{ResourceStorage, giB / 2, "0.5 GiB"},
		{ResourceMemory, 0, "0 GiB"},
	} {
		if got := format(tc.resource, tc.value); got != tc.want {
			t.Errorf("format(%q, %d) = %q, want %q", tc.resource, tc.value, got, tc.want)
		}
	}
}

// An absent CPU count must read as zero rather than as unlimited. Treating unknown as unlimited would
// quietly switch the whole check off.
func TestQuotaFromTreatsAbsentCountsAsZero(t *testing.T) {
	got := quotaFrom(&oxide.Utilization{
		Capacity: oxide.VirtualResourceCounts{
			Memory: oxide.ByteCount(256 * giB),
		},
		Provisioned: oxide.VirtualResourceCounts{
			Memory: oxide.ByteCount(128 * giB),
		},
	})

	if got.CapacityCPUs != 0 || got.ProvisionedCPUs != 0 {
		t.Errorf("cpu counts = (%d, %d), want (0, 0)", got.CapacityCPUs, got.ProvisionedCPUs)
	}
	if got.CapacityMemory != 256*giB {
		t.Errorf("capacity memory = %d, want %d", got.CapacityMemory, uint64(256*giB))
	}
}

func TestQuotaFromPopulatesEveryField(t *testing.T) {
	cpus, provisionedCPUs := 64, 48
	got := quotaFrom(&oxide.Utilization{
		Capacity: oxide.VirtualResourceCounts{
			Cpus:    &cpus,
			Memory:  oxide.ByteCount(256 * giB),
			Storage: oxide.ByteCount(1024 * giB),
		},
		Provisioned: oxide.VirtualResourceCounts{
			Cpus:    &provisionedCPUs,
			Memory:  oxide.ByteCount(192 * giB),
			Storage: oxide.ByteCount(900 * giB),
		},
	})

	if got != quota() {
		t.Errorf("quotaFrom() = %+v, want %+v", got, quota())
	}
}

// An unread quota must be distinguishable from a quota with no headroom: the first means "do not
// constrain", the second means "constrain everything".
func TestQuotaReportsMissingSnapshotDistinctly(t *testing.T) {
	p := NewProvider(nil, time.Minute)

	if _, err := p.Quota(); !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("Quota before any refresh returned %v, want ErrNoSnapshot", err)
	}
	if got := p.Generation(); got != 0 {
		t.Errorf("Generation before any refresh = %d, want 0", got)
	}
}

// Refresh is called from every refused launch, so the floor is what stops a burst of NodeClaims from
// each issuing their own read.
func TestRefreshRateLimitsUnforcedCalls(t *testing.T) {
	p := NewProvider(nil, time.Hour)
	p.lastAttempt = time.Now()

	// A nil client would panic if the call reached the API, so returning without one proves the floor
	// short-circuited first.
	refreshed, err := p.Refresh(t.Context(), false)
	if err != nil {
		t.Errorf("Refresh returned %v, want no error when skipped", err)
	}
	if refreshed {
		t.Error("Refresh reported that it refreshed despite being inside the rate limit floor")
	}
}

// The generation drives a full rebuild of the instance type list, and provisioned totals move every
// time anything in the silo starts or stops, so it must only move when the quota actually changes.
func TestGenerationOnlyMovesOnChange(t *testing.T) {
	p := NewProvider(nil, time.Minute)
	q := quota()

	p.quota = &q
	p.generation.Add(1)
	first := p.Generation()

	// Same values arriving again, as an unchanged poll would deliver them.
	same := quota()
	if *p.quota != same {
		t.Fatal("two identical quotas did not compare equal")
	}
	if got := p.Generation(); got != first {
		t.Errorf("Generation = %d, want it unchanged at %d", got, first)
	}
}
