// Package resolver is the machinery every resource provider under pkg/providers is built out of: a
// TTL cache keyed by the question a resolution answered, the fingerprinting that produces that key,
// and the glob matching selectors are evaluated with.
//
// It exists because the six providers do the same thing to six different Oxide resources. Each one
// is a selector-evaluation function and nothing else; caching, single-flighting, staleness and the
// metrics that describe all three are here, once, so that the providers look alike and a change to
// how freshness works is a change to one file.
//
// The cache holds post-selector-evaluation results only. An entry's value is the resolved status
// slice a nodeclass publishes, never a raw API listing: the key is a fingerprint of the selectors,
// so an edited selector is a different key and the world is re-read whenever the question changes.
// Caching the world instead would need invalidation, and invalidation is the thing this design is
// built to avoid.
package resolver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/samber/lo"
)

// staleMultiplier is how many consecutive failed refreshes a cache absorbs before it stops serving
// the last good reading and returns the failure instead.
//
// Derived from the TTL rather than fixed in wall clock, because the two cadences here differ by six
// times over: at three refreshes it reads as "three refreshes in a row could not be made" for both,
// whereas any single wall-clock number would be either shorter than one provider's own refresh
// interval or far longer than the other's.
const staleMultiplier = 3

// entryTTLIdle is how long an unread entry is kept. An entry not read for this long belongs to
// selectors nothing is asking about any more -- almost always a deleted nodeclass -- so dropping it
// is what garbage collects the cache without a delete hook to plumb through.
const entryTTLIdle = time.Hour

// Entry is one cached resolution and how old it is.
type Entry[T any] struct {
	Value   T
	TakenAt time.Time
	// Stale is set when this reading was served because the resolution that should have replaced it
	// failed. The value is still the last thing the API said, so it is worth publishing; what Stale
	// says is that nothing has confirmed it since TakenAt.
	Stale bool
}

// Age is how long ago the reading was taken, as of now.
func (e Entry[T]) Age(now time.Time) time.Duration {
	if e.TakenAt.IsZero() {
		return 0
	}
	return now.Sub(e.TakenAt)
}

// record is the cache's own bookkeeping for one key.
//
// mu is held across the resolve rather than only around the map access, which is what collapses
// concurrent Gets for one key into a single API call: the second caller blocks, and by the time it
// runs the entry it was about to resolve is already there and fresh.
type record[T any] struct {
	mu       sync.Mutex
	value    T
	takenAt  time.Time
	lastRead time.Time
	// failingSince is when the run of failed resolutions began, and zero while the last resolution
	// succeeded. Reported when the staleness ceiling is crossed, because "the API call failed" does
	// not distinguish the first failure from the twentieth.
	failingSince time.Time
	// held is whether takenAt/value mean anything yet. A key that has never resolved has nothing to
	// serve and nothing to age.
	held bool
}

// Cache serves resolutions for one resource type, keyed by the question that produced them.
type Cache[T any] struct {
	// name is the provider this cache belongs to, used as the provider label on every series below.
	name string
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	records map[string]*record[T]
}

// NewCache returns a cache that treats a reading older than ttl as needing a refresh.
func NewCache[T any](name string, ttl time.Duration) *Cache[T] {
	return NewCacheWithClock[T](name, ttl, time.Now)
}

// NewCacheWithClock is NewCache with the clock supplied, so that a test can move time rather than
// wait for it. Nothing in production passes anything but time.Now.
func NewCacheWithClock[T any](name string, ttl time.Duration, now func() time.Time) *Cache[T] {
	return &Cache[T]{
		name:    name,
		ttl:     ttl,
		now:     now,
		records: map[string]*record[T]{},
	}
}

// Name is the provider this cache belongs to.
func (c *Cache[T]) Name() string { return c.name }

// TTL is how old a reading may be before it is refreshed.
func (c *Cache[T]) TTL() time.Duration { return c.ttl }

// staleLimit is how old the last good reading may be before a failed refresh stops being absorbed.
func (c *Cache[T]) staleLimit() time.Duration { return staleMultiplier * c.ttl }

// Get returns the cached resolution for key, calling resolve when the entry is missing or older than
// the TTL. Concurrent Gets for the same key collapse into one resolve.
//
// A failed resolve is absorbed while a previous reading is still worth believing: that reading is
// returned with Stale set, its age, and a nil error, so a transient API failure does not take a
// nodeclass un-Ready over a reading that has not meaningfully aged. Once the reading is older than
// the staleness ceiling the failure is returned instead, and the caller's existing error path
// surfaces it -- which for the nodeclass controller means failing the reconcile, backing off, and
// leaving status exactly as it was. A failed resolve with nothing cached always returns the error.
func (c *Cache[T]) Get(
	ctx context.Context,
	key string,
	resolve func(context.Context) (T, error),
) (Entry[T], error) {
	rec := c.record(key)

	rec.mu.Lock()
	defer rec.mu.Unlock()

	now := c.now()
	rec.lastRead = now
	if rec.held && now.Sub(rec.takenAt) < c.ttl {
		metrics.ProviderCacheHits.WithLabelValues(c.name).Inc()
		return Entry[T]{Value: rec.value, TakenAt: rec.takenAt}, nil
	}

	value, err := resolve(ctx)
	now = c.now()
	if err == nil {
		metrics.ProviderResolutions.WithLabelValues(c.name, metrics.ResolutionSuccess).Inc()
		rec.value, rec.takenAt, rec.held, rec.failingSince = value, now, true, time.Time{}
		c.publishGauges()
		return Entry[T]{Value: value, TakenAt: now}, nil
	}

	metrics.ProviderResolutions.WithLabelValues(c.name, metrics.ResolutionError).Inc()
	if rec.failingSince.IsZero() {
		rec.failingSince = now
	}
	// Nothing to fall back on: the first resolution of these selectors is also the one that failed,
	// so the caller's error path is the only honest answer.
	if !rec.held {
		c.publishGauges()
		return Entry[T]{}, err
	}
	age := now.Sub(rec.takenAt)
	if age > c.staleLimit() {
		// Serving indefinitely would make a revoked token look exactly like a healthy cluster, so
		// past the ceiling the cache stops absorbing and says how long it has been failing.
		c.publishGauges()
		return Entry[T]{}, fmt.Errorf(
			"no %s resolution has succeeded for %s (last reading is %s old): %w",
			c.name,
			now.Sub(rec.failingSince).Round(time.Second),
			age.Round(time.Second),
			err,
		)
	}
	metrics.ProviderStaleServed.WithLabelValues(c.name).Inc()
	c.publishGauges()
	return Entry[T]{Value: rec.value, TakenAt: rec.takenAt, Stale: true}, nil
}

// record returns the bookkeeping for key, creating it if this is the first time it has been asked
// about, and prunes entries nothing has read for a while on the way past.
//
// Pruning is inline rather than on a timer because these caches hold single-digit entries: one per
// distinct selector set across the cluster's nodeclasses.
func (c *Cache[T]) record(key string) *record[T] {
	c.mu.Lock()
	defer c.mu.Unlock()

	cutoff := c.now().Add(-entryTTLIdle)
	for k, rec := range c.records {
		if k != key && rec.lastRead.Before(cutoff) {
			delete(c.records, k)
		}
	}

	rec, ok := c.records[key]
	if !ok {
		rec = &record[T]{}
		c.records[key] = rec
	}
	return rec
}

// publishGauges republishes the two gauges that describe the cache as a whole. Called after every
// resolution rather than on a timer, since a resolution is the only thing that changes either.
func (c *Cache[T]) publishGauges() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	oldest := time.Duration(0)
	for _, rec := range c.records {
		// Reading takenAt without rec.mu. The gauge is a description of the cache rather than an
		// input to any decision, so a value caught mid-update is not worth serializing every
		// resolution behind one lock for.
		if !rec.held {
			continue
		}
		if age := now.Sub(rec.takenAt); age > oldest {
			oldest = age
		}
	}
	metrics.ProviderOldestEntryAge.WithLabelValues(c.name).Set(metrics.Seconds(oldest))
	metrics.ProviderCacheEntries.WithLabelValues(c.name).Set(float64(len(c.records)))
}

// Fingerprint is the cache key for a resolution: a hash of the selectors that produced it, plus
// every other input that changes the answer -- notably the project a name is resolved in, since the
// same name in two projects is two different resources.
//
// Built on hashstructure the same way OxideNodeClass.Hash is, so the same selectors written in a
// different order are one key rather than two.
//
// It must never be taken over a whole nodeclass. Doing so would make an unrelated userData edit a
// cache miss for every resource, which is the opposite of why the key is a fingerprint of the
// question rather than of the object that asked it.
func Fingerprint(v any) string {
	return fmt.Sprint(
		lo.Must(hashstructure.Hash(v, hashstructure.FormatV2, &hashstructure.HashOptions{
			SlicesAsSets:    true,
			IgnoreZeroValue: true,
			ZeroNil:         true,
		})),
	)
}

// Resolution is the envelope every provider returns, so that all six read the same at the call site.
//
// T is the status type published to the nodeclass and S the selector type that produced it. The two
// halves are separate because a selector matching nothing is not an error: an unreadable API is
// retried, whereas a selector that is simply wrong is reported on the nodeclass for an operator to
// fix, and the condition message has to name the selector to do that.
type Resolution[T any, S any] struct {
	// Resolved is what to publish to status.
	Resolved []T
	// Unmatched are the selectors that matched nothing, for the condition message.
	Unmatched []S
	// Reason and Message are the condition to record when the resolution did not complete at all, as
	// opposed to completing and finding nothing.
	Reason  string
	Message string
	// Age is how old the reading is, and zero when it was just taken.
	Age time.Duration
	// Stale is whether the reading was served because a refresh failed. See Entry.
	Stale bool
}

// Complete reports whether the resolution completed, and so whether Resolved may be published.
func (r Resolution[T, S]) Complete() bool { return r.Reason == "" }
