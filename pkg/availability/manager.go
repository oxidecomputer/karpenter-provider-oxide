// Package availability decides whether shaped launch requests are worth attempting.
package availability

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

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
	// ReasonAffinityPolicy is a shape being unavailable under one placement constraint.
	ReasonAffinityPolicy Reason = "AffinityPolicyUnsatisfiable"
)

// State is where a shape sits in the circuit breaker.
type State string

const (
	// StateBackoff is a shape whose last launch was refused and whose wait has not elapsed.
	StateBackoff State = "backoff"
	// StateHalfOpen is a shape whose wait has elapsed.
	StateHalfOpen State = "half-open"
)

// Entry is one refused shape, why it was refused, and what the breaker has learned about it.
type Entry struct {
	// Shape is the shape that was actually refused.
	Shape  Shape
	Reason Reason
	State  State

	// Reach is the smallest shape this entry takes out of consideration, which is at most Shape and may be well below it.
	Reach Shape
	// Floor is the largest shape known to have launched while this entry stood, or the zero Shape if none has.
	Floor Shape

	// ConsecutiveICEs is how long the run of refusals this entry belongs to is.
	ConsecutiveICEs int
	// LastICE is when the shape was last refused, which is what its history ages out from.
	LastICE time.Time
	// RetryAfter is when a backed off shape becomes half-open.
	RetryAfter time.Time

	probeInFlight bool
}

// Manager decides which instance shapes are worth launching globally and
// under resolved placement constraints.
type Manager struct {
	mu         sync.Mutex
	entries    map[string]*Entry
	placements map[placementEntryKey]*placementEntry

	generation atomic.Uint64

	inventory InventoryProvider

	rack      capacity.Snapshot
	rackFresh bool

	launched Shape

	now    func() time.Time
	jitter func(time.Duration) time.Duration
}

// InventoryProvider supplies rack capacity snapshots.
type InventoryProvider interface {
	// Capacity returns the most recent reading of the rack.
	Capacity() capacity.Snapshot
}

// New builds an availability manager.
func New(inventory InventoryProvider) *Manager {
	return &Manager{
		entries:    map[string]*Entry{},
		placements: map[placementEntryKey]*placementEntry{},
		inventory:  inventory,
		now:        time.Now,
		jitter:     randomJitter,
	}
}

// RecordICE takes a shape, and every shape at least as large, out of consideration.
func (c *Manager) RecordICE(ctx context.Context, specs Shape, reason Reason) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	key := shapeKey(Shape{Ncpus: specs.Ncpus, MemInGiB: specs.MemInGiB})
	entry, ok := c.entries[key]
	if !ok {
		entry = &Entry{Shape: specs}
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
		if !covers(specs, other.Shape) {
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
		floor = Shape{}
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
		entry.Floor = Shape{}
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

		"ruled-out-from", shapeKey(Shape{Ncpus: entry.Reach.Ncpus, MemInGiB: entry.Reach.MemInGiB}),
		"launchable-floor", floorName(entry.Floor),

		"already-covered-by", floorName(covering),
	)
}

// RecordSuccess reports that a launch of this shape succeeded, which disproves every claim that it could not be placed.
func (c *Manager) RecordSuccess(ctx context.Context, specs Shape) {
	c.mu.Lock()
	defer c.mu.Unlock()

	changed := 0
	cleared := 0
	for key, entry := range c.entries {
		if covers(entry.Shape, specs) {
			delete(c.entries, key)
			cleared++
			changed++
			log.FromContext(ctx).V(1).Info(
				"capacity state changed",
				"instance-type", key,
				"from", entry.State,
				"to", "available",
				"reason", "launch-succeeded",
				"launched", shapeKey(Shape{Ncpus: specs.Ncpus, MemInGiB: specs.MemInGiB}),
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
		reach := between(specs, entry.Shape)
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
			"launched", shapeKey(Shape{Ncpus: specs.Ncpus, MemInGiB: specs.MemInGiB}),
			"ruled-out-from", shapeKey(Shape{Ncpus: reach.Ncpus, MemInGiB: reach.MemInGiB}),
			"was-ruled-out-from", shapeKey(Shape{Ncpus: previous.Ncpus, MemInGiB: previous.MemInGiB}),
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
func (c *Manager) SeedLaunchable(ctx context.Context, specs Shape) {
	if !known(specs) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if known(c.launched) && shapeSize(c.launched) >= shapeSize(specs) {
		return
	}
	previous := c.launched
	c.launched = specs

	log.FromContext(ctx).V(1).Info(
		"recorded a running shape as the launchable floor",
		"instance-type", shapeKey(Shape{Ncpus: specs.Ncpus, MemInGiB: specs.MemInGiB}),
		"previous", floorName(previous),
	)
}

// RecordCapacityReleased reports that the rack has taken back the resources of an instance of this shape.
func (c *Manager) RecordCapacityReleased(ctx context.Context, released Shape) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(ctx)

	for key, entry := range c.entries {
		if entry.State != StateBackoff || !fits(entry.Shape, released) {
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
			"released", shapeKey(Shape{Ncpus: released.Ncpus, MemInGiB: released.MemInGiB}),
			"consecutive-ices", entry.ConsecutiveICEs,
			"backoff-remaining", entry.RetryAfter.Sub(c.now()),
		)
	}
}

// Snapshot is everything needed to judge shapes, taken as one consistent reading.
type Snapshot struct {
	entries   []Entry
	rack      capacity.Snapshot
	inventory InventoryState
}

// Entries returns the refusals the breaker currently holds.
func (s Snapshot) Entries() []Entry {
	return s.entries
}

// Rack returns the rack reading the snapshot judged against, and what that reading is worth.
func (s Snapshot) Rack() (capacity.Snapshot, InventoryState) {
	return s.rack, s.inventory
}

// Unavailable reports whether a shape is ruled out in this reading, and why.
func (s Snapshot) Unavailable(specs Shape) (bool, Reason) {
	if s.inventory == InventoryFresh && !s.rack.CanFit(resourcesFor(specs)) {
		return true, ReasonCapacity
	}
	return covered(s.entries, specs)
}

// Snapshot takes one reading of the breaker and the rack together.
func (c *Manager) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(context.Background())

	entries := make([]Entry, 0, len(c.entries))
	for _, entry := range c.entries {
		entries = append(entries, *entry)
	}
	return Snapshot{entries: entries, rack: c.rack, inventory: c.inventoryState()}
}

func (c *Manager) inventoryState() InventoryState {
	switch {
	case c.inventory == nil:
		return InventoryDisabled
	case !c.rack.Known():
		return InventoryUnknown
	case !c.rackFresh:
		return InventoryStale
	default:
		return InventoryFresh
	}
}

// InventoryState is what the sled inventory is contributing to the manager's decisions.
type InventoryState string

const (
	// InventoryDisabled is inventory not being switched on at all.
	InventoryDisabled InventoryState = "Disabled"
	// InventoryUnknown is enabled inventory without a successful reading.
	InventoryUnknown InventoryState = "Unknown"
	// InventoryStale is a reading too old to be believed, which is ignored.
	InventoryStale InventoryState = "Stale"
	// InventoryFresh is a current reading.
	InventoryFresh InventoryState = "Fresh"
)

// Generation identifies the current set of unavailable shapes.
func (c *Manager) Generation() uint64 {
	c.mu.Lock()
	c.sweep(context.Background())
	c.mu.Unlock()
	return c.generation.Load()
}

func (c *Manager) sweep(ctx context.Context) {
	c.reconcileInventory(ctx)

	now := c.now()

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

func (c *Manager) reconcileInventory(ctx context.Context) {
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
		request := resourcesFor(entry.Shape)
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

func resourcesFor(specs Shape) capacity.Resources {
	return capacity.Resources{
		CPUs:   specs.Ncpus,
		Memory: uint64(specs.MemInGiB) * GiB,
	}
}

func (c *Manager) backoffFor(consecutiveICEs int) time.Duration {
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

func covered(entries []Entry, specs Shape) (bool, Reason) {
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

func covers(marked, specs Shape) bool {
	return specs.Ncpus >= marked.Ncpus && specs.MemInGiB >= marked.MemInGiB
}

func fits(specs, released Shape) bool {
	return specs.Ncpus <= released.Ncpus && specs.MemInGiB <= released.MemInGiB
}

func (c *Manager) coveredByOther(specs Shape) (Shape, bool) {
	for _, entry := range c.entries {
		if entry.Shape == specs {
			continue
		}
		if covers(entry.Shape, specs) {
			return entry.Shape, true
		}
	}
	return Shape{}, false
}

func (e Entry) reachOrSpecs() Shape {
	if !known(e.Reach) {
		return e.Shape
	}
	return e.Reach
}

func reachFor(specs, floor Shape, run int) Shape {
	if known(floor) {
		return between(floor, specs)
	}
	if run < walkThreshold {
		return specs
	}
	return halved(specs)
}

func halved(specs Shape) Shape {
	next := Shape{
		Ncpus:    max(specs.Ncpus/2, minSpeculativeCPUs),
		MemInGiB: max(specs.MemInGiB/2, minSpeculativeCPUs),
	}
	return Shape{
		Ncpus:    min(next.Ncpus, specs.Ncpus),
		MemInGiB: min(next.MemInGiB, specs.MemInGiB),
	}
}

func between(floor, refused Shape) Shape {
	midpoint := Shape{
		Ncpus:    (floor.Ncpus + refused.Ncpus) / 2,
		MemInGiB: (floor.MemInGiB + refused.MemInGiB) / 2,
	}
	return Shape{
		Ncpus:    min(max(midpoint.Ncpus+1, floor.Ncpus+1), refused.Ncpus),
		MemInGiB: min(max(midpoint.MemInGiB+1, floor.MemInGiB+1), refused.MemInGiB),
	}
}

func known(specs Shape) bool {
	return specs.Ncpus > 0 && specs.MemInGiB > 0
}

func shapeSize(shape Shape) float64 {
	return float64(shape.Ncpus) + 0.125*float64(shape.MemInGiB)
}

func atLeast(a, b Shape) bool {
	return covers(b, a)
}

func floorName(floor Shape) string {
	if !known(floor) {
		return "none"
	}
	return shapeKey(Shape{Ncpus: floor.Ncpus, MemInGiB: floor.MemInGiB})
}
