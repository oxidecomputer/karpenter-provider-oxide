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
	c := NewCapacityManager(nil)
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

// The refusal the subsumed entry recorded still happened, and the shape replacing it is smaller, so
// the run of refusals continues rather than starting over. Without this the entry that survives claims
// to be a first refusal and backs off for three minutes on the strength of it.
func TestSubsumingAShapeInheritsItsHistory(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	ctx := context.Background()
	large := Specs{Ncpus: 16, MemInGiB: 32}
	small := Specs{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(ctx, large, ReasonCapacity)
	c.RecordICE(ctx, large, ReasonCapacity) // 2 refusals, 6m
	c.RecordICE(ctx, small, ReasonCapacity)

	if got := ices(t, c, small); got != 3 {
		t.Errorf("consecutive ICEs = %d, want the subsumed entry's run continued", got)
	}
	if got := retryAfterOf(t, c, small).Sub(now); got != 12*time.Minute {
		t.Errorf("backoff = %v, want the third refusal's backoff", got)
	}
}

// The failure this whole mechanism exists to prevent, and the one the flat-3m behaviour let through: a
// full rack does not refuse one shape repeatedly, it refuses whatever the scheduler proposes, and the
// scheduler answers each refusal with something a little smaller. Every one of those is a new key, so
// counting per key holds the backoff at its minimum for as long as there is a smaller shape to try.
func TestWalkingDownShapesStillBacksOff(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	ctx := context.Background()

	// 21-42, 20-40, ... 17-34, as observed against a rack with nowhere to put any of them.
	for cpus := 21; cpus >= 17; cpus-- {
		c.RecordICE(ctx, Specs{Ncpus: cpus, MemInGiB: cpus * 2}, ReasonCapacity)
	}

	last := Specs{Ncpus: 17, MemInGiB: 34}
	if got := ices(t, c, last); got != 5 {
		t.Errorf("consecutive ICEs = %d, want one run of 5 rather than 5 runs of 1", got)
	}
	if got := retryAfterOf(t, c, last).Sub(now); got != 30*time.Minute {
		t.Errorf("backoff = %v, want the run to have reached the cap", got)
	}
	if got := len(c.Snapshot()); got != 1 {
		t.Errorf("snapshot has %d entries, want the boundary alone", got)
	}
}

// Inheritance is not cumulative across unrelated shapes. Two boundaries that neither covers the other
// describe different limits, and folding their counts together would let a rack with an odd shaped
// hole in it back off as though one shape had been refused over and over.
func TestSubsumptionTakesTheLongestRunNotTheSum(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	ctx := context.Background()

	// Neither covers the other: one is CPU heavy, the other memory heavy.
	c.RecordICE(ctx, Specs{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordICE(ctx, Specs{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordICE(ctx, Specs{Ncpus: 8, MemInGiB: 64}, ReasonCapacity)
	if got := len(c.Snapshot()); got != 2 {
		t.Fatalf("snapshot has %d entries, want two independent boundaries", got)
	}

	// Subsumes both.
	small := Specs{Ncpus: 8, MemInGiB: 32}
	c.RecordICE(ctx, small, ReasonCapacity)

	if got := ices(t, c, small); got != 3 {
		t.Errorf("consecutive ICEs = %d, want the longest run plus this refusal, not the sum", got)
	}
}

// A launch that succeeds is the one thing that disproves the run, and it has to take the inherited
// history with it. Otherwise a boundary that carried a long run forward would leave the next refusal
// of that shape backing off for half an hour on evidence a successful launch already contradicted.
func TestSuccessClearsInheritedHistory(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	ctx := context.Background()

	c.RecordICE(ctx, Specs{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordICE(ctx, Specs{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	small := Specs{Ncpus: 8, MemInGiB: 16}
	c.RecordICE(ctx, small, ReasonCapacity)

	c.RecordSuccess(ctx, small)
	if got := len(c.Snapshot()); got != 0 {
		t.Fatalf("snapshot has %d entries, want the launch to have cleared the boundary", got)
	}

	c.RecordICE(ctx, small, ReasonCapacity)
	if got := retryAfterOf(t, c, small).Sub(now); got != 3*time.Minute {
		t.Errorf("backoff after a successful launch = %v, want the first-refusal backoff", got)
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

// RetryAfter is what an operator reads to know when a shape that is out will be tried again, so a
// refusal while half-open has to push it back out rather than leave the shape looking reopened.
func TestARefusalMovesTheRetryTimeBackOut(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	c.RecordICE(ctx, specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)
	if state := entryOf(t, c, specs).State; state != StateHalfOpen {
		t.Fatalf("state = %q, want the shape reopened by its backoff elapsing", state)
	}

	c.RecordICE(ctx, specs, ReasonCapacity)
	entry := entryOf(t, c, specs)
	if entry.State != StateBackoff {
		t.Errorf("after a second refusal the shape is %q, want %q", entry.State, StateBackoff)
	}
	if got := entry.RetryAfter.Sub(now); got != 6*time.Minute {
		t.Errorf("it retries in %v, want the second backoff of 6m", got)
	}
}

// Jitter keeps a burst of refusals from reopening in the same instant and walking into the same wall
// together. It only ever adds, so a shape is never retried earlier than its backoff.
func TestJitterStaysWithinItsFraction(t *testing.T) {
	now := time.Now()
	c := NewCapacityManager(nil)
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

// The point of the whole mechanism: a shape sitting on a long backoff gets its probe as soon as the
// rack visibly has room for it, instead of waiting out a wait that was set when it did not.
func TestCapacityReleaseShortensALongBackoff(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	for range 5 {
		c.RecordICE(ctx, specs, ReasonCapacity)
		now = now.Add(retryAfterOf(t, c, specs).Sub(now))
	}
	c.RecordICE(ctx, specs, ReasonCapacity)
	if got := retryAfterOf(t, c, specs).Sub(now); got != 30*time.Minute {
		t.Fatalf("backoff = %v, want the capped 30m to be shortened", got)
	}

	now = now.Add(time.Minute)
	c.RecordCapacityReleased(ctx, specs)

	if unavailable, _ := c.IsUnavailable(specs); unavailable {
		t.Error("the shape is still ruled out after the rack gave back room for it")
	}
	if state := stateOf(t, c, specs); state != StateHalfOpen {
		t.Errorf("state = %q, want %q", state, StateHalfOpen)
	}
	if !c.TryAcquire(specs) {
		t.Error("the reopened shape could not be probed")
	}
	if c.TryAcquire(specs) {
		t.Error("a release opened the shape to the whole burst instead of to one probe")
	}
}

// A release is evidence the shape is worth trying, not evidence that it works. If the rack refuses
// the probe anyway, the backoff has to pick up where it left off rather than start over, or a cluster
// churning through terminations retries a hopeless shape every few minutes forever.
func TestCapacityReleaseKeepsTheRefusalHistory(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	c.RecordICE(ctx, specs, ReasonCapacity)
	c.RecordCapacityReleased(ctx, specs)

	if got := ices(t, c, specs); got != 1 {
		t.Errorf("consecutive ICEs = %d, want the release to leave the history alone", got)
	}
	c.RecordICE(ctx, specs, ReasonCapacity)
	if got := retryAfterOf(t, c, specs).Sub(now); got != 6*time.Minute {
		t.Errorf("backoff after the refused probe = %v, want the escalation to continue at 6m", got)
	}
}

// Freeing one small instance must not reopen the whole rack. A claim that a shape does not fit is only
// disturbed by a release big enough to have made room for that shape.
func TestCapacityReleaseOnlyReopensWhatItCouldFit(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	ctx := context.Background()

	for _, specs := range []Specs{
		{Ncpus: 2, MemInGiB: 4},   // fits in the release
		{Ncpus: 8, MemInGiB: 16},  // exactly the release
		{Ncpus: 16, MemInGiB: 16}, // too many cpus
		{Ncpus: 8, MemInGiB: 64},  // too much memory
	} {
		c.RecordICE(ctx, specs, ReasonCapacity)
	}

	c.RecordCapacityReleased(ctx, Specs{Ncpus: 8, MemInGiB: 16})

	for _, tc := range []struct {
		specs Specs
		want  State
	}{
		{Specs{Ncpus: 2, MemInGiB: 4}, StateHalfOpen},
		{Specs{Ncpus: 8, MemInGiB: 16}, StateHalfOpen},
		{Specs{Ncpus: 16, MemInGiB: 16}, StateBackoff},
		{Specs{Ncpus: 8, MemInGiB: 64}, StateBackoff},
	} {
		if got := stateOf(t, c, tc.specs); got != tc.want {
			t.Errorf("%d-%d state = %q, want %q",
				tc.specs.Ncpus, tc.specs.MemInGiB, got, tc.want)
		}
	}
}

// Offerings are only rebuilt when the generation moves, so a release that reopens something has to
// move it, and one that reopens nothing must not: a cluster terminating nodes steadily would
// otherwise rebuild thousands of instance types for every one of them.
func TestCapacityReleaseMovesTheGenerationOnlyWhenItReopens(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	ctx := context.Background()
	c.RecordICE(ctx, Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	before := c.Generation()
	c.RecordCapacityReleased(ctx, Specs{Ncpus: 1, MemInGiB: 2})
	if got := c.Generation(); got != before {
		t.Error("a release that reopened nothing rebuilt the offerings anyway")
	}

	c.RecordCapacityReleased(ctx, Specs{Ncpus: 8, MemInGiB: 16})
	if got := c.Generation(); got == before {
		t.Error("a release that reopened a shape did not move the generation")
	}
}

// A shape that is already being probed is not disturbed by a release. It is mid-experiment, and
// clearing the interlock underneath it would let a second launch pile onto the same question.
func TestCapacityReleaseDoesNotDisturbAnInFlightProbe(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	specs := Specs{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	c.RecordICE(ctx, specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)
	if !c.TryAcquire(specs) {
		t.Fatal("the probe could not be acquired once the backoff elapsed")
	}

	c.RecordCapacityReleased(ctx, specs)

	if c.TryAcquire(specs) {
		t.Error("the release released a probe that was still in flight")
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
	p := NewProvider(nil, nil)

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
	p := NewProvider(nil, nil)
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

// searchSim drives the manager the way the scheduler and the rack together do: it repeatedly offers
// the largest shape still available, launches it, and reports the outcome back. maxLaunchableCPUs is
// the rack's real boundary, which the manager cannot see and has to find.
//
// Shapes are the 1:2 memory family, which is what a NodeClaim's candidates look like: a chain under
// covers, so there is exactly one largest available shape at any moment.
type searchSim struct {
	c                 *CapacityManager
	maxLaunchableCPUs int
	maxCPUs           int
	// attempts is every shape launched, in order, and launched is the ones the rack accepted.
	attempts []int
	launched []int
}

// largestAvailable is the shape the scheduler would pick for a NodeClaim that wants as much as it
// can get, which is what a big bin-packed claim asks for.
func (s *searchSim) largestAvailable() (Specs, bool) {
	for cpus := s.maxCPUs; cpus >= 1; cpus-- {
		specs := Specs{Ncpus: cpus, MemInGiB: cpus * 2}
		if unavailable, _ := s.c.IsUnavailable(specs); !unavailable {
			return specs, true
		}
	}
	return Specs{}, false
}

// run launches shapes until one is placed or the budget is spent, and returns the shape that
// launched.
func (s *searchSim) run(t *testing.T, budget int) Specs {
	t.Helper()
	for range budget {
		specs, ok := s.largestAvailable()
		if !ok {
			t.Fatalf("every shape was ruled out after %d attempts: %v", len(s.attempts), s.attempts)
		}
		s.attempts = append(s.attempts, specs.Ncpus)
		if specs.Ncpus <= s.maxLaunchableCPUs {
			s.launched = append(s.launched, specs.Ncpus)
			s.c.RecordSuccess(context.Background(), specs)
			return specs
		}
		s.c.RecordICE(context.Background(), specs, ReasonCapacity)
	}
	t.Fatalf("no shape launched within %d attempts: %v", budget, s.attempts)
	return Specs{}
}

// A rack with no room refuses the largest shape, the scheduler comes back one size smaller, and that
// is refused too. Stepping through that one size at a time costs a launch per size and takes the
// better part of twenty minutes across a wide instance type range, so the search descends
// geometrically instead: each refusal past the first halves what is offered next.
func TestCapacitySearchDescendsGeometrically(t *testing.T) {
	now := time.Now()
	sim := &searchSim{
		c:                 newTestCapacityManager(&now),
		maxLaunchableCPUs: 9,
		maxCPUs:           38,
	}

	launched := sim.run(t, 10)

	if launched.Ncpus > sim.maxLaunchableCPUs {
		t.Fatalf("launched %d cpus, which the rack had no room for", launched.Ncpus)
	}
	// Stepping down one size at a time from 38 to 9 would be 29 refusals. Anything close to that is
	// the walk this exists to avoid, whatever the exact halving works out to.
	if len(sim.attempts) > 6 {
		t.Errorf("took %d launches to place a shape, want a geometric descent: %v",
			len(sim.attempts), sim.attempts)
	}
	// Each attempt must be meaningfully smaller than the last, not one size down.
	for i := 2; i < len(sim.attempts); i++ {
		if sim.attempts[i] > sim.attempts[i-1]/2 {
			t.Errorf("attempt %d went from %d to %d cpus, want at most half: %v",
				i, sim.attempts[i-1], sim.attempts[i], sim.attempts)
		}
	}
}

// Halving overshoots on purpose, so the search has to climb back. A shape that launches puts a floor
// under the band nothing has tested, and the next offering is its midpoint rather than the floor
// itself, which is what stops an aggressive skip from leaving the cluster on the smallest shape on
// the rack for as long as the backoff lasts.
func TestCapacitySearchClimbsBackToTheBoundary(t *testing.T) {
	now := time.Now()
	sim := &searchSim{
		c:                 newTestCapacityManager(&now),
		maxLaunchableCPUs: 9,
		maxCPUs:           38,
	}

	first := sim.run(t, 10)
	// The descent is expected to undershoot: it halves rather than testing every size, so it lands
	// below the boundary and there is a band above it nothing has ruled out on evidence.
	if first.Ncpus == sim.maxLaunchableCPUs {
		t.Skip("the descent happened to land exactly on the boundary, leaving nothing to climb")
	}

	// Keep launching. Each success should reach higher than the last until the boundary is found.
	for range 8 {
		if sim.launched[len(sim.launched)-1] == sim.maxLaunchableCPUs {
			break
		}
		sim.run(t, 6)
	}

	best := sim.launched[len(sim.launched)-1]
	if best != sim.maxLaunchableCPUs {
		t.Errorf(
			"settled on %d cpus, want the search to find the boundary at %d: attempts %v, launched %v",
			best,
			sim.maxLaunchableCPUs,
			sim.attempts,
			sim.launched,
		)
	}
	if len(sim.attempts) > 12 {
		t.Errorf("took %d launches to find the boundary: %v", len(sim.attempts), sim.attempts)
	}
}

// A skip is a guess, and a guess must never be the reason nothing can launch. However far a walk
// runs, some shape has to be left offerable.
func TestCapacitySearchNeverRulesOutEveryShape(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)

	// A rack that refuses everything, walked all the way down.
	for _, cpus := range []int{38, 19, 9, 4, 2, 1} {
		c.RecordICE(context.Background(), Specs{Ncpus: cpus, MemInGiB: cpus * 2}, ReasonCapacity)
		smallest := Specs{Ncpus: 1, MemInGiB: 2}
		if unavailable, _ := c.IsUnavailable(smallest); unavailable {
			// The smallest shape may only be ruled out by having been refused itself, never by a skip.
			if cpus > 1 {
				t.Fatalf(
					"a skip after refusing %d cpus ruled out the smallest shape on the rack",
					cpus,
				)
			}
		}
	}
}

// Only the shape that was actually refused clears an entry. A launch below the reach is the search
// working, not evidence against the refusal, and treating it as such would throw away a real refusal
// and send the scheduler straight back to the top of the walk.
func TestCapacitySuccessBelowTheReachDoesNotClearTheRefusal(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)

	c.RecordICE(context.Background(), Specs{Ncpus: 32, MemInGiB: 64}, ReasonCapacity)
	c.RecordICE(context.Background(), Specs{Ncpus: 31, MemInGiB: 62}, ReasonCapacity)

	// The second refusal establishes the walk, so the reach is well below 31 cpus and a much smaller
	// shape is offered.
	c.RecordSuccess(context.Background(), Specs{Ncpus: 4, MemInGiB: 8})

	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 31, MemInGiB: 62}); !unavailable {
		t.Error("a refused shape became available again because something smaller launched")
	}
	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 4, MemInGiB: 8}); unavailable {
		t.Error("the shape that just launched is still ruled out")
	}
}

// Refusals arrive out of order, because a launch already in flight when a shape is refused still
// runs and still fails. The later refusal is for a larger shape, so it does not subsume the smaller
// entry and the two sit side by side.
//
// That is fine as long as the larger one does not search. If it descends a reach of its own, that
// reach reaches below the smaller entry and rules out the smaller entry's own shape, so reopening
// that shape for a probe achieves nothing: the probe cannot launch, and nothing says why.
func TestOutOfOrderRefusalDoesNotSmotherTheSmallerEntry(t *testing.T) {
	now := time.Now()
	c := newTestCapacityManager(&now)
	ctx := context.Background()

	// A walk that has been running a while, so the smaller entry has a long run behind it.
	for _, cpus := range []int{32, 16, 11} {
		c.RecordICE(ctx, Specs{Ncpus: cpus, MemInGiB: cpus * 2}, ReasonCapacity)
	}
	// The 12 CPU launch that was already in flight when 11 was refused now fails too.
	c.RecordICE(ctx, Specs{Ncpus: 12, MemInGiB: 24}, ReasonCapacity)

	// A teardown frees enough for the shape the walk stopped on, reopening it for one probe.
	c.RecordCapacityReleased(ctx, Specs{Ncpus: 11, MemInGiB: 22})

	if got := stateOf(t, c, Specs{Ncpus: 11, MemInGiB: 22}); got != StateHalfOpen {
		t.Fatalf("11-22 state = %q, want it reopened for a probe", got)
	}
	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 11, MemInGiB: 22}); unavailable {
		t.Error("the reopened shape is still ruled out, so its probe can never run")
	}
	if !c.TryAcquire(Specs{Ncpus: 11, MemInGiB: 22}) {
		t.Error("the reopened shape could not be acquired for its probe")
	}
	// The out-of-order refusal is still evidence, and still rules out what it refused.
	if unavailable, _ := c.IsUnavailable(Specs{Ncpus: 12, MemInGiB: 24}); !unavailable {
		t.Error("12-24 was refused and is being offered anyway")
	}
}

// The case this was written for. A controller that has just started has launched nothing, so no entry
// has a floor, and the descent is pure speculation: refusing 10-20 halves to 5-10 and takes the shape
// every node in the cluster is running out of the catalogue along with it. A running node is proof its
// shape placed, and seeding it brackets the search instead.
func TestSeededFloorKeepsARunningShapeOffered(t *testing.T) {
	now := time.Now()
	ctx := context.Background()

	refused := Specs{Ncpus: 10, MemInGiB: 20}
	running := Specs{Ncpus: 5, MemInGiB: 10}

	unseeded := newTestCapacityManager(&now)
	for range walkThreshold + 1 {
		unseeded.RecordICE(ctx, refused, ReasonCapacity)
	}
	if got, _ := unseeded.IsUnavailable(running); !got {
		t.Fatalf(
			"without a floor the descent was expected to rule out %d-%d, so this test proves nothing",
			running.Ncpus,
			running.MemInGiB,
		)
	}

	seeded := newTestCapacityManager(&now)
	seeded.SeedLaunchable(ctx, running)
	for range walkThreshold + 1 {
		seeded.RecordICE(ctx, refused, ReasonCapacity)
	}
	if got, _ := seeded.IsUnavailable(running); got {
		t.Errorf("IsUnavailable(%d-%d) = true, want a shape the cluster is running to stay offered",
			running.Ncpus, running.MemInGiB)
	}
	// between(5-10, 10-20) puts the midpoint at 7-15 and rules out from one step above it.
	if got := entryOf(t, seeded, refused).Reach; got != (Specs{Ncpus: 8, MemInGiB: 16}) {
		t.Errorf("reach = %d-%d, want the bracketed midpoint 8-16", got.Ncpus, got.MemInGiB)
	}
}

// A seed is weaker evidence than a launch and must never displace one. RecordSuccess sets a floor
// during the entry's life; a seed taken from a node that has been up for hours is older news.
func TestSeededFloorDoesNotDisplaceALaunchedOne(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	c := newTestCapacityManager(&now)

	c.SeedLaunchable(ctx, Specs{Ncpus: 2, MemInGiB: 4})
	c.RecordICE(ctx, Specs{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordSuccess(ctx, Specs{Ncpus: 8, MemInGiB: 16})

	if got := entryOf(
		t,
		c,
		Specs{Ncpus: 16, MemInGiB: 32},
	).Floor; got != (Specs{Ncpus: 8, MemInGiB: 16}) {
		t.Errorf(
			"floor = %d-%d, want the launched shape rather than the seed",
			got.Ncpus,
			got.MemInGiB,
		)
	}
}

// Only the largest seed matters: a smaller running shape says nothing a larger one does not, and
// order of arrival is whatever order the NodeClaims happened to list in.
func TestSeedKeepsTheLargestShape(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	c := newTestCapacityManager(&now)

	c.SeedLaunchable(ctx, Specs{Ncpus: 12, MemInGiB: 24})
	c.SeedLaunchable(ctx, Specs{Ncpus: 2, MemInGiB: 4})

	refused := Specs{Ncpus: 32, MemInGiB: 64}
	for range walkThreshold + 1 {
		c.RecordICE(ctx, refused, ReasonCapacity)
	}
	if got, _ := c.IsUnavailable(Specs{Ncpus: 12, MemInGiB: 24}); got {
		t.Error("the largest seeded shape was ruled out, so a later smaller seed overwrote it")
	}
}

// A seed at or above the shape just refused describes a rack that has changed since. The refusal is
// the newer evidence, so the search must fall back to descending rather than bracket above a shape
// known not to fit.
func TestSeedAtOrAboveTheRefusalIsDropped(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	c := newTestCapacityManager(&now)

	c.SeedLaunchable(ctx, Specs{Ncpus: 16, MemInGiB: 32})
	refused := Specs{Ncpus: 8, MemInGiB: 16}
	for range walkThreshold + 1 {
		c.RecordICE(ctx, refused, ReasonCapacity)
	}

	entry := entryOf(t, c, refused)
	if known(entry.Floor) {
		t.Errorf("floor = %d-%d, want a stale seed above the refusal to be dropped",
			entry.Floor.Ncpus, entry.Floor.MemInGiB)
	}
	if got := entry.Reach; got != (Specs{Ncpus: 4, MemInGiB: 8}) {
		t.Errorf("reach = %d-%d, want the halving descent 4-8", got.Ncpus, got.MemInGiB)
	}
}
