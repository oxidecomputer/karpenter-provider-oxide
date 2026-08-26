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

type record[T any] struct {
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

	if !rec.held {
		c.publishGauges()
		return Entry[T]{}, err
	}
	age := now.Sub(rec.takenAt)

	if age > providers.StaleLimit {

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

func (c *Cache[T]) publishGauges() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	oldest := time.Duration(0)
	for _, rec := range c.records {

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
