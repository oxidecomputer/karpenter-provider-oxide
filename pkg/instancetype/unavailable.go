package instancetype

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickmn/go-cache"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// unavailableTTL is how long a shape stays out of consideration after the rack refused to place
	// it. Capacity frees up as workloads come and go, so this is short enough that a shape is retried
	// once the rack has plausibly changed, and long enough that a provisioning loop does not walk
	// into the same failure repeatedly.
	unavailableTTL = 3 * time.Minute
	// unavailableCleanupInterval is how often expired entries are swept. Expiry is what puts a shape
	// back into consideration, and offerings are only recomputed when the set of unavailable shapes
	// changes, so the sweep is what makes an expiry take effect.
	unavailableCleanupInterval = 30 * time.Second
)

// Reason is why a shape is out of consideration. The two are not interchangeable: one is the rack
// having no room right now, which time may fix on its own, and the other is the silo not being
// entitled to the resources, which only an administrator or a teardown fixes.
//
// The distinction exists because the Oxide API does not make it. Exceeding a silo quota and finding
// no sled with room both return InsufficientCapacity, so the reason is decided by comparing the
// refused shape against the quota headroom rather than by reading it off the error.
type Reason string

const (
	// ReasonCapacity is the rack having no sled with room for the shape.
	ReasonCapacity Reason = "InsufficientCapacity"
	// ReasonQuota is the silo's quota having no room for the shape.
	ReasonQuota Reason = "QuotaExceeded"
)

// Entry is one unavailable shape and why.
type Entry struct {
	Specs  Specs
	Reason Reason
}

// UnavailableOfferings tracks the instance shapes recently refused, keyed by CPU count and memory.
//
// Entries are stored as the exact shape that failed, but cover every shape at least that large in
// both dimensions: a rack with no room for 8 CPUs and 16 GiB has no room for 16 CPUs and 32 GiB
// either, and there is nothing to learn by trying. The same holds for a quota. Entries expire, so a
// shape that failed only because of what the rack happened to be running is reconsidered rather than
// written off.
type UnavailableOfferings struct {
	mu    sync.Mutex
	cache *cache.Cache
	// generation changes whenever the covered set does, which is how the instance type provider
	// knows its offerings have to be recomputed. Bumped both when a shape is marked and when the
	// sweep expires one, since either direction changes what is available.
	generation atomic.Uint64
}

func NewUnavailableOfferings() *UnavailableOfferings {
	u := &UnavailableOfferings{
		cache: cache.New(unavailableTTL, unavailableCleanupInterval),
	}
	u.cache.OnEvicted(func(string, any) {
		u.generation.Add(1)
	})
	return u
}

// Mark takes a shape, and every shape at least as large, out of consideration.
//
// Marking refreshes the entry when the shape is already covered: being refused again is fresh
// evidence, and the point of the TTL is to time out from the last refusal rather than the first.
// Entries the new one subsumes are dropped, since they no longer describe anything it does not.
// Subsumption ignores the reason, because coverage is what an offering is filtered on; a smaller
// shape refused for quota genuinely does explain a larger one refused for capacity.
func (u *UnavailableOfferings) Mark(ctx context.Context, specs Specs, reason Reason) {
	u.mu.Lock()
	defer u.mu.Unlock()

	key := SpecsToInstanceTypeName(specs.Ncpus, specs.MemInGiB)
	u.cache.SetDefault(key, Entry{Specs: specs, Reason: reason})
	for existing, item := range u.cache.Items() {
		if existing == key {
			continue
		}
		other, ok := item.Object.(Entry)
		if !ok {
			continue
		}
		if covers(specs, other.Specs) {
			u.cache.Delete(existing)
		}
	}
	u.generation.Add(1)

	log.FromContext(ctx).V(1).Info(
		"marking instance shape unavailable",
		"ncpus", specs.Ncpus,
		"memory-gib", specs.MemInGiB,
		"reason", reason,
		"ttl", unavailableTTL,
	)
}

// IsUnavailable reports whether a shape is covered by an unexpired entry, and why.
func (u *UnavailableOfferings) IsUnavailable(specs Specs) (bool, Reason) {
	return covered(u.Snapshot(), specs)
}

// Snapshot returns the entries currently marked unavailable. Callers checking many shapes at once
// take one of these rather than asking about each shape individually, since every question is
// answered against the same set.
func (u *UnavailableOfferings) Snapshot() []Entry {
	u.mu.Lock()
	defer u.mu.Unlock()

	items := u.cache.Items()
	entries := make([]Entry, 0, len(items))
	for _, item := range items {
		if e, ok := item.Object.(Entry); ok {
			entries = append(entries, e)
		}
	}
	return entries
}

// Generation identifies the current set of unavailable shapes. It changes whenever the set does,
// and carries no other meaning.
func (u *UnavailableOfferings) Generation() uint64 {
	return u.generation.Load()
}

// covered reports whether any entry covers this shape, and why.
//
// When entries with both reasons cover the shape, quota wins. It is the more useful thing to tell an
// operator: rack capacity is transient and Karpenter will retry into it, whereas a quota will keep
// refusing every shape this size until somebody changes it.
func covered(entries []Entry, specs Specs) (bool, Reason) {
	found := false
	reason := Reason("")
	for _, e := range entries {
		if !covers(e.Specs, specs) {
			continue
		}
		found = true
		if e.Reason == ReasonQuota {
			return true, ReasonQuota
		}
		reason = e.Reason
	}
	return found, reason
}

// covers reports whether a failure to place marked also rules out specs, which is the case when
// specs asks for at least as much of both resources.
func covers(marked, specs Specs) bool {
	return specs.Ncpus >= marked.Ncpus && specs.MemInGiB >= marked.MemInGiB
}
