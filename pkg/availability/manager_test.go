package availability

import (
	"context"
	"sync"
	"testing"
	"time"
)

func newTestManager(now *time.Time) *Manager {
	c := New(nil)
	c.now = func() time.Time { return *now }
	c.jitter = func(time.Duration) time.Duration { return 0 }
	return c
}

func TestManagerCoversLargerShapes(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	c.RecordICE(context.Background(), Shape{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	for _, tc := range []struct {
		specs Shape
		want  bool
	}{
		{Shape{Ncpus: 8, MemInGiB: 16}, true},
		{Shape{Ncpus: 16, MemInGiB: 32}, true},
		{Shape{Ncpus: 8, MemInGiB: 64}, true},
		{Shape{Ncpus: 4, MemInGiB: 16}, false},
		{Shape{Ncpus: 16, MemInGiB: 8}, false},
		{Shape{Ncpus: 1, MemInGiB: 2}, false},
	} {
		if got, _ := unavailableFor(c, tc.specs); got != tc.want {
			t.Errorf("Check(%d cpus, %d GiB) unavailable = %v, want %v",
				tc.specs.Ncpus, tc.specs.MemInGiB, got, tc.want)
		}
	}
}

func TestManagerDropsSubsumedEntries(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	c.RecordICE(context.Background(), Shape{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordICE(context.Background(), Shape{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	if got := len(c.Snapshot().Entries()); got != 1 {
		t.Fatalf("snapshot has %d entries, want 1", got)
	}
	if unavailable, _ := unavailableFor(c, Shape{Ncpus: 16, MemInGiB: 32}); !unavailable {
		t.Error("the subsumed shape is no longer covered")
	}
}

func TestSubsumingAShapeInheritsItsHistory(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()
	large := Shape{Ncpus: 16, MemInGiB: 32}
	small := Shape{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(ctx, large, ReasonCapacity)
	c.RecordICE(ctx, large, ReasonCapacity)
	c.RecordICE(ctx, small, ReasonCapacity)

	if got := ices(t, c, small); got != 3 {
		t.Errorf("consecutive ICEs = %d, want the subsumed entry's run continued", got)
	}
	if got := retryAfterOf(t, c, small).Sub(now); got != 12*time.Minute {
		t.Errorf("backoff = %v, want the third refusal's backoff", got)
	}
}

func TestWalkingDownShapesStillBacksOff(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()

	for cpus := 21; cpus >= 17; cpus-- {
		c.RecordICE(ctx, Shape{Ncpus: cpus, MemInGiB: cpus * 2}, ReasonCapacity)
	}

	last := Shape{Ncpus: 17, MemInGiB: 34}
	if got := ices(t, c, last); got != 5 {
		t.Errorf("consecutive ICEs = %d, want one run of 5 rather than 5 runs of 1", got)
	}
	if got := retryAfterOf(t, c, last).Sub(now); got != 30*time.Minute {
		t.Errorf("backoff = %v, want the run to have reached the cap", got)
	}
	if got := len(c.Snapshot().Entries()); got != 1 {
		t.Errorf("snapshot has %d entries, want the boundary alone", got)
	}
}

func TestSubsumptionTakesTheLongestRunNotTheSum(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()

	c.RecordICE(ctx, Shape{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordICE(ctx, Shape{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordICE(ctx, Shape{Ncpus: 8, MemInGiB: 64}, ReasonCapacity)
	if got := len(c.Snapshot().Entries()); got != 2 {
		t.Fatalf("snapshot has %d entries, want two independent boundaries", got)
	}

	small := Shape{Ncpus: 8, MemInGiB: 32}
	c.RecordICE(ctx, small, ReasonCapacity)

	if got := ices(t, c, small); got != 3 {
		t.Errorf("consecutive ICEs = %d, want the longest run plus this refusal, not the sum", got)
	}
}

func TestSuccessClearsInheritedHistory(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()

	c.RecordICE(ctx, Shape{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordICE(ctx, Shape{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	small := Shape{Ncpus: 8, MemInGiB: 16}
	c.RecordICE(ctx, small, ReasonCapacity)

	c.RecordSuccess(ctx, small)
	if got := len(c.Snapshot().Entries()); got != 0 {
		t.Fatalf("snapshot has %d entries, want the launch to have cleared the boundary", got)
	}

	c.RecordICE(ctx, small, ReasonCapacity)
	if got := retryAfterOf(t, c, small).Sub(now); got != 3*time.Minute {
		t.Errorf("backoff after a successful launch = %v, want the first-refusal backoff", got)
	}
}

func TestFirstICEBacksOffThreeMinutes(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)

	now = now.Add(3*time.Minute - time.Second)
	if unavailable, _ := unavailableFor(c, specs); !unavailable {
		t.Fatal("the shape reopened before its backoff elapsed")
	}
	now = now.Add(2 * time.Second)
	if unavailable, _ := unavailableFor(c, specs); unavailable {
		t.Fatal("the shape is still unavailable after its backoff elapsed")
	}
	if state := stateOf(t, c, specs); state != StateHalfOpen {
		t.Errorf("state = %q, want %q", state, StateHalfOpen)
	}
}

func TestRepeatedICEsLengthenTheBackoff(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}

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

		now = now.Add(want)
		if unavailable, _ := unavailableFor(c, specs); unavailable {
			t.Fatalf("the shape did not reopen after %v", want)
		}
	}
}

func TestLookupsDoNotExtendTheBackoff(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)
	retryAfter := retryAfterOf(t, c, specs)

	for range 10 {
		now = now.Add(15 * time.Second)
		unavailableFor(c, specs)
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

func TestSuccessResetsHistory(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	for range 3 {
		c.RecordICE(ctx, specs, ReasonCapacity)
		now = now.Add(retryAfterOf(t, c, specs).Sub(now))
	}

	c.RecordSuccess(ctx, specs)
	if got := len(c.Snapshot().Entries()); got != 0 {
		t.Fatalf("snapshot has %d entries after a successful launch, want 0", got)
	}

	c.RecordICE(ctx, specs, ReasonCapacity)
	if got := retryAfterOf(t, c, specs).Sub(now); got != 3*time.Minute {
		t.Errorf("backoff after the reset = %v, want the first-refusal backoff", got)
	}
}

func TestSuccessOnlyClearsEntriesItDisproves(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()

	c.RecordICE(ctx, Shape{Ncpus: 4, MemInGiB: 8}, ReasonCapacity)
	c.RecordICE(ctx, Shape{Ncpus: 32, MemInGiB: 64}, ReasonCapacity)

	c.RecordSuccess(ctx, Shape{Ncpus: 8, MemInGiB: 16})

	if unavailable, _ := unavailableFor(c, Shape{Ncpus: 8, MemInGiB: 16}); unavailable {
		t.Error("the launched shape is still ruled out by the smaller entry it disproved")
	}
	if unavailable, _ := unavailableFor(c, Shape{Ncpus: 32, MemInGiB: 64}); !unavailable {
		t.Error("placing 8 cpus wrongly cleared the entry for a much larger shape")
	}
}

func TestOnlyOneProbeRunsAtATime(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)

	if !acquireShape(c, specs) {
		t.Fatal("the first launch could not acquire the probe")
	}
	if acquireShape(c, specs) {
		t.Error("a second launch acquired a probe that was already in flight")
	}

	if acquireShape(c, Shape{Ncpus: 16, MemInGiB: 32}) {
		t.Error("a launch of a covered shape acquired the in-flight probe")
	}

	if !acquireShape(c, Shape{Ncpus: 2, MemInGiB: 4}) {
		t.Error("an uncovered shape was blocked by an unrelated probe")
	}

	releaseShape(specs)
	if !acquireShape(c, specs) {
		t.Error("the probe was not handed back on release")
	}
}

func TestProbeRefusalReclosesTheBreaker(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	c.RecordICE(ctx, specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)
	if !acquireShape(c, specs) {
		t.Fatal("the probe could not be acquired once the backoff elapsed")
	}

	c.RecordICE(ctx, specs, ReasonCapacity)

	if unavailable, _ := unavailableFor(c, specs); !unavailable {
		t.Error("the shape stayed available after its probe was refused")
	}
	if got := retryAfterOf(t, c, specs).Sub(now); got != 6*time.Minute {
		t.Errorf("backoff after the refused probe = %v, want 6m", got)
	}

	now = now.Add(6 * time.Minute)
	if !acquireShape(c, specs) {
		t.Error("the next probe could not be acquired, so the refused one leaked its reservation")
	}
}

func TestProbeSuccessOpensTheShapeToEveryone(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	c.RecordICE(ctx, specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)
	if !acquireShape(c, specs) {
		t.Fatal("the probe could not be acquired once the backoff elapsed")
	}

	c.RecordSuccess(ctx, specs)

	if !acquireShape(c, specs) {
		t.Error("a second launch was still held off after the probe succeeded")
	}
	if !acquireShape(c, specs) {
		t.Error("launches are still being serialized after the breaker closed")
	}
}

func TestConcurrentProbesDoNotStampede(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		acquired int
	)
	for range 32 {
		wg.Go(func() {
			if acquireShape(c, specs) {
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

func TestBackedOffShapesCannotBeAcquired(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)

	if acquireShape(c, specs) {
		t.Error("a backed off shape was acquired for launch")
	}
}

func TestReopeningChangesTheGeneration(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}

	c.RecordICE(context.Background(), specs, ReasonCapacity)
	backedOff := c.Generation()

	now = now.Add(3 * time.Minute)
	if reopened := c.Generation(); reopened == backedOff {
		t.Error("the generation did not move when the shape reopened")
	}

	stable := c.Generation()
	now = now.Add(time.Minute)
	if got := c.Generation(); got != stable {
		t.Errorf("generation = %d, want it settled at %d once the shape has reopened", got, stable)
	}
}

func TestHistoryIsForgottenEventually(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	c.RecordICE(ctx, specs, ReasonCapacity)
	c.RecordICE(ctx, specs, ReasonCapacity)

	now = now.Add(historyTTL / 2)
	if got := ices(t, c, specs); got != 2 {
		t.Fatalf("consecutive ICEs = %d, want the history still held at half the TTL", got)
	}

	now = now.Add(historyTTL)
	if got := len(c.Snapshot().Entries()); got != 0 {
		t.Fatalf("snapshot has %d entries, want the history forgotten", got)
	}
	c.RecordICE(ctx, specs, ReasonCapacity)
	if got := retryAfterOf(t, c, specs).Sub(now); got != 3*time.Minute {
		t.Errorf("backoff after the history aged out = %v, want the first-refusal backoff", got)
	}
}

func TestARefusalMovesTheRetryTimeBackOut(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}
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

func TestJitterStaysWithinItsFraction(t *testing.T) {
	now := time.Now()
	c := New(nil)
	c.now = func() time.Time { return now }
	specs := Shape{Ncpus: 8, MemInGiB: 16}

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

func TestCapacityReleaseShortensALongBackoff(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}
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

	if unavailable, _ := unavailableFor(c, specs); unavailable {
		t.Error("the shape is still ruled out after the rack gave back room for it")
	}
	if state := stateOf(t, c, specs); state != StateHalfOpen {
		t.Errorf("state = %q, want %q", state, StateHalfOpen)
	}
	if !acquireShape(c, specs) {
		t.Error("the reopened shape could not be probed")
	}
	if acquireShape(c, specs) {
		t.Error("a release opened the shape to the whole burst instead of to one probe")
	}
}

func TestCapacityReleaseKeepsTheRefusalHistory(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}
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

func TestCapacityReleaseOnlyReopensWhatItCouldFit(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()

	for _, specs := range []Shape{
		{Ncpus: 2, MemInGiB: 4},
		{Ncpus: 8, MemInGiB: 16},
		{Ncpus: 16, MemInGiB: 16},
		{Ncpus: 8, MemInGiB: 64},
	} {
		c.RecordICE(ctx, specs, ReasonCapacity)
	}

	c.RecordCapacityReleased(ctx, Shape{Ncpus: 8, MemInGiB: 16})

	for _, tc := range []struct {
		specs Shape
		want  State
	}{
		{Shape{Ncpus: 2, MemInGiB: 4}, StateHalfOpen},
		{Shape{Ncpus: 8, MemInGiB: 16}, StateHalfOpen},
		{Shape{Ncpus: 16, MemInGiB: 16}, StateBackoff},
		{Shape{Ncpus: 8, MemInGiB: 64}, StateBackoff},
	} {
		if got := stateOf(t, c, tc.specs); got != tc.want {
			t.Errorf("%d-%d state = %q, want %q",
				tc.specs.Ncpus, tc.specs.MemInGiB, got, tc.want)
		}
	}
}

func TestCapacityReleaseMovesTheGenerationOnlyWhenItReopens(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()
	c.RecordICE(ctx, Shape{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	before := c.Generation()
	c.RecordCapacityReleased(ctx, Shape{Ncpus: 1, MemInGiB: 2})
	if got := c.Generation(); got != before {
		t.Error("a release that reopened nothing rebuilt the offerings anyway")
	}

	c.RecordCapacityReleased(ctx, Shape{Ncpus: 8, MemInGiB: 16})
	if got := c.Generation(); got == before {
		t.Error("a release that reopened a shape did not move the generation")
	}
}

func TestCapacityReleaseDoesNotDisturbAnInFlightProbe(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	specs := Shape{Ncpus: 8, MemInGiB: 16}
	ctx := context.Background()

	c.RecordICE(ctx, specs, ReasonCapacity)
	now = now.Add(3 * time.Minute)
	if !acquireShape(c, specs) {
		t.Fatal("the probe could not be acquired once the backoff elapsed")
	}

	c.RecordCapacityReleased(ctx, specs)

	if acquireShape(c, specs) {
		t.Error("the release released a probe that was still in flight")
	}
}

func entryOf(t *testing.T, c *Manager, specs Shape) Entry {
	t.Helper()
	for _, e := range c.Snapshot().Entries() {
		if e.Shape == specs {
			return e
		}
	}
	t.Fatalf("no entry for %d cpus and %d GiB", specs.Ncpus, specs.MemInGiB)
	return Entry{}
}

func stateOf(t *testing.T, c *Manager, specs Shape) State {
	t.Helper()
	return entryOf(t, c, specs).State
}

func retryAfterOf(t *testing.T, c *Manager, specs Shape) time.Time {
	t.Helper()
	return entryOf(t, c, specs).RetryAfter
}

func ices(t *testing.T, c *Manager, specs Shape) int {
	t.Helper()
	return entryOf(t, c, specs).ConsecutiveICEs
}

type searchSim struct {
	c                 *Manager
	maxLaunchableCPUs int
	maxCPUs           int

	attempts []int
	launched []int
}

func (s *searchSim) largestAvailable() (Shape, bool) {
	for cpus := s.maxCPUs; cpus >= 1; cpus-- {
		specs := Shape{Ncpus: cpus, MemInGiB: cpus * 2}
		if unavailable, _ := unavailableFor(s.c, specs); !unavailable {
			return specs, true
		}
	}
	return Shape{}, false
}

func (s *searchSim) run(t *testing.T, budget int) Shape {
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
	return Shape{}
}

func TestCapacitySearchDescendsGeometrically(t *testing.T) {
	now := time.Now()
	sim := &searchSim{
		c:                 newTestManager(&now),
		maxLaunchableCPUs: 9,
		maxCPUs:           38,
	}

	launched := sim.run(t, 10)

	if launched.Ncpus > sim.maxLaunchableCPUs {
		t.Fatalf("launched %d cpus, which the rack had no room for", launched.Ncpus)
	}

	if len(sim.attempts) > 6 {
		t.Errorf("took %d launches to place a shape, want a geometric descent: %v",
			len(sim.attempts), sim.attempts)
	}

	for i := 2; i < len(sim.attempts); i++ {
		if sim.attempts[i] > sim.attempts[i-1]/2 {
			t.Errorf("attempt %d went from %d to %d cpus, want at most half: %v",
				i, sim.attempts[i-1], sim.attempts[i], sim.attempts)
		}
	}
}

func TestCapacitySearchClimbsBackToTheBoundary(t *testing.T) {
	now := time.Now()
	sim := &searchSim{
		c:                 newTestManager(&now),
		maxLaunchableCPUs: 9,
		maxCPUs:           38,
	}

	first := sim.run(t, 10)

	if first.Ncpus == sim.maxLaunchableCPUs {
		t.Skip("the descent happened to land exactly on the boundary, leaving nothing to climb")
	}

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

func TestCapacitySearchNeverRulesOutEveryShape(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)

	for _, cpus := range []int{38, 19, 9, 4, 2, 1} {
		c.RecordICE(context.Background(), Shape{Ncpus: cpus, MemInGiB: cpus * 2}, ReasonCapacity)
		smallest := Shape{Ncpus: 1, MemInGiB: 2}
		if unavailable, _ := unavailableFor(c, smallest); unavailable {

			if cpus > 1 {
				t.Fatalf(
					"a skip after refusing %d cpus ruled out the smallest shape on the rack",
					cpus,
				)
			}
		}
	}
}

func TestCapacitySuccessBelowTheReachDoesNotClearTheRefusal(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)

	c.RecordICE(context.Background(), Shape{Ncpus: 32, MemInGiB: 64}, ReasonCapacity)
	c.RecordICE(context.Background(), Shape{Ncpus: 31, MemInGiB: 62}, ReasonCapacity)

	c.RecordSuccess(context.Background(), Shape{Ncpus: 4, MemInGiB: 8})

	if unavailable, _ := unavailableFor(c, Shape{Ncpus: 31, MemInGiB: 62}); !unavailable {
		t.Error("a refused shape became available again because something smaller launched")
	}
	if unavailable, _ := unavailableFor(c, Shape{Ncpus: 4, MemInGiB: 8}); unavailable {
		t.Error("the shape that just launched is still ruled out")
	}
}

func TestOutOfOrderRefusalDoesNotSmotherTheSmallerEntry(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()

	for _, cpus := range []int{32, 16, 11} {
		c.RecordICE(ctx, Shape{Ncpus: cpus, MemInGiB: cpus * 2}, ReasonCapacity)
	}

	c.RecordICE(ctx, Shape{Ncpus: 12, MemInGiB: 24}, ReasonCapacity)

	c.RecordCapacityReleased(ctx, Shape{Ncpus: 11, MemInGiB: 22})

	if got := stateOf(t, c, Shape{Ncpus: 11, MemInGiB: 22}); got != StateHalfOpen {
		t.Fatalf("11-22 state = %q, want it reopened for a probe", got)
	}
	if unavailable, _ := unavailableFor(c, Shape{Ncpus: 11, MemInGiB: 22}); unavailable {
		t.Error("the reopened shape is still ruled out, so its probe can never run")
	}
	if !acquireShape(c, Shape{Ncpus: 11, MemInGiB: 22}) {
		t.Error("the reopened shape could not be acquired for its probe")
	}

	if unavailable, _ := unavailableFor(c, Shape{Ncpus: 12, MemInGiB: 24}); !unavailable {
		t.Error("12-24 was refused and is being offered anyway")
	}
}

func TestSeededFloorKeepsARunningShapeOffered(t *testing.T) {
	now := time.Now()
	ctx := context.Background()

	refused := Shape{Ncpus: 10, MemInGiB: 20}
	running := Shape{Ncpus: 5, MemInGiB: 10}

	unseeded := newTestManager(&now)
	for range walkThreshold + 1 {
		unseeded.RecordICE(ctx, refused, ReasonCapacity)
	}
	if got, _ := unavailableFor(unseeded, running); !got {
		t.Fatalf(
			"without a floor the descent was expected to rule out %d-%d, so this test proves nothing",
			running.Ncpus,
			running.MemInGiB,
		)
	}

	seeded := newTestManager(&now)
	seeded.SeedLaunchable(ctx, running)
	for range walkThreshold + 1 {
		seeded.RecordICE(ctx, refused, ReasonCapacity)
	}
	if got, _ := unavailableFor(seeded, running); got {
		t.Errorf(
			"Check(%d-%d) reports unavailable, want a shape the cluster is running to stay offered",
			running.Ncpus,
			running.MemInGiB,
		)
	}

	if got := entryOf(t, seeded, refused).Reach; got != (Shape{Ncpus: 8, MemInGiB: 16}) {
		t.Errorf("reach = %d-%d, want the bracketed midpoint 8-16", got.Ncpus, got.MemInGiB)
	}
}

func TestSeededFloorDoesNotDisplaceALaunchedOne(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	c := newTestManager(&now)

	c.SeedLaunchable(ctx, Shape{Ncpus: 2, MemInGiB: 4})
	c.RecordICE(ctx, Shape{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	c.RecordSuccess(ctx, Shape{Ncpus: 8, MemInGiB: 16})

	if got := entryOf(
		t,
		c,
		Shape{Ncpus: 16, MemInGiB: 32},
	).Floor; got != (Shape{Ncpus: 8, MemInGiB: 16}) {
		t.Errorf(
			"floor = %d-%d, want the launched shape rather than the seed",
			got.Ncpus,
			got.MemInGiB,
		)
	}
}

func TestSeedKeepsTheLargestShape(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	c := newTestManager(&now)

	c.SeedLaunchable(ctx, Shape{Ncpus: 12, MemInGiB: 24})
	c.SeedLaunchable(ctx, Shape{Ncpus: 2, MemInGiB: 4})

	refused := Shape{Ncpus: 32, MemInGiB: 64}
	for range walkThreshold + 1 {
		c.RecordICE(ctx, refused, ReasonCapacity)
	}
	if got, _ := unavailableFor(c, Shape{Ncpus: 12, MemInGiB: 24}); got {
		t.Error("the largest seeded shape was ruled out, so a later smaller seed overwrote it")
	}
}

func TestSeedAtOrAboveTheRefusalIsDropped(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	c := newTestManager(&now)

	c.SeedLaunchable(ctx, Shape{Ncpus: 16, MemInGiB: 32})
	refused := Shape{Ncpus: 8, MemInGiB: 16}
	for range walkThreshold + 1 {
		c.RecordICE(ctx, refused, ReasonCapacity)
	}

	entry := entryOf(t, c, refused)
	if known(entry.Floor) {
		t.Errorf("floor = %d-%d, want a stale seed above the refusal to be dropped",
			entry.Floor.Ncpus, entry.Floor.MemInGiB)
	}
	if got := entry.Reach; got != (Shape{Ncpus: 4, MemInGiB: 8}) {
		t.Errorf("reach = %d-%d, want the halving descent 4-8", got.Ncpus, got.MemInGiB)
	}
}
