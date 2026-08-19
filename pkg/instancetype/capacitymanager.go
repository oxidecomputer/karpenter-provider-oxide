package instancetype

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// baseBackoff is how long a shape stays out of consideration after the first refusal. Capacity
	// frees up as workloads come and go, so the first wait is short enough that a shape is retried
	// once the rack has plausibly changed.
	baseBackoff = 3 * time.Minute
	// maxBackoff caps the wait. A shape that has been refused five times running is not going to be
	// placeable any time soon, but the rack is not static either, so the breaker keeps probing at a
	// rate that costs nothing rather than giving up on the shape entirely.
	maxBackoff = 30 * time.Minute
	// jitterFraction is the range of the delay added on top of each backoff, as a fraction of it.
	// Provisioning refuses shapes in bursts, and without jitter the whole burst reopens in the same
	// instant and walks into the same wall together.
	jitterFraction = 0.2
	// historyTTL is how long a shape's refusal history outlives its last refusal. History is what
	// makes the backoff grow, so forgetting it too eagerly turns a repeatedly unplaceable shape back
	// into a three minute retry loop, and keeping it forever punishes a shape for a rack that was
	// full yesterday.
	historyTTL = 6 * time.Hour
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

// State is where a shape sits in the circuit breaker.
//
// A shape with no history is available and has no entry at all, so the state only ever describes a
// shape that has been refused at least once.
type State string

const (
	// StateBackoff is a shape whose last launch was refused and whose wait has not elapsed. It is
	// filtered out of offerings, so the scheduler never considers it.
	StateBackoff State = "backoff"
	// StateHalfOpen is a shape whose wait has elapsed. It is offered again, but only one launch is
	// allowed to test it: either that probe succeeds and the shape is forgotten, or it is refused and
	// the shape goes back into a longer backoff.
	StateHalfOpen State = "half-open"
)

// Entry is one refused shape, why it was refused, and what the breaker has learned about it.
type Entry struct {
	Specs  Specs
	Reason Reason
	State  State

	// ConsecutiveICEs is how many times in a row a launch of this shape has been refused. It is what
	// the backoff is computed from, and only an actual refusal moves it: looking a shape up, or
	// waiting out its backoff, teaches the breaker nothing.
	ConsecutiveICEs int
	// LastICE is when the shape was last refused, which is what its history ages out from.
	LastICE time.Time
	// RetryAfter is when a backed off shape becomes half-open. Meaningless in any other state.
	RetryAfter time.Time

	// probeInFlight is a half-open shape having a launch already testing it. Unexported because it is
	// interlock state rather than something a caller reasons about, and a Snapshot copy of it would be
	// stale the moment it was taken.
	probeInFlight bool
}

// CapacityManager decides which instance shapes are worth launching, keyed by CPU count and memory.
//
// It is a circuit breaker over the rack's refusals. A refused shape is taken out of consideration
// for a while, and each consecutive refusal roughly doubles that wait, so a shape the rack has no
// room for is retried on a schedule that costs less and less rather than at a fixed rate forever.
// When the wait elapses the shape is offered again but only one launch is allowed to test it, which
// is what keeps a burst of pending pods from turning one reopened shape into a stampede of
// refusals.
//
// Entries are stored as the exact shape that failed, but cover every shape at least that large in
// both dimensions: a rack with no room for 8 CPUs and 16 GiB has no room for 16 CPUs and 32 GiB
// either, and there is nothing to learn by trying. The same holds for a quota.
//
// Quota refusals ride the same machinery. The Oxide API reports them identically and the caller only
// tells them apart by inspecting the quota after the fact, so a quota refusal is evidence of the same
// kind: this shape could not be launched just now. The reason is carried through so an operator can
// be told which limit was hit, but it does not change how the shape is retried.
type CapacityManager struct {
	mu      sync.Mutex
	entries map[string]*Entry
	// generation changes whenever the set of unavailable shapes does, which is how the instance type
	// provider knows its offerings have to be recomputed. Bumped when a shape is refused, when one
	// reopens, and when the rack has been read again, since all three change what is available.
	generation atomic.Uint64

	// inventory is what the rack has room for, or nil when inventory is switched off. It is an
	// interface so that this package does not depend on how the rack is read, and so that leaving
	// inventory out is a matter of passing nothing rather than a mode this code has to carry.
	inventory InventoryProvider
	// rack is the last reading of the rack this manager acted on, and rackFresh is whether that
	// reading was still believable at the time. Both are refreshed by every sweep, and are held so
	// that a new reading can be compared against the one before it: what makes a shape worth reopening
	// is the rack having gained room for it, which is not visible in either reading alone.
	rack      capacity.Snapshot
	rackFresh bool

	// now and jitter are injectable so tests can drive the breaker on a fake clock and compare exact
	// backoffs. Nothing outside this package sets them.
	now    func() time.Time
	jitter func(time.Duration) time.Duration
}

// InventoryProvider is the rack inventory the capacity manager consults before believing that a shape
// is worth offering. It may be nil, in which case every decision is made from refusal history alone.
type InventoryProvider interface {
	// Capacity returns the most recent reading of the rack. An unknown snapshot is the answer when
	// the rack has never been read, and is not an error: inventory is opt-in and a provider without it
	// has to keep launching nodes.
	Capacity() capacity.Snapshot
}

// NewCapacityManager builds a capacity manager. inventory may be nil.
func NewCapacityManager(inventory InventoryProvider) *CapacityManager {
	return &CapacityManager{
		entries:   map[string]*Entry{},
		inventory: inventory,
		now:       time.Now,
		jitter:    randomJitter,
	}
}

// RecordICE takes a shape, and every shape at least as large, out of consideration.
//
// Each consecutive refusal lengthens the wait, and the count only resets when a launch of the shape
// actually succeeds. A shape refused while half-open counts too: the probe was the retry, and it
// failed, so the breaker backs further off rather than reopening at the same rate.
//
// Entries the new one subsumes are dropped, since they no longer describe anything it does not.
// Subsumption ignores the reason, because coverage is what an offering is filtered on; a smaller
// shape refused for quota genuinely does explain a larger one refused for capacity.
func (c *CapacityManager) RecordICE(ctx context.Context, specs Specs, reason Reason) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	key := SpecsToInstanceTypeName(specs.Ncpus, specs.MemInGiB)
	entry, ok := c.entries[key]
	if !ok {
		entry = &Entry{Specs: specs}
		c.entries[key] = entry
	}
	from := entry.State
	entry.Reason = reason
	entry.ConsecutiveICEs++
	entry.LastICE = now
	entry.State = StateBackoff
	entry.RetryAfter = now.Add(c.backoffFor(entry.ConsecutiveICEs))
	entry.probeInFlight = false

	for existing, other := range c.entries {
		if existing == key {
			continue
		}
		if covers(specs, other.Specs) {
			delete(c.entries, existing)
		}
	}
	c.generation.Add(1)

	log.FromContext(ctx).V(1).Info(
		"capacity state changed",
		"instance-type", key,
		"from", stateOrAvailable(from),
		"to", StateBackoff,
		"reason", reason,
		"consecutive-ices", entry.ConsecutiveICEs,
		"backoff", entry.RetryAfter.Sub(now),
	)
}

// RecordSuccess reports that a launch of this shape succeeded, which disproves every claim that it
// could not be placed.
//
// That is more than the shape's own entry. An entry for a smaller shape asserts that nothing this
// size or larger fits, and a launch of this shape has just shown otherwise, so those entries are
// wrong about the rack as it stands now and are dropped along with their history. Entries for larger
// shapes are left alone: placing 8 CPUs says nothing about whether 64 fit.
func (c *CapacityManager) RecordSuccess(ctx context.Context, specs Specs) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cleared := 0
	for key, entry := range c.entries {
		if !covers(entry.Specs, specs) {
			continue
		}
		delete(c.entries, key)
		cleared++
		log.FromContext(ctx).V(1).Info(
			"capacity state changed",
			"instance-type", key,
			"from", entry.State,
			"to", "available",
			"reason", "launch-succeeded",
			"launched", SpecsToInstanceTypeName(specs.Ncpus, specs.MemInGiB),
			"consecutive-ices", entry.ConsecutiveICEs,
		)
	}
	if cleared > 0 {
		c.generation.Add(1)
	}
}

// RecordCapacityReleased reports that the rack has taken back the resources of an instance of this
// shape, and reopens the shapes that plausibly makes room for.
//
// The trigger is Oxide confirming the instance is gone, not a delete being asked for. Until the
// instance actually stops, its CPU and memory are still held on whatever sled it was placed on, and
// reopening a shape on the strength of an intention is how consolidation ends up planning a
// replacement against capacity that does not exist yet.
//
// Only shapes the release could actually have made room for are reopened. A sled that just got an 8
// CPU, 16 GiB instance back has room for something that size, and no more than that: nothing here
// knows what else was already free, so a claim that a larger shape does not fit is left standing
// until its own backoff elapses. Deleting one small instance must not reopen every shape on the rack.
//
// The refusal history is deliberately left alone. This is evidence that the shape is worth trying
// again, not evidence that it works; only a launch that succeeds is that. So the shape gets its one
// probe, and if the rack refuses it anyway the backoff picks up where it left off rather than
// restarting at three minutes.
func (c *CapacityManager) RecordCapacityReleased(ctx context.Context, released Specs) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(ctx)

	for key, entry := range c.entries {
		if entry.State != StateBackoff || !fits(entry.Specs, released) {
			continue
		}
		entry.State = StateHalfOpen
		entry.probeInFlight = false
		c.generation.Add(1)

		log.FromContext(ctx).V(1).Info(
			"capacity state changed",
			"instance-type", key,
			"from", StateBackoff,
			"to", StateHalfOpen,
			"reason", "capacity-released",
			"released", SpecsToInstanceTypeName(released.Ncpus, released.MemInGiB),
			"consecutive-ices", entry.ConsecutiveICEs,
			"backoff-remaining", entry.RetryAfter.Sub(c.now()),
		)
	}
}

// TryAcquire reserves the right to launch this shape, and reports whether the caller got it.
//
// It only ever refuses a half-open shape that another launch is already probing. Letting the rest of
// a burst through would turn one cheap probe into a wave of refusals, each one lengthening the
// backoff of a shape that may well be placeable. A caller told no should try a different shape rather
// than treat this as a capacity failure; nothing has been learned about the rack.
//
// Every acquired probe must be released, by RecordICE, by RecordSuccess, or by ReleaseProbe.
func (c *CapacityManager) TryAcquire(specs Specs) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(context.Background())

	for _, entry := range c.entries {
		if !covers(entry.Specs, specs) {
			continue
		}
		// Backed off shapes are filtered out of offerings, so reaching here means a NodeClaim built
		// before the refusal is still asking for the shape. Refuse it for the same reason it is not
		// offered.
		if entry.State == StateBackoff || entry.probeInFlight {
			return false
		}
	}
	for _, entry := range c.entries {
		if covers(entry.Specs, specs) {
			entry.probeInFlight = true
		}
	}
	return true
}

// ReleaseProbe hands back a probe acquired by TryAcquire without reporting an outcome, for a launch
// abandoned before the rack ever saw it. It is idempotent, and safe to defer alongside a RecordICE or
// RecordSuccess that has already released the probe itself.
func (c *CapacityManager) ReleaseProbe(specs Specs) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, entry := range c.entries {
		if covers(entry.Specs, specs) {
			entry.probeInFlight = false
		}
	}
}

// IsUnavailable reports whether a shape is currently ruled out, and why. A half-open shape is
// available: the whole point of reopening it is that something tries it.
func (c *CapacityManager) IsUnavailable(specs Specs) (bool, Reason) {
	return c.Availability().Unavailable(specs)
}

// Availability is everything needed to judge shapes, taken as one consistent reading.
//
// Callers judging many shapes at once, which is to say anything building the instance type list, take
// one of these rather than asking about each shape individually. A list built from a set of entries
// and a rack reading that changed halfway through would be internally inconsistent in a way nothing
// downstream could detect.
type Availability struct {
	entries []Entry
	// rack is the rack reading to judge against, and rackKnown is whether it may be believed about
	// what does not fit. See CapacityManager.rackFresh.
	rack      capacity.Snapshot
	rackKnown bool
}

// Unavailable reports whether a shape is currently ruled out, and why.
//
// The rack is asked first, and it is the only thing that can rule a shape out indefinitely: a shape
// no sled has room for is not worth offering, and no amount of waiting or probing changes that until
// the rack itself changes. This is what a fixed retry interval gets wrong, by walking a scheduler
// into the same impossible shape every few minutes for as long as the pods stay pending.
//
// It is only asked while its reading is believable. An inventory that has stopped refreshing, or was
// never switched on, says nothing at all here and the decision falls back to refusal history, which
// is exactly how the provider behaves without inventory.
func (a Availability) Unavailable(specs Specs) (bool, Reason) {
	if a.rackKnown && !a.rack.CanFit(resourcesFor(specs)) {
		return true, ReasonCapacity
	}
	return covered(a.entries, specs)
}

// Availability takes one reading of the breaker and the rack together.
func (c *CapacityManager) Availability() Availability {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(context.Background())

	entries := make([]Entry, 0, len(c.entries))
	for _, entry := range c.entries {
		entries = append(entries, *entry)
	}
	return Availability{entries: entries, rack: c.rack, rackKnown: c.rackFresh}
}

// Snapshot returns the entries the breaker currently holds, for callers reporting on its state rather
// than deciding with it.
func (c *CapacityManager) Snapshot() []Entry {
	return c.Availability().entries
}

// Generation identifies the current set of unavailable shapes. It changes whenever the set does, and
// carries no other meaning.
//
// Reading it advances the breaker's clock, because an elapsed backoff is a change to that set and
// nothing else is going to notice it: there is no timer, the state machine only moves when somebody
// looks.
func (c *CapacityManager) Generation() uint64 {
	c.mu.Lock()
	c.sweep(context.Background())
	c.mu.Unlock()
	return c.generation.Load()
}

// sweep applies everything that has changed since the last look: backoffs that have elapsed reopen,
// history nobody has refreshed in a long while is forgotten, and a rack that has been read again is
// compared against the reading before it. Callers hold the lock.
//
// It is driven by reads rather than by a timer because every one of those transitions only matters to
// a caller asking a question, and a caller is always about to ask one: the instance type list is
// rebuilt from this on every scheduling pass.
func (c *CapacityManager) sweep(ctx context.Context) {
	c.reconcileInventory(ctx)

	now := c.now()
	for key, entry := range c.entries {
		if entry.State == StateBackoff && !now.Before(entry.RetryAfter) {
			entry.State = StateHalfOpen
			entry.probeInFlight = false
			// A reopened shape is offered again, so the offerings built against the old generation no
			// longer describe what is available.
			c.generation.Add(1)
			continue
		}
		// Only ever reached by a half-open shape, since a backed off one was refused within the last
		// half hour. Dropping it changes nothing about what is available, so the generation stays put
		// and no offerings are rebuilt for it.
		if entry.State == StateHalfOpen && now.Sub(entry.LastICE) > historyTTL {
			delete(c.entries, key)
		}
	}
}

// reconcileInventory takes in a new reading of the rack and applies what it changed. Callers hold the
// lock.
//
// Two things can change. The reading itself can be replaced, which changes what shapes are worth
// offering at all and so has to invalidate anything built against the old one. And the reading can
// stop being believable, when refreshes have stopped for long enough that it no longer describes the
// rack; that flip matters just as much, because it is what releases shapes the old reading was
// suppressing. Neither is noticed by anything else, so the generation is bumped here for both.
func (c *CapacityManager) reconcileInventory(ctx context.Context) {
	if c.inventory == nil {
		return
	}

	current := c.inventory.Capacity()
	fresh := current.Fresh(c.now())
	if current.Generation() == c.rack.Generation() && fresh == c.rackFresh {
		return
	}

	previous, previouslyFresh := c.rack, c.rackFresh
	c.rack, c.rackFresh = current, fresh
	c.generation.Add(1)

	if previouslyFresh && !fresh {
		log.FromContext(ctx).Info(
			"the sled inventory has gone stale, falling back to retrying refused shapes",
			"taken-at", current.TakenAt(),
		)
	}

	// A shape is reopened by the rack gaining room it did not have, which takes two readings to see.
	// Without a previous one there is nothing to compare against, and treating that as a gain would
	// reopen every backed off shape the first time inventory is read.
	if !fresh || !previous.Known() {
		return
	}
	for key, entry := range c.entries {
		if entry.State != StateBackoff {
			continue
		}
		request := resourcesFor(entry.Specs)
		previousCount := previous.FeasibleCount(request)
		currentCount := current.FeasibleCount(request)
		// Only a rise from nothing counts. A shape that was already feasible and was refused anyway was
		// refused for something this reading cannot see, and more of the same room is not new evidence;
		// reopening on every increase would also hand a busy rack, where these counts move constantly,
		// a probe of every backed off shape on every refresh and quietly undo the backoff entirely.
		if previousCount != 0 || currentCount == 0 {
			continue
		}
		entry.State = StateHalfOpen
		entry.probeInFlight = false

		log.FromContext(ctx).V(1).Info(
			"capacity state changed",
			"instance-type", key,
			"from", StateBackoff,
			"to", StateHalfOpen,
			"reason", "inventory-capacity-increased",
			"previous-feasible", previousCount,
			"current-feasible", currentCount,
			"consecutive-ices", entry.ConsecutiveICEs,
			"backoff-remaining", entry.RetryAfter.Sub(c.now()),
		)
	}
}

// resourcesFor is what launching one instance of this shape asks a sled for.
func resourcesFor(specs Specs) capacity.Resources {
	return capacity.Resources{
		CPUs:   specs.Ncpus,
		Memory: uint64(specs.MemInGiB) * GiB,
	}
}

// backoffFor is the wait after the nth consecutive refusal: 3m, 6m, 12m, 24m, then 30m from there on,
// plus jitter.
func (c *CapacityManager) backoffFor(consecutiveICEs int) time.Duration {
	backoff := maxBackoff
	// Shifting past the cap would overflow, and anything at or beyond it is capped anyway.
	if consecutiveICEs >= 1 && consecutiveICEs <= 10 {
		backoff = min(baseBackoff<<(consecutiveICEs-1), maxBackoff)
	}
	return backoff + c.jitter(backoff)
}

// randomJitter is a delay of up to jitterFraction of the backoff, added on top of it.
func randomJitter(backoff time.Duration) time.Duration {
	return time.Duration(rand.Float64() * jitterFraction * float64(backoff))
}

// stateOrAvailable names the state a shape with no entry is in, for logging a transition out of it.
func stateOrAvailable(state State) string {
	if state == "" {
		return "available"
	}
	return string(state)
}

// covered reports whether any entry rules this shape out, and why.
//
// Only backed off entries rule anything out. A half-open one is still recorded, because its history is
// what makes the next refusal back off further, but it is deliberately offered.
//
// When entries with both reasons cover the shape, quota wins. It is the more useful thing to tell an
// operator: rack capacity is transient and Karpenter will retry into it, whereas a quota will keep
// refusing every shape this size until somebody changes it.
func covered(entries []Entry, specs Specs) (bool, Reason) {
	found := false
	reason := Reason("")
	for _, e := range entries {
		if e.State != StateBackoff || !covers(e.Specs, specs) {
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

// fits reports whether a shape would fit in the resources a released one gave back, which is the
// case when it asks for no more of either. It is the mirror of covers, and reads better at the call
// site than covers with its arguments the other way around.
func fits(specs, released Specs) bool {
	return specs.Ncpus <= released.Ncpus && specs.MemInGiB <= released.MemInGiB
}
