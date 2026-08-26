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
	baseBackoff = 3 * time.Minute

	maxBackoff = 30 * time.Minute

	jitterFraction = 0.2

	historyTTL = 6 * time.Hour

	walkThreshold = 2

	minSpeculativeCPUs = 2
)

// Reason is why a shape is out of consideration.
type Reason string

const (
	// ReasonCapacity is the rack having no sled with room for the shape.
	ReasonCapacity Reason = "InsufficientCapacity"
	// ReasonQuota is the silo's quota having no room for the shape.
	ReasonQuota Reason = "QuotaExceeded"
)

// State is where a shape sits in the circuit breaker.
type State string

const (
	// StateBackoff is a shape whose last launch was refused and whose wait has not elapsed.
	StateBackoff State = "backoff"
	// StateHalfOpen is a shape whose wait has elapsed.
	StateHalfOpen State = "half-open"
)

// StorageRefusal is the most recent launch refused because the boot disk could not be provisioned.
type StorageRefusal struct {
	// Reason is which limit was diagnosed as binding, on the same terms as a shape's.
	Reason Reason
	// Consecutive is how many launches in a row have been refused this way.
	Consecutive int
	// LastRefusal is when the most recent one happened, and is what this ages out from.
	LastRefusal time.Time
	// Message is the diagnosis as it was reported on the NodeClaim.
	Message string
}

// Entry is one refused shape, why it was refused, and what the breaker has learned about it.
type Entry struct {
	// Specs is the shape that was actually refused.
	Specs  Specs
	Reason Reason
	State  State

	// Reach is the smallest shape this entry takes out of consideration, which is at most Specs and may be well below it.
	Reach Specs
	// Floor is the largest shape known to have launched while this entry stood, or the zero Specs if none has.
	Floor Specs

	// ConsecutiveICEs is how long the run of refusals this entry belongs to is.
	ConsecutiveICEs int
	// LastICE is when the shape was last refused, which is what its history ages out from.
	LastICE time.Time
	// RetryAfter is when a backed off shape becomes half-open.
	RetryAfter time.Time

	probeInFlight bool
}

// CapacityManager decides which instance shapes are worth launching, keyed by CPU count and memory.
type CapacityManager struct {
	mu      sync.Mutex
	entries map[string]*Entry

	storage *StorageRefusal

	generation atomic.Uint64

	inventory InventoryProvider

	rack      capacity.Snapshot
	rackFresh bool

	launched Specs

	now    func() time.Time
	jitter func(time.Duration) time.Duration
}

// InventoryProvider supplies rack capacity snapshots.
type InventoryProvider interface {
	// Capacity returns the most recent reading of the rack.
	Capacity() capacity.Snapshot
}

// NewCapacityManager builds a capacity manager.
func NewCapacityManager(inventory InventoryProvider) *CapacityManager {
	return &CapacityManager{
		entries:   map[string]*Entry{},
		inventory: inventory,
		now:       time.Now,
		jitter:    randomJitter,
	}
}

// RecordICE takes a shape, and every shape at least as large, out of consideration.
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

	run := entry.ConsecutiveICEs

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

	if !known(floor) {
		floor = c.launched
	}

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

		"ruled-out-from", SpecsToInstanceTypeName(entry.Reach.Ncpus, entry.Reach.MemInGiB),
		"launchable-floor", floorName(entry.Floor),

		"already-covered-by", floorName(covering),
	)
}

// RecordSuccess reports that a launch of this shape succeeded, which disproves every claim that it could not be placed.
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

		if covers(entry.Reach, specs) {
			continue
		}
		if known(entry.Floor) && !atLeast(specs, entry.Floor) {
			continue
		}
		reach := between(specs, entry.Specs)
		if reach == entry.Reach {

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

// SeedLaunchable reports that the cluster is running a node of this shape, which is proof the rack placed it.
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

// RecordStorageRefusal reports that a launch was refused because its boot disk could not be provisioned.
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

// ClearStorageRefusal reports that a launch created its boot disk, which is every launch that succeeds.
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

// RecordCapacityReleased reports that the rack has taken back the resources of an instance of this shape.
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
func (c *CapacityManager) TryAcquire(specs Specs) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(context.Background())

	probing := false
	for _, entry := range c.entries {
		if !covers(entry.Reach, specs) {
			continue
		}
		probing = true

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

// ReleaseProbe hands back a probe acquired by TryAcquire without reporting an outcome.
func (c *CapacityManager) ReleaseProbe(specs Specs) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, entry := range c.entries {
		if covers(entry.Reach, specs) {
			entry.probeInFlight = false
		}
	}
}

// IsUnavailable reports whether a shape is currently ruled out, and why.
func (c *CapacityManager) IsUnavailable(specs Specs) (bool, Reason) {
	return c.Availability().Unavailable(specs)
}

// Availability is everything needed to judge shapes, taken as one consistent reading.
type Availability struct {
	entries []Entry

	rack      capacity.Snapshot
	rackKnown bool
}

// Unavailable reports whether a shape is currently ruled out, and why.
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

// Snapshot returns the entries the breaker currently holds.
func (c *CapacityManager) Snapshot() []Entry {
	return c.Availability().entries
}

// InventoryState is what the sled inventory is contributing to the manager's decisions.
type InventoryState string

const (
	// InventoryDisabled is inventory not being switched on at all.
	InventoryDisabled InventoryState = v1alpha1.InventoryDisabled
	// InventoryUnknown is enabled inventory without a successful reading.
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

// Generation identifies the current set of unavailable shapes.
func (c *CapacityManager) Generation() uint64 {
	c.mu.Lock()
	c.sweep(context.Background())
	c.mu.Unlock()
	return c.generation.Load()
}

func (c *CapacityManager) sweep(ctx context.Context) {
	c.reconcileInventory(ctx)

	now := c.now()

	if c.storage != nil && now.Sub(c.storage.LastRefusal) > historyTTL {
		c.storage = nil
	}

	for key, entry := range c.entries {
		if entry.State == StateBackoff && !now.Before(entry.RetryAfter) {
			entry.State = StateHalfOpen
			entry.probeInFlight = false
			metrics.Reopens.WithLabelValues(metrics.ReopenTimeout).Inc()

			c.generation.Add(1)
			continue
		}

		if entry.State == StateHalfOpen && now.Sub(entry.LastICE) > historyTTL {
			delete(c.entries, key)
		}
	}
}

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

func resourcesFor(specs Specs) capacity.Resources {
	return capacity.Resources{
		CPUs:   specs.Ncpus,
		Memory: uint64(specs.MemInGiB) * GiB,
	}
}

func (c *CapacityManager) backoffFor(consecutiveICEs int) time.Duration {
	backoff := maxBackoff

	if consecutiveICEs >= 1 && consecutiveICEs <= 10 {
		backoff = min(baseBackoff<<(consecutiveICEs-1), maxBackoff)
	}
	return backoff + c.jitter(backoff)
}

func randomJitter(backoff time.Duration) time.Duration {
	return time.Duration(rand.Float64() * jitterFraction * float64(backoff))
}

func stateOrAvailable(state State) string {
	if state == "" {
		return "available"
	}
	return string(state)
}

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

func covers(marked, specs Specs) bool {
	return specs.Ncpus >= marked.Ncpus && specs.MemInGiB >= marked.MemInGiB
}

func fits(specs, released Specs) bool {
	return specs.Ncpus <= released.Ncpus && specs.MemInGiB <= released.MemInGiB
}

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

func (e Entry) reachOrSpecs() Specs {
	if !known(e.Reach) {
		return e.Specs
	}
	return e.Reach
}

func reachFor(specs, floor Specs, run int) Specs {
	if known(floor) {
		return between(floor, specs)
	}
	if run < walkThreshold {
		return specs
	}
	return halved(specs)
}

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

func known(specs Specs) bool {
	return specs.Ncpus > 0 && specs.MemInGiB > 0
}

func atLeast(a, b Specs) bool {
	return covers(b, a)
}

func floorName(floor Specs) string {
	if !known(floor) {
		return "none"
	}
	return SpecsToInstanceTypeName(floor.Ncpus, floor.MemInGiB)
}
