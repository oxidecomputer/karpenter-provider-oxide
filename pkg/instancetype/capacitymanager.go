package instancetype

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
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

	// walkThreshold is how many refusals in a row establish that the scheduler is walking down
	// through shapes rather than having run into one bad size.
	//
	// A rack with no room does not refuse one shape. It refuses the largest the scheduler proposes,
	// the scheduler comes back one size smaller, and that is refused too, each attempt costing a
	// launch. Walking a rack that is out of room from 38 CPUs to 2 takes 36 launches and the better
	// part of twenty minutes, and every one of them was predictable from the refusal before it.
	//
	// So a walk is answered by skipping rather than stepping. The first refusal is taken at face
	// value, because a single one genuinely says nothing about the shape one size down; the second
	// establishes a trend, and from there the search descends geometrically. See Entry.Reach.
	walkThreshold = 2
	// minSpeculativeCPUs is the smallest CPU count a skip is allowed to rule out.
	//
	// A skip claims shapes are unavailable that nothing has refused, which is a guess, and a guess
	// that ruled out everything would stop the cluster scaling on no evidence at all. The provider
	// generates shapes from one CPU up, so holding the floor here at two guarantees a shape is
	// always left offerable no matter how far a walk has run: speculation may narrow the search but
	// may never be the reason nothing can launch.
	minSpeculativeCPUs = 2
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

// StorageRefusal is the most recent launch refused because the boot disk could not be provisioned.
//
// It is not an Entry and is not keyed by shape, because a storage refusal is not about a shape. The
// boot disk size comes from the OxideNodeClass, and Crucible places a disk's regions on three sleds
// chosen independently of wherever the instance lands, so nothing about the shape being launched had
// any bearing on the refusal and no other shape would have fared better.
//
// Which is also why this changes no decision. Nothing is filtered on it and no backoff is computed
// from it: it exists so that an operator whose nodes have stopped launching can see that storage is
// why, rather than reading it out of the logs. The provider still learns about storage the only way
// it can, by trying.
type StorageRefusal struct {
	// Reason is which limit was diagnosed as binding, on the same terms as a shape's: ReasonQuota for
	// the silo's storage entitlement, ReasonCapacity for the rack having nowhere to put the regions.
	Reason Reason
	// Consecutive is how many launches in a row have been refused this way. Any launch that succeeds
	// clears it, since every launch creates a boot disk and so proves storage was available.
	Consecutive int
	// LastRefusal is when the most recent one happened, and is what this ages out from.
	LastRefusal time.Time
	// Message is the diagnosis as it was reported on the NodeClaim, kept so the published view can say
	// what the limit actually was rather than only naming it.
	Message string
}

// Entry is one refused shape, why it was refused, and what the breaker has learned about it.
type Entry struct {
	// Specs is the shape that was actually refused. It is evidence, and only a launch that succeeds
	// contradicts it.
	Specs  Specs
	Reason Reason
	State  State

	// Reach is the smallest shape this entry takes out of consideration, which is at most Specs and
	// may be well below it. It is where the search for a launchable shape is currently looking, and
	// unlike Specs it is a guess rather than something the rack said.
	//
	// The two differ because stepping down one size at a time is far too slow to be useful. Once a
	// walk is established the reach halves, so the scheduler is offered roughly half of what was
	// just refused instead of one size less, and a rack that is out of room is found out in a
	// handful of launches rather than dozens. See walkThreshold.
	//
	// Halving overshoots on purpose, and Floor is what corrects it. Between the two the entry is a
	// search for the largest launchable shape: the reach descends geometrically while nothing
	// launches, and climbs to the midpoint of what is known once something does, which converges on
	// the boundary from both sides instead of crawling towards it from one.
	//
	// It is never allowed to rule out everything. See minSpeculativeCPUs.
	Reach Specs
	// Floor is the largest shape known to have launched while this entry stood, or the zero Specs if
	// none has. It is the bottom of the search: nothing at or below it is worth ruling out, because
	// the rack has demonstrably placed it.
	Floor Specs

	// ConsecutiveICEs is how long the run of refusals this entry belongs to is. It is what the backoff
	// is computed from, and only an actual refusal moves it: looking a shape up, or waiting out its
	// backoff, teaches the breaker nothing.
	//
	// Usually that run is this shape's own refusals, but not always. An entry that subsumes others
	// inherits the longest run among them, because a rack refusing ever smaller shapes is one run of
	// refusals spread across several keys rather than a series of unrelated first refusals. See
	// RecordICE.
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
	// storage is the last launch refused for want of boot disk storage, or nil if none has been since
	// the last successful launch. It sits alongside the shape entries rather than among them because
	// it is not about a shape, and it never moves the generation: no offering is filtered on it, so
	// rebuilding them for it would be work with no effect.
	storage *StorageRefusal
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

	// launched is the largest shape the cluster is known to be running, seeded from nodes that are
	// already up rather than from a launch this process made. It is the floor a refusal falls back on
	// when the entry it lands on has none of its own.
	//
	// An entry's own Floor is set by RecordSuccess and so only exists once this process has launched
	// something. Across a restart that is nothing at all, which leaves the first refusal to descend by
	// halving -- speculation that can rule out a shape the cluster is visibly running. A node that is
	// up is proof its shape placed, and unlike RecordSuccess's proof it survives a restart.
	launched Specs

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
// Entries the new one subsumes are dropped, since they no longer describe anything it does not, but
// their refusal history is carried over to it rather than deleted with them. Subsumption ignores the
// reason, because coverage is what an offering is filtered on; a smaller shape refused for quota
// genuinely does explain a larger one refused for capacity.
//
// A refusal of a shape some smaller entry already covers is recorded, but does not search. See
// coveredByOther.
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

	// The longest run of refusals this one continues, which is not necessarily this entry's own.
	//
	// A rack with no room does not refuse the same shape over and over. It refuses the largest shape
	// the scheduler proposes, and the scheduler comes back with a smaller one, which is a different
	// key: nine refusals in a row can be nine first refusals, each starting the backoff again at three
	// minutes while the rack stays exactly as full as it was. Counting only this entry's history is
	// what makes that walk cost a launch every three minutes indefinitely.
	//
	// So the run carries across subsumption. A subsumed entry described this same rack refusing this
	// same series of launches, and the shape replacing it is smaller, which is worse news rather than a
	// fresh start. The longest run is taken rather than their sum, since these are one series of
	// refusals seen through several keys and not several independent ones.
	run := entry.ConsecutiveICEs
	// The largest shape known to launch, which the search descends towards and must never rule out.
	// It carries across subsumption for the same reason the run does: a subsumed entry was watching
	// this same rack, and what it saw launch is still something that launched.
	floor := entry.Floor
	subsumed := 0
	for existing, other := range c.entries {
		if existing == key {
			continue
		}
		if !covers(specs, other.Specs) {
			continue
		}
		run = max(run, other.ConsecutiveICEs)
		if known(other.Floor) && (!known(floor) || atLeast(other.Floor, floor)) {
			floor = other.Floor
		}
		subsumed++
		delete(c.entries, existing)
	}
	// An entry with no floor of its own falls back on what the cluster is running. Without it the
	// search descends by halving, which is a guess, and a node that is up is evidence. This is the
	// only floor a process that has launched nothing yet can have.
	if !known(floor) {
		floor = c.launched
	}
	// A floor at or above the shape just refused describes a rack that has changed since, and using
	// it would put the search entirely above a shape known not to fit. The refusal is the newer
	// evidence, so the floor is dropped rather than believed.
	if known(floor) && atLeast(floor, specs) {
		floor = Specs{}
	}

	entry.Reason = reason
	entry.ConsecutiveICEs = run + 1
	entry.LastICE = now
	entry.State = StateBackoff
	entry.RetryAfter = now.Add(c.backoffFor(entry.ConsecutiveICEs))
	entry.probeInFlight = false
	entry.Floor = floor
	entry.Reach = reachFor(specs, floor, entry.ConsecutiveICEs)
	// Checked after subsumption, which has already removed every entry larger than this one, so the
	// only thing that can still cover it is something smaller: an entry that is by definition further
	// along the same search and does not need a second, coarser one running underneath it.
	covering, coveredByOther := c.coveredByOther(specs)
	if coveredByOther {
		entry.Reach = specs
		entry.Floor = Specs{}
	}

	c.generation.Add(1)
	metrics.ICEs.WithLabelValues(string(reason)).Inc()

	log.FromContext(ctx).V(1).Info(
		"capacity state changed",
		"instance-type", key,
		"from", stateOrAvailable(from),
		"to", StateBackoff,
		"reason", reason,
		"consecutive-ices", entry.ConsecutiveICEs,
		"subsumed-shapes", subsumed,
		"backoff", entry.RetryAfter.Sub(now),
		// What the scheduler is actually offered next, which during a walk is well below the shape
		// that was refused. Logged separately because it is the entry's guess rather than evidence,
		// and an operator reading "38-76 refused" needs to see that 18-37 is also out as a result.
		"ruled-out-from", SpecsToInstanceTypeName(entry.Reach.Ncpus, entry.Reach.MemInGiB),
		"launchable-floor", floorName(entry.Floor),
		// Empty unless a smaller entry already covers this shape, in which case this refusal arrived
		// out of order from a launch that was in flight, and this entry deliberately does not search.
		"already-covered-by", floorName(covering),
	)
}

// RecordSuccess reports that a launch of this shape succeeded, which disproves every claim that it
// could not be placed.
//
// That is more than the shape's own entry. An entry for a smaller shape asserts that nothing this
// size or larger fits, and a launch of this shape has just shown otherwise, so those entries are
// wrong about the rack as it stands now and are dropped along with their history. Entries for larger
// shapes are left alone: placing 8 CPUs says nothing about whether 64 fit.
//
// Only Specs clears an entry, never Reach. Specs is what the rack refused and a launch of something
// larger genuinely contradicts it; the reach is this entry's own guess about where to look next, and
// letting a launch below it delete the entry would throw away a real refusal on the strength of a
// speculative skip having been right about the small end.
//
// What a launch below the reach does instead is narrow the search. It puts a floor under it -- this
// shape launches -- and moves the reach up to the midpoint between that floor and the shape that was
// actually refused, so the next launch tests the middle of the untested band rather than crawling up
// from the bottom. Descending halves and climbing to midpoints is a search from both sides, which is
// what keeps an overshooting skip from leaving the cluster on the smallest shape on the rack.
func (c *CapacityManager) RecordSuccess(ctx context.Context, specs Specs) {
	c.mu.Lock()
	defer c.mu.Unlock()

	changed := 0
	cleared := 0
	for key, entry := range c.entries {
		if covers(entry.Specs, specs) {
			delete(c.entries, key)
			cleared++
			changed++
			log.FromContext(ctx).V(1).Info(
				"capacity state changed",
				"instance-type", key,
				"from", entry.State,
				"to", "available",
				"reason", "launch-succeeded",
				"launched", SpecsToInstanceTypeName(specs.Ncpus, specs.MemInGiB),
				"consecutive-ices", entry.ConsecutiveICEs,
			)
			continue
		}

		// Only a launch this entry was allowing teaches it anything. One at or above the reach is a
		// NodeClaim built before the skip, and while it is welcome news it says nothing about where
		// the boundary is that the entry was not already assuming.
		if covers(entry.Reach, specs) {
			continue
		}
		if known(entry.Floor) && !atLeast(specs, entry.Floor) {
			continue
		}
		reach := between(specs, entry.Specs)
		if reach == entry.Reach {
			// Already converged, or this launch was no larger than what the search had assumed.
			entry.Floor = specs
			continue
		}
		previous := entry.Reach
		entry.Floor = specs
		entry.Reach = reach
		changed++

		log.FromContext(ctx).V(1).Info(
			"narrowing the search for a launchable shape",
			"instance-type", key,
			"reason", "launch-succeeded",
			"launched", SpecsToInstanceTypeName(specs.Ncpus, specs.MemInGiB),
			"ruled-out-from", SpecsToInstanceTypeName(reach.Ncpus, reach.MemInGiB),
			"was-ruled-out-from", SpecsToInstanceTypeName(previous.Ncpus, previous.MemInGiB),
			"refused-shape", key,
		)
	}
	if cleared > 0 {
		metrics.Successes.Inc()
	}
	if changed > 0 {
		c.generation.Add(1)
	}
}

// SeedLaunchable reports that the cluster is running a node of this shape, which is proof the rack
// placed it. The largest such shape becomes the floor a refusal falls back on when the entry it lands
// on has no floor of its own.
//
// Only the largest is kept, since a smaller running shape says nothing a larger one does not. It is
// never used in place of an entry's own Floor: that one was established by a launch during the
// entry's life and is the newer evidence.
//
// Seeding from a node that has been up for hours is deliberately weaker evidence than a fresh launch,
// and it is allowed to be wrong. A rack that has tightened since will refuse the shape the bracketed
// search proposes, and that refusal drops the floor and resumes the descent. The failure mode it
// removes is worse than the one it risks: without a floor, one refusal of a mid-size shape rules out
// every shape at or above half of it, including ones the cluster is visibly running.
func (c *CapacityManager) SeedLaunchable(ctx context.Context, specs Specs) {
	if !known(specs) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if known(c.launched) && price(c.launched) >= price(specs) {
		return
	}
	previous := c.launched
	c.launched = specs

	log.FromContext(ctx).V(1).Info(
		"recorded a running shape as the launchable floor",
		"instance-type", SpecsToInstanceTypeName(specs.Ncpus, specs.MemInGiB),
		"previous", floorName(previous),
	)
}

// RecordStorageRefusal reports that a launch was refused because its boot disk could not be
// provisioned. reason is the caller's diagnosis of which limit was binding, and message is what it
// reported on the NodeClaim.
//
// No shape is taken out of consideration, deliberately. See StorageRefusal for why. This only
// records, so that the published view can say storage is the problem instead of leaving an operator
// to infer it from every shape failing at once.
func (c *CapacityManager) RecordStorageRefusal(ctx context.Context, reason Reason, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.storage == nil {
		c.storage = &StorageRefusal{}
	}
	c.storage.Reason = reason
	c.storage.Consecutive++
	c.storage.LastRefusal = c.now()
	c.storage.Message = message

	metrics.StorageRefusals.WithLabelValues(string(reason)).Inc()

	log.FromContext(ctx).V(1).Info(
		"launch refused for want of boot disk storage",
		"reason", reason,
		"consecutive-refusals", c.storage.Consecutive,
		"diagnosis", message,
	)
}

// ClearStorageRefusal reports that a launch created its boot disk, which is every launch that
// succeeds. Storage was available, so whatever was recorded about it no longer describes anything.
func (c *CapacityManager) ClearStorageRefusal(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.storage == nil {
		return
	}
	log.FromContext(ctx).V(1).Info(
		"boot disk storage is available again",
		"reason", "launch-succeeded",
		"consecutive-refusals", c.storage.Consecutive,
	)
	c.storage = nil
}

// StorageRefusal returns the last storage refusal, and whether there is one to report.
func (c *CapacityManager) StorageRefusal() (StorageRefusal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.storage == nil {
		return StorageRefusal{}, false
	}
	return *c.storage, true
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
		metrics.Reopens.WithLabelValues(metrics.ReopenCapacityReleased).Inc()

		log.FromContext(ctx).V(1).Info(
			"capacity state changed",
			"instance-type", key,
			"from", StateBackoff,
			"to", StateHalfOpen,
			"reason", metrics.ReopenCapacityReleased,
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

	// Whether this launch is testing a claim at all, as opposed to launching a shape nothing has ever
	// refused. Only the former is a probe worth counting: every launch passes through here, and
	// counting all of them would bury the interlock's behaviour under ordinary provisioning.
	probing := false
	for _, entry := range c.entries {
		if !covers(entry.Reach, specs) {
			continue
		}
		probing = true
		// Backed off shapes are filtered out of offerings, so reaching here means a NodeClaim built
		// before the refusal is still asking for the shape. Refuse it for the same reason it is not
		// offered.
		if entry.State == StateBackoff || entry.probeInFlight {
			metrics.Probes.WithLabelValues(metrics.ProbeRejected).Inc()
			return false
		}
	}
	for _, entry := range c.entries {
		if covers(entry.Reach, specs) {
			entry.probeInFlight = true
		}
	}
	if probing {
		metrics.Probes.WithLabelValues(metrics.ProbeAcquired).Inc()
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
		if covers(entry.Reach, specs) {
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

// InventoryState is what the sled inventory is contributing to the manager's decisions. The four
// states are distinguished because only one of them lets the rack rule a shape out, and an operator
// looking at a shape that is out needs to know which of the other three they are in.
type InventoryState string

const (
	// InventoryDisabled is inventory not being switched on at all.
	InventoryDisabled InventoryState = v1alpha1.InventoryDisabled
	// InventoryUnknown is inventory being enabled but never yet read successfully.
	InventoryUnknown InventoryState = v1alpha1.InventoryUnknown
	// InventoryStale is a reading too old to be believed, which is ignored.
	InventoryStale InventoryState = v1alpha1.InventoryStale
	// InventoryFresh is a current reading.
	InventoryFresh InventoryState = v1alpha1.InventoryFresh
)

// Rack returns the rack reading the manager is deciding against, and what that reading is worth.
func (c *CapacityManager) Rack() (capacity.Snapshot, InventoryState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(context.Background())

	switch {
	case c.inventory == nil:
		return capacity.Snapshot{}, InventoryDisabled
	case !c.rack.Known():
		return c.rack, InventoryUnknown
	case !c.rackFresh:
		return c.rack, InventoryStale
	default:
		return c.rack, InventoryFresh
	}
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

	// Aged out on the same TTL as a shape's history, and for the same reason: a storage refusal from
	// six hours ago describes a rack that may have been emptied since, and nothing clears it except a
	// launch that succeeds. A cluster that stopped scaling for unrelated reasons would otherwise keep
	// reporting storage as the problem indefinitely.
	if c.storage != nil && now.Sub(c.storage.LastRefusal) > historyTTL {
		c.storage = nil
	}

	for key, entry := range c.entries {
		if entry.State == StateBackoff && !now.Before(entry.RetryAfter) {
			entry.State = StateHalfOpen
			entry.probeInFlight = false
			metrics.Reopens.WithLabelValues(metrics.ReopenTimeout).Inc()
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
		metrics.Reopens.WithLabelValues(metrics.ReopenInventory).Inc()

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
		if e.State != StateBackoff || !covers(e.reachOrSpecs(), specs) {
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

// coveredByOther reports whether some entry for a strictly smaller shape already rules this one out,
// and names it. Callers hold the lock.
//
// It happens because a launch already in flight when a shape was refused still runs, and still
// fails, so refusals arrive out of order: 11 CPUs is refused, and five seconds later the 12 CPU
// launch that had already started fails too. The refusal is real and worth keeping -- if the 11 CPU
// entry is later disproven by a launch, the 12 CPU one is all that still rules out anything larger.
//
// What it must not do is search. Two entries searching the same rack is one too many, and the larger
// one searches worse: its reach descends from a shape further from the boundary, so it lands lower,
// and a reach that extends below a smaller entry rules that entry's own shape out. In the case that
// found this, an 11-22 entry was reopened for a probe by a teardown that freed capacity, while a
// 12-24 entry recorded five seconds later sat in a backoff whose reach had descended to 6-12 and
// went on covering 11-22. The probe was silently dead: nothing could launch it, nothing retried it,
// and neither the NodeClaim nor the published view said why.
//
// So the entry keeps its refusal and rules out exactly what was refused, nothing more. The smaller
// entry is further along the same search and is the one that gets to run it.
func (c *CapacityManager) coveredByOther(specs Specs) (Specs, bool) {
	for _, entry := range c.entries {
		if entry.Specs == specs {
			continue
		}
		if covers(entry.Specs, specs) {
			return entry.Specs, true
		}
	}
	return Specs{}, false
}

// reachOrSpecs is where this entry starts ruling shapes out. An entry with no reach set rules out
// exactly what was refused, which is what every entry did before the search existed and is the
// answer that assumes least.
func (e Entry) reachOrSpecs() Specs {
	if !known(e.Reach) {
		return e.Specs
	}
	return e.Reach
}

// reachFor is where an entry should start ruling shapes out, given what was refused, the largest
// shape known to launch, and how long the run of refusals is.
//
// With a floor the search is bracketed and the answer is the midpoint: both ends are known, so
// halving the remaining band is what converges fastest. Without one there is no upper bound on how
// far down the boundary lies, so the descent is geometric until something launches and provides one.
// A single refusal gets neither -- one is not a walk, and skipping on the strength of it would rule
// out shapes that were about to work.
func reachFor(specs, floor Specs, run int) Specs {
	if known(floor) {
		return between(floor, specs)
	}
	if run < walkThreshold {
		return specs
	}
	return halved(specs)
}

// halved is the shape to look at next when a walk is under way and nothing is known to launch yet.
// It never descends past minSpeculativeCPUs, and never exceeds what was refused.
func halved(specs Specs) Specs {
	next := Specs{
		Ncpus:    max(specs.Ncpus/2, minSpeculativeCPUs),
		MemInGiB: max(specs.MemInGiB/2, minSpeculativeCPUs),
	}
	return Specs{
		Ncpus:    min(next.Ncpus, specs.Ncpus),
		MemInGiB: min(next.MemInGiB, specs.MemInGiB),
	}
}

// between is the reach of a bracketed search: it puts the midpoint of the untested band -- between
// the largest shape known to launch and the smallest known to have been refused -- at the top of
// what is still offered, so that the midpoint is what gets tried next.
//
// The reach sits one step above that midpoint rather than on it, because the reach is where ruling
// out begins. Landing it on the midpoint would exclude the very shape the search wants to test, and
// the search would keep re-testing the floor and never move.
//
// It is clamped into (floor, refused]: never at or below the floor, which would rule out a shape
// known to launch, and never above the refusal, which is evidence and stands regardless. A band with
// nothing left in it converges on the refusal, which leaves the floor as the largest offered shape
// and is the correct answer once the boundary has been found.
func between(floor, refused Specs) Specs {
	midpoint := Specs{
		Ncpus:    (floor.Ncpus + refused.Ncpus) / 2,
		MemInGiB: (floor.MemInGiB + refused.MemInGiB) / 2,
	}
	return Specs{
		Ncpus:    min(max(midpoint.Ncpus+1, floor.Ncpus+1), refused.Ncpus),
		MemInGiB: min(max(midpoint.MemInGiB+1, floor.MemInGiB+1), refused.MemInGiB),
	}
}

// known reports whether a shape has been set at all, as opposed to being the zero value standing in
// for "no such shape".
func known(specs Specs) bool {
	return specs.Ncpus > 0 && specs.MemInGiB > 0
}

// atLeast reports whether a asks for at least as much as b of both resources.
func atLeast(a, b Specs) bool {
	return covers(b, a)
}

// floorName names a search floor for a log line, including the case of there not being one yet.
func floorName(floor Specs) string {
	if !known(floor) {
		return "none"
	}
	return SpecsToInstanceTypeName(floor.Ncpus, floor.MemInGiB)
}
