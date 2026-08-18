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

// UnavailableOfferings tracks the instance shapes the rack has recently refused to place, keyed by
// CPU count and memory.
//
// Entries are stored as the exact shape that failed, but cover every shape at least that large in
// both dimensions: a rack with no room for 8 CPUs and 16 GiB has no room for 16 CPUs and 32 GiB
// either, and there is nothing to learn by trying. Entries expire, so a shape that failed only
// because of what the rack happened to be running is reconsidered rather than written off.
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
func (u *UnavailableOfferings) Mark(ctx context.Context, specs Specs) {
	u.mu.Lock()
	defer u.mu.Unlock()

	key := SpecsToInstanceTypeName(specs.Ncpus, specs.MemInGiB)
	u.cache.SetDefault(key, specs)
	for existing, item := range u.cache.Items() {
		if existing == key {
			continue
		}
		other, ok := item.Object.(Specs)
		if !ok {
			continue
		}
		if covers(specs, other) {
			u.cache.Delete(existing)
		}
	}
	u.generation.Add(1)

	log.FromContext(ctx).V(1).Info(
		"marking instance shape unavailable after an insufficient capacity error",
		"ncpus", specs.Ncpus,
		"memory-gib", specs.MemInGiB,
		"ttl", unavailableTTL,
	)
}

// IsUnavailable reports whether a shape is covered by an unmarked entry.
func (u *UnavailableOfferings) IsUnavailable(specs Specs) bool {
	return covered(u.Snapshot(), specs)
}

// Snapshot returns the shapes currently marked unavailable. Callers checking many shapes at once
// take one of these rather than asking about each shape individually, since every question is
// answered against the same set.
func (u *UnavailableOfferings) Snapshot() []Specs {
	u.mu.Lock()
	defer u.mu.Unlock()

	items := u.cache.Items()
	specs := make([]Specs, 0, len(items))
	for _, item := range items {
		if s, ok := item.Object.(Specs); ok {
			specs = append(specs, s)
		}
	}
	return specs
}

// Generation identifies the current set of unavailable shapes. It changes whenever the set does,
// and carries no other meaning.
func (u *UnavailableOfferings) Generation() uint64 {
	return u.generation.Load()
}

// covered reports whether any of the marked shapes covers this one.
func covered(marked []Specs, specs Specs) bool {
	for _, m := range marked {
		if covers(m, specs) {
			return true
		}
	}
	return false
}

// covers reports whether a failure to place marked also rules out specs, which is the case when
// specs asks for at least as much of both resources.
func covers(marked, specs Specs) bool {
	return specs.Ncpus >= marked.Ncpus && specs.MemInGiB >= marked.MemInGiB
}
