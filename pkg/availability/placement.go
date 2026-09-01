package availability

import (
	"context"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	placementBaseBackoff = 30 * time.Second
	placementMaxBackoff  = 5 * time.Minute
	placementGateAfter   = 2
	placementHistoryTTL  = time.Hour
)

// Placement identifies one resolved set of fail-policy placement constraints.
// It is intentionally independent of the NodeClass that selected the groups so
// equivalent NodeClasses share availability knowledge.
type Placement string

// NewPlacement returns a stable key for a resolved set of placement constraints.
func NewPlacement(constraints ...Constraint) Placement {
	parts := make([]string, 0, len(constraints))
	for _, constraint := range constraints {
		parts = append(parts, constraint.Kind+":"+constraint.ID)
	}
	if len(parts) == 0 {
		return ""
	}
	slices.Sort(parts)
	parts = slices.Compact(parts)
	return Placement(strings.Join(parts, ","))
}

// Constraint is one resolved placement constraint. Callers adapt provider API
// objects into this lower-level value before constructing a Placement.
type Constraint struct {
	Kind string
	ID   string
}

type placementEntryKey struct {
	Placement Placement
	Shape     Shape
}

// placementEntry is one shape refused under one resolved placement constraint.
// It rules out that shape and every shape at least as large in both dimensions.
type placementEntry struct {
	Placement Placement
	Shape     Shape

	Consecutive int
	LastRefusal time.Time
	RetryAfter  time.Time
	Message     string

	probeInFlight bool
}

// RecordPlacementRefusal records that this exact shape could not satisfy this
// exact set of placement constraints. The first refusal establishes history but
// does not gate launches; a later consecutive refusal opens the breaker.
func (c *Manager) RecordPlacementRefusal(
	ctx context.Context,
	placement Placement,
	specs Shape,
	message string,
) (window time.Duration, gated bool) {
	if placement == "" {
		return 0, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	c.sweepPlacements(now)
	key := placementEntryKey{Placement: placement, Shape: specs}
	current, ok := c.placements[key]
	if !ok {
		current = &placementEntry{Placement: placement, Shape: specs}
		c.placements[key] = current
	}
	current.LastRefusal = now
	current.Message = message
	current.probeInFlight = false

	if ok && now.Before(current.RetryAfter) {
		return current.RetryAfter.Sub(now), current.Consecutive >= placementGateAfter
	}

	run := current.Consecutive
	for otherKey, other := range c.placements {
		if otherKey == key || other.Placement != placement || !covers(other.Shape, specs) {
			continue
		}
		run = max(run, other.Consecutive)
	}
	current.Consecutive = run + 1
	window = c.placementBackoffFor(current.Consecutive)
	current.RetryAfter = now.Add(window)
	gated = current.Consecutive >= placementGateAfter

	logger := log.FromContext(ctx).WithValues(
		"placement", placement,
		"instance-type", shapeKey(Shape{Ncpus: specs.Ncpus, MemInGiB: specs.MemInGiB}),
		"consecutive", current.Consecutive,
	)
	if gated {
		logger.Info(
			"placement could not be satisfied again, backing off this shape",
			"backoff", window.String(),
		)
	} else {
		logger.V(1).Info(
			"placement could not be satisfied, retaining the shape during the grace window",
			"grace-window", window.String(),
		)
	}
	return window, gated
}

// RecordPlacementSuccess clears every refusal disproved by a successful launch
// under the same placement. A successful larger shape disproves failures of
// smaller shapes; a successful smaller shape says nothing about larger ones.
func (c *Manager) RecordPlacementSuccess(
	ctx context.Context,
	placement Placement,
	specs Shape,
) {
	if placement == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	cleared := 0
	for key, entry := range c.placements {
		if entry.Placement != placement || !covers(entry.Shape, specs) {
			continue
		}
		delete(c.placements, key)
		cleared++
	}
	if cleared == 0 {
		return
	}
	log.FromContext(ctx).V(1).Info(
		"a launch satisfied placement, clearing covered shape backoffs",
		"placement", placement,
		"instance-type", shapeKey(Shape{Ncpus: specs.Ncpus, MemInGiB: specs.MemInGiB}),
		"cleared", cleared,
	)
}

func (c *Manager) placementBackoffFor(consecutive int) time.Duration {
	backoff := placementMaxBackoff
	if consecutive >= 1 && consecutive <= 10 {
		backoff = min(placementBaseBackoff<<(consecutive-1), placementMaxBackoff)
	}
	return backoff + c.jitter(backoff)
}

func (c *Manager) sweepPlacements(now time.Time) {
	for key, entry := range c.placements {
		if now.Sub(entry.LastRefusal) > placementHistoryTTL {
			delete(c.placements, key)
		}
	}
}
