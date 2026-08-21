package resolver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
)

// clock is a hand-wound clock, so that the ceiling cases can be about how old a reading is rather
// than about how long a test is willing to sleep.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// counter is a resolve function that records how many times it was called, so that a cache hit can be
// asserted as the absence of an API call rather than as the presence of the right answer.
type counter struct {
	calls int
	value string
	err   error
}

func (c *counter) resolve(context.Context) (string, error) {
	c.calls++
	if c.err != nil {
		return "", c.err
	}
	return c.value, nil
}

// ttl is the coalescing window these tests measure against. Its own number, not
// providers.CoalesceWindow, because what a cache does inside its window is the same whatever the
// production window happens to be -- and deliberately not equal to it, so that a test asserting the
// staleness ceiling cannot pass by accident on the two being the same length.
const ttl = 5 * time.Minute

func TestCacheServesAHitWithoutResolving(t *testing.T) {
	clk := newClock()
	cache := NewCacheWithClock[string]("test", ttl, clk.Now)
	resolver := &counter{value: "first"}

	for i := range 3 {
		clk.advance(time.Minute)
		entry, err := cache.Get(context.Background(), "key", resolver.resolve)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if entry.Value != "first" || entry.Stale {
			t.Fatalf("Get() = %+v, want the first reading, not stale", entry)
		}
		if resolver.calls != 1 {
			t.Fatalf("resolve called %d times after %d gets, want 1", resolver.calls, i+1)
		}
	}
}

func TestCacheReresolvesPastTheTTL(t *testing.T) {
	clk := newClock()
	cache := NewCacheWithClock[string]("test", ttl, clk.Now)
	resolver := &counter{value: "first"}

	if _, err := cache.Get(context.Background(), "key", resolver.resolve); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	clk.advance(ttl)
	resolver.value = "second"
	entry, err := cache.Get(context.Background(), "key", resolver.resolve)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if entry.Value != "second" {
		t.Errorf("Get() = %q, want the re-resolved reading", entry.Value)
	}
	if resolver.calls != 2 {
		t.Errorf("resolve called %d times, want 2", resolver.calls)
	}
	if got := entry.Age(clk.Now()); got != 0 {
		t.Errorf("Age() = %s, want a reading just taken", got)
	}
}

// An edited selector is a different question, so it is a different key. This is the whole of cache
// invalidation: nothing has to notice the edit, because nothing looks up the old answer again.
func TestADifferentKeyResolvesSeparately(t *testing.T) {
	cache := NewCacheWithClock[string]("test", ttl, newClock().Now)
	resolver := &counter{value: "first"}

	if _, err := cache.Get(
		context.Background(),
		Fingerprint([]string{"a"}),
		resolver.resolve,
	); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, err := cache.Get(
		context.Background(),
		Fingerprint([]string{"b"}),
		resolver.resolve,
	); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if resolver.calls != 2 {
		t.Errorf("resolve called %d times across two keys, want 2", resolver.calls)
	}
}

// Two nodeclasses selecting the same resources ask the same question, so they share one reading. This
// is what makes the cache shared rather than per nodeclass.
func TestIdenticalSelectorsShareOneEntry(t *testing.T) {
	cache := NewCacheWithClock[string]("test", ttl, newClock().Now)
	resolver := &counter{value: "first"}

	// Written in a different order on purpose: the fingerprint is over the set of selectors, not over
	// the order someone happened to type them in.
	first := Fingerprint([]string{"ubuntu-*", "rke2-*"})
	second := Fingerprint([]string{"rke2-*", "ubuntu-*"})
	if first != second {
		t.Fatalf(
			"Fingerprint() = %q and %q for the same selectors in a different order",
			first,
			second,
		)
	}
	for range 2 {
		if _, err := cache.Get(context.Background(), first, resolver.resolve); err != nil {
			t.Fatalf("Get() error = %v", err)
		}
	}
	if resolver.calls != 1 {
		t.Errorf("resolve called %d times for one question, want 1", resolver.calls)
	}
}

// A transient API failure must not take a nodeclass un-Ready over a reading that has not
// meaningfully aged, so the last good reading is served with Stale set and no error.
func TestAFailedResolutionServesTheLastReading(t *testing.T) {
	clk := newClock()
	cache := NewCacheWithClock[string]("test", ttl, clk.Now)
	resolver := &counter{value: "first"}

	if _, err := cache.Get(context.Background(), "key", resolver.resolve); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	clk.advance(ttl)
	resolver.err = errors.New("the API is unreachable")

	entry, err := cache.Get(context.Background(), "key", resolver.resolve)
	if err != nil {
		t.Fatalf("Get() error = %v, want the previous reading served instead", err)
	}
	if entry.Value != "first" || !entry.Stale {
		t.Errorf("Get() = %+v, want the previous reading marked stale", entry)
	}
	if got := entry.Age(clk.Now()); got != ttl {
		t.Errorf("Age() = %s, want %s", got, ttl)
	}
}

// Past the ceiling the cache stops absorbing. Serving stale indefinitely would make a revoked token
// look identical to a healthy cluster, and the error says how long it has been failing because "the
// API call failed" does not distinguish the first failure from the twentieth.
func TestAFailedResolutionPastTheCeilingReturnsTheError(t *testing.T) {
	clk := newClock()
	cache := NewCacheWithClock[string]("test", ttl, clk.Now)
	resolver := &counter{value: "first"}

	if _, err := cache.Get(context.Background(), "key", resolver.resolve); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	resolver.err = errors.New("the API is unreachable")

	// Three resyncs in a row that could not be made are absorbed; the reading is only over the
	// ceiling once it is older than providers.StaleLimit. Advanced a resync at a time rather than a
	// TTL at a time, because the ceiling is pinned to the resync and not to the coalescing window.
	for range providers.StaleLimit / providers.ResolveInterval {
		clk.advance(providers.ResolveInterval)
		entry, err := cache.Get(context.Background(), "key", resolver.resolve)
		if err != nil {
			t.Fatalf(
				"Get() at age %s error = %v, want the reading still served",
				entry.Age(clk.Now()),
				err,
			)
		}
		if !entry.Stale {
			t.Fatalf("Get() = %+v, want the reading marked stale", entry)
		}
	}

	clk.advance(time.Second)
	_, err := cache.Get(context.Background(), "key", resolver.resolve)
	if err == nil {
		t.Fatal("Get() error = nil, want the failure surfaced past the staleness ceiling")
	}
	if !errors.Is(err, resolver.err) {
		t.Errorf("Get() error = %v, want it to wrap the resolution failure", err)
	}
	if !strings.Contains(err.Error(), "no test resolution has succeeded for") {
		t.Errorf("Get() error = %q, want it to say how long the resolution has been failing", err)
	}
}

// The first resolution of a set of selectors having failed leaves nothing to serve, so the error is
// the only honest answer.
func TestAFailedResolutionWithNothingCachedReturnsTheError(t *testing.T) {
	cache := NewCacheWithClock[string]("test", ttl, newClock().Now)
	want := errors.New("the API is unreachable")

	_, err := cache.Get(context.Background(), "key", (&counter{err: want}).resolve)
	if !errors.Is(err, want) {
		t.Errorf("Get() error = %v, want %v", err, want)
	}
}

// A successful resolution clears the failure history, so a run of failures that recovers does not
// count towards a later run's ceiling.
func TestASuccessfulResolutionClearsStaleness(t *testing.T) {
	clk := newClock()
	cache := NewCacheWithClock[string]("test", ttl, clk.Now)
	resolver := &counter{value: "first"}

	if _, err := cache.Get(context.Background(), "key", resolver.resolve); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	resolver.err = errors.New("the API is unreachable")
	clk.advance(ttl)
	if _, err := cache.Get(context.Background(), "key", resolver.resolve); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	resolver.err, resolver.value = nil, "second"
	clk.advance(time.Minute)
	entry, err := cache.Get(context.Background(), "key", resolver.resolve)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if entry.Value != "second" || entry.Stale {
		t.Errorf("Get() = %+v, want the fresh reading", entry)
	}
}

// An entry nothing has asked about for an hour belongs to selectors nothing is asking about any more,
// almost always a deleted nodeclass. Dropping it is what garbage collects the cache.
func TestAnUnreadEntryIsPruned(t *testing.T) {
	clk := newClock()
	cache := NewCacheWithClock[string]("test", ttl, clk.Now)
	resolver := &counter{value: "first"}

	if _, err := cache.Get(context.Background(), "gone", resolver.resolve); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	clk.advance(entryTTLIdle + time.Minute)
	if _, err := cache.Get(context.Background(), "still-here", resolver.resolve); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(cache.records) != 1 {
		t.Errorf("cache holds %d entries, want only the one still being read", len(cache.records))
	}
}

// Concurrent Gets for one key collapse into one resolve, which is what stops a burst of reconciles
// from issuing a burst of identical API calls.
func TestConcurrentGetsCollapse(t *testing.T) {
	cache := NewCacheWithClock[string]("test", ttl, newClock().Now)

	// Not guarded, and it does not need to be: the cache holds one lock across the resolve, so if
	// this races the test has already found the bug it is here to catch.
	resolver := &counter{value: "first"}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := cache.Get(context.Background(), "key", resolver.resolve); err != nil {
				t.Errorf("Get() error = %v", err)
			}
		})
	}
	wg.Wait()
	if resolver.calls != 1 {
		t.Errorf("resolve called %d times across 8 concurrent gets, want 1", resolver.calls)
	}
}
