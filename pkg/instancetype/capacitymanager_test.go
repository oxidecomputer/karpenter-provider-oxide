package instancetype

import (
	"context"
	"sync"
	"testing"
	"time"
)

// newTestCapacityManager returns a manager on a clock the test drives, with jitter switched off so
// backoffs can be compared exactly. Real jitter is only there to spread a burst out.
func newTestCapacityManager(now *time.Time) *CapacityManager {
	c := NewCapacityManager()
	c.now = func() time.Time { return *now }
	c.jitter = func(time.Duration) time.Duration { return 0 }
	return c
}

func TestCapacityManagerCoversLargerShapes(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	c.RecordICE(context.Background(), Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	for _, tc := range []struct {
		specs Specs
		want  bool
	}{
		{Specs{Ncpus: 8, MemInGiB: 16}, true},  // the refused shape itself
		{Specs{Ncpus: 16, MemInGiB: 32}, true}, // larger in both
		{Specs{Ncpus: 8, MemInGiB: 64}, true},  // larger in memory only
		{Specs{Ncpus: 4, MemInGiB: 16}, false}, // smaller in cpus
		{Specs{Ncpus: 16, MemInGiB: 8}, false}, // more cpus but less memory
		{Specs{Ncpus: 1, MemInGiB: 2}, false},  // smaller in both
	} {
		if got, _ := c.IsUnavailable(tc.specs); got != tc.want {
			t.Errorf("IsUnavailable(%d cpus, %d GiB) = %v, want %v",
				tc.specs.Ncpus, tc.specs.MemInGiB, got, tc.want)
		}
	}
}

func TestCapacityManagerDropsSubsumedEntries(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	c.RecordICE(context.Background(), Specs{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordICE(context.Background(), Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	if got := len(c.Snapshot()); got != 1 {
		t.Fatalf("snapshot has %d entries, want 1", got)
	}
	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 16, MemInGiB: 32}); !unavailable {
		t.Error("the subsumed shape is no longer covered")
	}
}

// The first refusal is the cheap one to retry: capacity comes and goes, so the shape is reconsidered
// as soon as the rack has plausibly changed.
func TestFirstICEBacksOffThreeMinutes(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)

	now = now.Add(3*time.Minute - time.Second)
	if unavailable, _ := c.IsUnavailable(specs); !unavailable {
		t.Fatal("the shape reopened before its backoff elapsed")
	}
	now = now.Add(2 * time.Second)
	if unavailable, _ := c.IsUnavailable(specs); unavailable {
		t.Fatal("the shape is still unavailable after its backoff elapsed")
	}
	if state := stateOf(t, c, specs); state != StateHalfOpen {
		t.Errorf("state = %q, want %q", state, StateHalfOpen)
	}
}

// Each consecutive refusal roughly doubles the wait, up to the cap. A shape the rack keeps refusing is
// worth retrying less and less often, but never worth writing off.
func TestRepeatedICEsLengthenTheBackoff(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}

	for _, want := range []time.Duration{
		3 * time.Minute,
		6 * time.Minute,
		12 * time.Minute,
		24 * time.Minute,
		30 * time.Minute,
		30 * time.Minute,
	} {
		c.RecordICE(context.Background(), specs, ReasonCapacity)
		if got := retryAfterOf(t, c, specs).Sub(now); got != want {
			t.Fatalf("backoff after %d refusals = %v, want %v", ices(t, c, specs), got, want)
		}
		// Wait it out, which reopens the shape, and let the next probe be refused in turn.
		now = now.Add(want)
		if unavailable, _ := c.IsUnavailable(specs); unavailable {
			t.Fatalf("the shape did not reopen after %v", want)
		}
	}
}

// Asking about a shape is not evidence about the rack. Only a refused launch is, and a lookup that
// pushed the retry out would let a busy scheduler keep a shape backed off indefinitely.
func TestLookupsDoNotExtendTheBackoff(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)
	retryAfter := retryAfterOf(t, c, specs)

	for range 10 {
		now = now.Add(15 * time.Second)
		c.IsUnavailable(specs)
		c.Snapshot()
		c.Generation()
	}

	if got := retryAfterOf(t, c, specs); !got.Equal(retryAfter) {
		t.Errorf("retry after = %v, want it unmoved at %v", got, retryAfter)
	}
	if got := ices(t, c, specs); got != 1 {
		t.Errorf("consecutive ICEs = %d, want the single refusal", got)
	}
}

// A launch that succeeded is proof the rack has room, so the history that says otherwise is wrong and
// goes away entirely. The next incident starts over at three minutes rather than resuming the old
// escalation.
func TestSuccessResetsHistory(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	for range 3 {
		c.RecordICE(ctx, specs, ReasonCapacity)
		now = now.Add(retryAfterOf(t, c, specs).Sub(now))
	}

	c.RecordSuccess(ctx, specs)
	if got := len(c.Snapshot()); got != 0 {
		t.Fatalf("snapshot has %d entries after a successful launch, want 0", got)
	}

	c.RecordICE(ctx, specs, ReasonCapacity)
	if got := retryAfterOf(t, c, specs).Sub(now); got != 3*time.Minute {
		t.Errorf("backoff after the reset = %v, want the first-refusal backoff", got)
	}
}

// A launch of a large shape disproves the claim that nothing that size or larger fits, wherever that
// claim was recorded. It says nothing about shapes larger still.
func TestSuccessOnlyClearsEntriesItDisproves(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	ctx := context.Background()

	c.RecordICE(ctx, Specs{Ncpus: 4, MemInGiB: 8}, ReasonCapacity)
	c.RecordICE(ctx, Specs{Ncpus: 32, MemInGiB: 64}, ReasonCapacity)

	c.RecordSuccess(ctx, Specs{Ncpus: 8, MemInGiB: 16})

	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 8, MemInGiB: 16}); unavailable {
		t.Error("the launched shape is still ruled out by the smaller entry it disproved")
	}
	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 32, MemInGiB: 64}); !unavailable {
		t.Error("placing 8 cpus wrongly cleared the entry for a much larger shape")
	}
}

// The whole point of reopening a shape is that exactly one launch tests it. Letting the rest of a
// burst through would turn one cheap probe into a wave of refusals against a shape that may well fit.
func TestOnlyOneProbeRunsAtATime(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)

	if !c.TryAcquire(specs) {
		t.Fatal("the first launch could not acquire the probe")
	}
	if c.TryAcquire(specs) {
		t.Error("a second launch acquired a probe that was already in flight")
	}
	// Larger shapes are covered by the same entry, so they are probing the same claim.
	if c.TryAcquire(Specs{Ncpus: 16, MemInGiB: 32}) {
		t.Error("a launch of a covered shape acquired the in-flight probe")
	}
	// An unrelated shape has nothing to do with the probe.
	if !c.TryAcquire(Specs{Ncpus: 2, MemInGiB: 4}) {
		t.Error("an uncovered shape was blocked by an unrelated probe")
	}

	c.ReleaseProbe(specs)
	if !c.TryAcquire(specs) {
		t.Error("the probe was not handed back on release")
	}
}

// A refused probe both releases the interlock and lengthens the wait, so the shape leaves half-open
// rather than staying open to the rest of the burst.
func TestProbeRefusalReclosesTheBreaker(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	c.RecordICE(ctx, specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)
	if !c.TryAcquire(specs) {
		t.Fatal("the probe could not be acquired once the backoff elapsed")
	}

	c.RecordICE(ctx, specs, ReasonCapacity)

	if unavailable, _ := c.IsUnavailable(specs); !unavailable {
		t.Error("the shape stayed available after its probe was refused")
	}
	if got := retryAfterOf(t, c, specs).Sub(now); got != 6*time.Minute {
		t.Errorf("backoff after the refused probe = %v, want 6m", got)
	}
	// The probe is gone with the state that held it, not leaked.
	now = now.Add(6 * time.Minute)
	if !c.TryAcquire(specs) {
		t.Error("the next probe could not be acquired, so the refused one leaked its reservation")
	}
}

// A succeeded probe closes the breaker, which is what lets the rest of the burst through.
func TestProbeSuccessOpensTheShapeToEveryone(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	c.RecordICE(ctx, specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)
	if !c.TryAcquire(specs) {
		t.Fatal("the probe could not be acquired once the backoff elapsed")
	}

	c.RecordSuccess(ctx, specs)

	if !c.TryAcquire(specs) {
		t.Error("a second launch was still held off after the probe succeeded")
	}
	if !c.TryAcquire(specs) {
		t.Error("launches are still being serialized after the breaker closed")
	}
}

// Concurrent launches must not stampede a reopened shape: whatever the interleaving, exactly one of
// them gets the probe.
func TestConcurrentProbesDoNotStampede(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		acquired int
	)
	for range 32 {
		wg.Go(func() {
			if c.TryAcquire(specs) {
				mu.Lock()
				acquired++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if acquired != 1 {
		t.Errorf("%d launches acquired the probe, want exactly 1", acquired)
	}
}

// A backed off shape is not offered at all, so a NodeClaim built before the refusal that still asks
// for it is turned away here too rather than launched into a known wall.
func TestBackedOffShapesCannotBeAcquired(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)

	if c.TryAcquire(specs) {
		t.Error("a backed off shape was acquired for launch")
	}
}

// Offerings are only rebuilt when the generation moves, so a backoff elapsing has to move it or the
// reopened shape is never actually offered.
func TestReopeningChangesTheGeneration(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)
	backedOff := c.Generation()

	now = now.Add(3 * time.Minute)
	if reopened := c.Generation(); reopened == backedOff {
		t.Error("the generation did not move when the shape reopened")
	}
	// And settles once nothing more is changing, or the instance type list is rebuilt forever.
	stable := c.Generation()
	now = now.Add(time.Minute)
	if got := c.Generation(); got != stable {
		t.Errorf("generation = %d, want it settled at %d once the shape has reopened", got, stable)
	}
}

// History is what makes the backoff grow, so it outlives the backoff itself. It does not outlive the
// rack it described: a shape refused this morning starts fresh tonight.
func TestHistoryIsForgottenEventually(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	c.RecordICE(ctx, specs, ReasonCapacity)
	c.RecordICE(ctx, specs, ReasonCapacity)

	now = now.Add(historyTTL / 2)
	if got := ices(t, c, specs); got != 2 {
		t.Fatalf("consecutive ICEs = %d, want the history still held at half the TTL", got)
	}

	now = now.Add(historyTTL)
	if got := len(c.Snapshot()); got != 0 {
		t.Fatalf("snapshot has %d entries, want the history forgotten", got)
	}
	c.RecordICE(ctx, specs, ReasonCapacity)
	if got := retryAfterOf(t, c, specs).Sub(now); got != 3*time.Minute {
		t.Errorf("backoff after the history aged out = %v, want the first-refusal backoff", got)
	}
}

// Jitter keeps a burst of refusals from reopening in the same instant and walking into the same wall
// together. It only ever adds, so a shape is never retried earlier than its backoff.
func TestJitterStaysWithinItsFraction(t *testing.T) {
	now := time.Now()
	c := NewCapacityManager()
	c.now = func() time.Time { return now }
	specs := Specs{Ncpus: 8, MemInGiB: 16}

	minBackoff := 3 * time.Minute
	maxJittered := minBackoff + time.Duration(jitterFraction*float64(minBackoff))
	for range 100 {
		c.RecordICE(context.Background(), specs, ReasonCapacity)
		backoff := retryAfterOf(t, c, specs).Sub(now)
		if backoff < minBackoff || backoff > maxJittered {
			t.Fatalf("backoff = %v, want between %v and %v", backoff, minBackoff, maxJittered)
		}
		c.RecordSuccess(context.Background(), specs)
	}
}

func entryOf(t *testing.T, c *CapacityManager, specs Specs) Entry {
	t.Helper()
	for _, e := range c.Snapshot() {
		if e.Specs == specs {
			return e
		}
	}
	t.Fatalf("no entry for %d cpus and %d GiB", specs.Ncpus, specs.MemInGiB)
	return Entry{}
}

func stateOf(t *testing.T, c *CapacityManager, specs Specs) State {
	t.Helper()
	return entryOf(t, c, specs).State
}

func retryAfterOf(t *testing.T, c *CapacityManager, specs Specs) time.Time {
	t.Helper()
	return entryOf(t, c, specs).RetryAfter
}

func ices(t *testing.T, c *CapacityManager, specs Specs) int {
	t.Helper()
	return entryOf(t, c, specs).ConsecutiveICEs
}

func TestInstanceTypesMarkOfferingsUnavailable(t *testing.T) {
	p := NewProvider(nil)

	before, ok := p.Get("8-16")
	if !ok {
		t.Fatal("instance type 8-16 does not exist")
	}
	if !before.Offerings[0].Available {
		t.Fatal("instance type 8-16 is unavailable before anything is refused")
	}

	p.RecordICE(context.Background(), Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"8-16", false},
		{"16-32", false},
		{"4-8", true},
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

// A reopened shape has to make it all the way back into the offerings the scheduler reads, not just
// into the breaker's own view, or nothing ever probes it.
func TestReopenedShapesAreOfferedAgain(t *testing.T) {
	now := time.Now()
	p := NewProvider(nil)
	p.capacity = newTestCapacityManager(&now)

	p.RecordICE(context.Background(), Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)
	if it, _ := p.Get("8-16"); it.Offerings[0].Available {
		t.Fatal("8-16 is still offered while it is backed off")
	}

	now = now.Add(3 * time.Minute)

	if it, _ := p.Get("8-16"); !it.Offerings[0].Available {
		t.Error("8-16 is not offered again once its backoff elapsed")
	}
}
