// Package resolver caches provider lookups.
package resolver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/samber/lo"
)

const entryTTLIdle = time.Hour

// Entry is one cached resolution and how old it is.
type Entry[T any] struct {
	Value   T
	TakenAt time.Time
	// Stale is set when this reading was served because the resolution that should have replaced it failed.
	Stale bool
}

// Age is how long ago the reading was taken, as of now.
func (e Entry[T]) Age(now time.Time) time.Duration {
	if e.TakenAt.IsZero() {
		return 0
	}
	return now.Sub(e.TakenAt)
}

// Cache locking rules:
//   - Cache.mu protects only map membership.
//   - record.mu protects every record field.
//   - resolution holds only the relevant record lock, so different keys remain concurrent.
//   - code that needs both locks acquires record.mu before Cache.mu.
//   - metrics snapshot map membership and record fields in separate locking passes.
type record[T any] struct {
	// mu protects every field in this record.
	mu       sync.Mutex
	value    T
	takenAt  time.Time
	lastRead time.Time

	failingSince time.Time

	held bool
}

// Cache serves resolutions for one resource type, keyed by the question that produced them.
type Cache[T any] struct {
	name string
	ttl  time.Duration
	now  func() time.Time

	// mu protects only records. Code must not acquire a record lock while holding mu.
	mu      sync.Mutex
	records map[string]*record[T]
}

// NewCache returns a cache that coalesces reads of one key arriving within ttl of each other into a single resolution.
func NewCache[T any](name string, ttl time.Duration) *Cache[T] {
	return NewCacheWithClock[T](name, ttl, time.Now)
}

// NewCacheWithClock builds a cache with a custom clock.
func NewCacheWithClock[T any](name string, ttl time.Duration, now func() time.Time) *Cache[T] {
	return &Cache[T]{
		name:    name,
		ttl:     ttl,
		now:     now,
		records: map[string]*record[T]{},
	}
}

// Get returns the cached resolution for key, calling resolve when the entry is missing or older than the TTL.
func (c *Cache[T]) Get(
	ctx context.Context,
	key string,
	resolve func(context.Context) (T, error),
) (Entry[T], error) {
	rec := c.lockRecord(key)
	now := c.now()
	rec.lastRead = now
	if rec.held && now.Sub(rec.takenAt) < c.ttl {
		metrics.ProviderCacheHits.WithLabelValues(c.name).Inc()
		entry := Entry[T]{Value: rec.value, TakenAt: rec.takenAt}
		rec.mu.Unlock()
		c.evictIdle(key)
		return entry, nil
	}

	value, err := resolve(ctx)
	now = c.now()
	if err == nil {
		metrics.ProviderResolutions.WithLabelValues(c.name, metrics.ResolutionSuccess).Inc()
		rec.value, rec.takenAt, rec.held, rec.failingSince = value, now, true, time.Time{}
		rec.mu.Unlock()
		c.evictIdle(key)
		c.publishGauges()
		return Entry[T]{Value: value, TakenAt: now}, nil
	}

	metrics.ProviderResolutions.WithLabelValues(c.name, metrics.ResolutionError).Inc()
	if rec.failingSince.IsZero() {
		rec.failingSince = now
	}

	if !rec.held {
		rec.mu.Unlock()
		c.evictIdle(key)
		c.publishGauges()
		return Entry[T]{}, err
	}
	age := now.Sub(rec.takenAt)

	if age > providers.StaleLimit {
		failingFor := now.Sub(rec.failingSince).Round(time.Second)
		rec.mu.Unlock()
		c.evictIdle(key)
		c.publishGauges()
		return Entry[T]{}, fmt.Errorf(
			"no %s resolution has succeeded for %s (last reading is %s old): %w",
			c.name,
			failingFor,
			age.Round(time.Second),
			err,
		)
	}
	metrics.ProviderStaleServed.WithLabelValues(c.name).Inc()
	entry := Entry[T]{Value: rec.value, TakenAt: rec.takenAt, Stale: true}
	rec.mu.Unlock()
	c.evictIdle(key)
	c.publishGauges()
	return entry, nil
}

// lockRecord returns the current record for key with its mutex held. If eviction
// removed the record before it could be locked, it retries with the replacement.
func (c *Cache[T]) lockRecord(key string) *record[T] {
	for {
		rec := c.record(key)
		rec.mu.Lock()
		c.mu.Lock()
		current := c.records[key] == rec
		c.mu.Unlock()
		if current {
			return rec
		}
		rec.mu.Unlock()
	}
}

func (c *Cache[T]) record(key string) *record[T] {
	c.mu.Lock()
	rec, ok := c.records[key]
	if !ok {
		rec = &record[T]{}
		c.records[key] = rec
	}
	c.mu.Unlock()
	return rec
}

// evictIdle removes records that stayed idle through the final check under their record lock.
func (c *Cache[T]) evictIdle(currentKey string) {
	c.mu.Lock()
	candidates := make(map[string]*record[T], len(c.records))
	for key, candidate := range c.records {
		if key != currentKey {
			candidates[key] = candidate
		}
	}
	c.mu.Unlock()

	cutoff := c.now().Add(-entryTTLIdle)
	for key, candidate := range candidates {
		candidate.mu.Lock()
		if candidate.lastRead.Before(cutoff) {
			c.mu.Lock()
			if c.records[key] == candidate {
				delete(c.records, key)
			}
			c.mu.Unlock()
		}
		candidate.mu.Unlock()
	}
}

func (c *Cache[T]) recordSnapshot() []*record[T] {
	c.mu.Lock()
	records := make([]*record[T], 0, len(c.records))
	for _, rec := range c.records {
		records = append(records, rec)
	}
	c.mu.Unlock()
	return records
}

func (c *Cache[T]) publishGauges() {
	records := c.recordSnapshot()
	now := c.now()
	oldest := time.Duration(0)
	for _, rec := range records {
		rec.mu.Lock()
		held, takenAt := rec.held, rec.takenAt
		rec.mu.Unlock()
		if !held {
			continue
		}
		if age := now.Sub(takenAt); age > oldest {
			oldest = age
		}
	}
	metrics.ProviderOldestEntryAge.WithLabelValues(c.name).Set(metrics.Seconds(oldest))
	metrics.ProviderCacheEntries.WithLabelValues(c.name).Set(float64(len(records)))
}

// Fingerprint is the cache key for a resolution: a hash of the selectors that produced it.
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
type Resolution[T any, S any] struct {
	// Resolved is what to publish to status.
	Resolved []T
	// Unmatched contains unmatched selectors.
	Unmatched []S
	// Reason and Message are the condition to record when the resolution did not complete at all.
	Reason  string
	Message string
	// Age is how old the reading is, and zero when it was just taken.
	Age time.Duration
	// Stale is whether the reading was served because a refresh failed.
	Stale bool
}

// Complete reports whether the resolution completed, and so whether Resolved may be published.
func (r Resolution[T, S]) Complete() bool { return r.Reason == "" }
