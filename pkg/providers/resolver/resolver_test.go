package resolver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
)

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

func TestIdenticalSelectorsShareOneEntry(t *testing.T) {
	cache := NewCacheWithClock[string]("test", ttl, newClock().Now)
	resolver := &counter{value: "first"}

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

func TestAFailedResolutionPastTheCeilingReturnsTheError(t *testing.T) {
	clk := newClock()
	cache := NewCacheWithClock[string]("test", ttl, clk.Now)
	resolver := &counter{value: "first"}

	if _, err := cache.Get(context.Background(), "key", resolver.resolve); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	resolver.err = errors.New("the API is unreachable")

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

func TestAFailedResolutionWithNothingCachedReturnsTheError(t *testing.T) {
	cache := NewCacheWithClock[string]("test", ttl, newClock().Now)
	want := errors.New("the API is unreachable")

	_, err := cache.Get(context.Background(), "key", (&counter{err: want}).resolve)
	if !errors.Is(err, want) {
		t.Errorf("Get() error = %v, want %v", err, want)
	}
}

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

func TestConcurrentGetsCollapse(t *testing.T) {
	cache := NewCacheWithClock[string]("test", ttl, newClock().Now)
	var calls atomic.Int64
	resolve := func(context.Context) (string, error) {
		calls.Add(1)
		return "first", nil
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := cache.Get(context.Background(), "key", resolve); err != nil {
				t.Errorf("Get() error = %v", err)
			}
		})
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Errorf("resolve called %d times across 8 concurrent gets, want 1", got)
	}
}

func TestDifferentKeysResolveConcurrently(t *testing.T) {
	cache := NewCacheWithClock[string]("test", ttl, newClock().Now)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	resolve := func(context.Context) (string, error) {
		entered <- struct{}{}
		<-release
		return "value", nil
	}

	var wg sync.WaitGroup
	for _, key := range []string{"first", "second"} {
		wg.Go(func() {
			if _, err := cache.Get(context.Background(), key, resolve); err != nil {
				t.Errorf("Get() error = %v", err)
			}
		})
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("different keys did not enter their resolvers concurrently")
		}
	}
	close(release)
	wg.Wait()
}

func TestEntryBecomingActiveWhileEvictionWaitsIsKept(t *testing.T) {
	clk := newClock()
	cache := NewCacheWithClock[string]("test", ttl, clk.Now)
	if _, err := cache.Get(context.Background(), "active", (&counter{value: "value"}).resolve); err != nil {
		t.Fatal(err)
	}
	clk.advance(entryTTLIdle + time.Minute)

	active := cache.records["active"]
	active.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.evictIdle("trigger")
	}()
	active.lastRead = clk.Now()
	active.mu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("eviction did not finish")
	}
	cache.mu.Lock()
	got := cache.records["active"]
	cache.mu.Unlock()
	if got != active {
		t.Fatal("entry that became active while eviction waited was removed")
	}
}

func TestGaugePublicationWaitsForActiveResolutionWithoutDeadlocking(t *testing.T) {
	cache := NewCacheWithClock[string]("test", ttl, newClock().Now)
	entered := make(chan struct{})
	release := make(chan struct{})
	resolved := make(chan struct{})
	go func() {
		defer close(resolved)
		_, _ = cache.Get(context.Background(), "key", func(context.Context) (string, error) {
			close(entered)
			<-release
			return "value", nil
		})
	}()
	<-entered

	published := make(chan struct{})
	go func() {
		defer close(published)
		cache.publishGauges()
	}()
	close(release)

	for name, done := range map[string]<-chan struct{}{"resolution": resolved, "publication": published} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s deadlocked", name)
		}
	}
}

func TestConcurrentResolutionEvictionAndPublication(t *testing.T) {
	clk := newClock()
	cache := NewCacheWithClock[string]("test", 0, clk.Now)
	var calls atomic.Int64
	resolve := func(context.Context) (string, error) {
		calls.Add(1)
		return "value", nil
	}

	var wg sync.WaitGroup
	for worker := range 32 {
		wg.Go(func() {
			for iteration := range 100 {
				key := fmt.Sprintf("key-%d", (worker+iteration)%8)
				if _, err := cache.Get(context.Background(), key, resolve); err != nil {
					t.Errorf("Get() error = %v", err)
					return
				}
				if iteration%10 == 0 {
					clk.advance(entryTTLIdle)
					cache.evictIdle("eviction-trigger")
				}
				cache.publishGauges()
			}
		})
	}
	wg.Wait()
	if calls.Load() == 0 {
		t.Fatal("resolver was never called")
	}
}
