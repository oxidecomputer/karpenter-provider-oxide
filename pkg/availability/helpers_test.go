package availability

import "sync"

// The manager's public surface is request-oriented, so these narrow it back
// down to the single question each test is asking. The probe registry stands in
// for the caller that would otherwise be holding one across a launch.

func unavailableFor(c *Manager, shape Shape) (bool, Reason) {
	decision := c.Check(Request{Shape: shape})
	return !decision.Available, decision.Reason
}

func acquireShape(c *Manager, shape Shape) bool {
	return acquireHeld(c, Request{Shape: shape})
}

func releaseShape(shape Shape) {
	releaseHeld(Request{Shape: shape})
}

func placementUnavailable(c *Manager, placement Placement, shape Shape) bool {
	return !c.Check(Request{Shape: shape, Placement: placement}).Available
}

func acquirePlacement(c *Manager, shape Shape) bool {
	return acquireHeld(c, Request{Shape: shape, Placement: testPlacement})
}

func releasePlacement(shape Shape) {
	releaseHeld(Request{Shape: shape, Placement: testPlacement})
}

var held = struct {
	sync.Mutex
	probes map[Request]*Probe
}{probes: map[Request]*Probe{}}

func acquireHeld(c *Manager, request Request) bool {
	probe, ok := c.Acquire(request)
	if !ok {
		return false
	}
	held.Lock()
	defer held.Unlock()
	held.probes[request] = probe
	return true
}

func releaseHeld(request Request) {
	held.Lock()
	probe, ok := held.probes[request]
	delete(held.probes, request)
	held.Unlock()

	if ok {
		probe.Release()
	}
}
