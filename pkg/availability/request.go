package availability

import (
	"context"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
)

// Check evaluates global rack capacity and the request's optional placement scope.
func (m *Manager) Check(request Request) Decision {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sweep(context.Background())
	now := m.now()
	m.sweepPlacements(now)

	if m.rackFresh && !m.rack.CanFit(resourcesFor(request.Shape)) {
		return Decision{Reason: ReasonCapacity}
	}
	unavailable, reason := coveredEntries(m.entries, request.Shape, request.Platform)
	if unavailable {
		return Decision{Reason: reason}
	}
	for _, entry := range m.placements {
		if entry.Placement == request.Placement &&
			covers(entry.Shape, request.Shape) &&
			entry.Consecutive >= placementGateAfter &&
			now.Before(entry.RetryAfter) {
			return Decision{Reason: ReasonAffinityPolicy}
		}
	}
	return Decision{Available: true}
}

// Acquire atomically reserves every half-open global and placement breaker
// covering a request. The returned probe must be released or given an outcome.
func (m *Manager) Acquire(request Request) (*Probe, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sweep(context.Background())
	now := m.now()
	m.sweepPlacements(now)

	if m.rackFresh && !m.rack.CanFit(resourcesFor(request.Shape)) {
		return nil, false
	}

	probingGlobal := false
	for _, entry := range m.entries {
		if !covers(entry.Reach, request.Shape) || !entry.Platform.Covers(request.Platform) {
			continue
		}
		probingGlobal = true
		if entry.State == StateBackoff || entry.probeInFlight {
			metrics.Probes.WithLabelValues(metrics.ProbeRejected).Inc()
			return nil, false
		}
	}
	for _, entry := range m.placements {
		if entry.Placement != request.Placement ||
			!covers(entry.Shape, request.Shape) ||
			entry.Consecutive < placementGateAfter {
			continue
		}
		if now.Before(entry.RetryAfter) || entry.probeInFlight {
			return nil, false
		}
	}

	for _, entry := range m.entries {
		if covers(entry.Reach, request.Shape) && entry.Platform.Covers(request.Platform) {
			entry.probeInFlight = true
		}
	}
	for _, entry := range m.placements {
		if entry.Placement == request.Placement &&
			covers(entry.Shape, request.Shape) &&
			entry.Consecutive >= placementGateAfter {
			entry.probeInFlight = true
		}
	}
	if probingGlobal {
		metrics.Probes.WithLabelValues(metrics.ProbeAcquired).Inc()
	}
	return &Probe{manager: m, request: request}, true
}

func (m *Manager) release(request Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, entry := range m.entries {
		if covers(entry.Reach, request.Shape) && entry.Platform.Covers(request.Platform) {
			entry.probeInFlight = false
		}
	}
	for _, entry := range m.placements {
		if entry.Placement == request.Placement && covers(entry.Shape, request.Shape) {
			entry.probeInFlight = false
		}
	}
}

func coveredEntries(entries map[string]*Entry, shape Shape, platform Platform) (bool, Reason) {
	values := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		values = append(values, *entry)
	}
	return covered(values, shape, platform)
}
