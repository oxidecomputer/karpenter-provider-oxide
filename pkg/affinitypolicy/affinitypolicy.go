// Package affinitypolicy backs a nodeclass off when the rack cannot satisfy a fail-policy affinity
// group it launches into.
//
// An Oxide affinity or anti-affinity group with policy "fail" refuses a launch the rack cannot place
// within the group's constraint: an anti-affinity group has no sled left that is free of the
// nodeclass's instances, or an affinity group has no single sled with room for another. Neither is a
// statement about the instance shape. No smaller shape would have fared better, because what ran out
// is sleds satisfying a constraint rather than room on any one of them.
//
// That is why this is not the shape circuit breaker in pkg/instancetype. Recording an affinity
// refusal there would take the refused shape, and every shape larger, out of consideration for every
// nodeclass in the cluster, on the strength of a limit that had nothing to do with shapes. It would
// then walk down through smaller shapes, fail identically at each, and end up asserting that the rack
// has no room for a one-CPU node. The reasoning is the same one written down for StorageRefusal,
// which is likewise deliberately not keyed by shape.
//
// So the back-off is keyed by nodeclass and enforced through the nodeclass's readiness. A nodeclass
// that is not Ready is filtered out of provisioning by Karpenter's own provisioner, which is what
// stops the scheduler from producing NodeClaims that cannot launch, without touching what any other
// nodeclass is offered. It is blunt -- all provisioning for that nodeclass stops, including for pods
// and shapes with no bearing on the group -- but under an unsatisfiable fail policy that is simply
// true: no instance of that nodeclass can launch until something frees a sled.
//
// Because it is blunt, and because the attribution below is a guess, it takes two consecutive
// refusals to engage. The first only starts a clock; the retries through it are what distinguish a
// launch that raced a node being deleted from a policy that genuinely cannot be satisfied. See
// gateThreshold.
//
// TODO: the Oxide API does not currently distinguish a fail-policy refusal from a genuine capacity
// refusal. Both arrive as InsufficientCapacity with no error code or body that separates them, so a
// launch is attributed here on the basis that the nodeclass had a fail-policy group in its desired
// set and the quota had room. Once the rack reports the two differently, attribute on the error
// itself: a refusal that is genuinely about capacity should go back to pkg/instancetype's breaker
// where it belongs, so it warns every nodeclass rather than only this one. See Attributable.
package affinitypolicy

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// baseBackoff is the first wait, and every wait after it doubles.
	//
	// Deliberately shorter than pkg/instancetype's, because what this waits on is a different and much
	// more dynamic thing. A shape becomes launchable again when the rack gains room, which is a slow
	// event; a fail policy becomes satisfiable again the moment any one of the nodeclass's own
	// instances goes away, which happens on every scale-down, consolidation and node replacement. So
	// the first retry is quick, when a freed sled is most likely.
	baseBackoff = 30 * time.Second
	// maxBackoff caps the wait, and is low for the same reason. A policy that has been unsatisfiable
	// five times running still becomes satisfiable the instant a node is deleted, and there is nothing
	// to be gained by not noticing that for half an hour. The cost of a short cap is one launch per
	// interval per nodeclass, which is what the interval is chosen to bound.
	maxBackoff = 5 * time.Minute
	// gateThreshold is how many consecutive refusals it takes before the nodeclass is actually held
	// out of provisioning. Refusals below it are counted and timed but gate nothing.
	//
	// One refusal is not enough, because the Oxide API does not say whether a fail policy is what
	// refused the launch (see Attributable) and holding a nodeclass out of provisioning on a guess is
	// the one consequence here worth being slow about. It is also not enough on its own terms: a
	// single refusal can be a race rather than a condition, where a node was being deleted as the
	// launch was placed and the sled it frees would have satisfied the policy a moment later.
	//
	// So the first refusal opens a window that gates nothing. The provisioner keeps retrying through
	// it, which is exactly what settles the question: a racing refusal is answered by a launch that
	// succeeds, which clears the history and never un-Readies anything, while a real condition refuses
	// again and gates from the second window on. The cost is one window of retries, bounded by
	// baseBackoff.
	gateThreshold = 2
	// jitterFraction is the range of the delay added on top of each backoff, as a fraction of it.
	jitterFraction = 0.2
	// historyTTL is how long a nodeclass's refusal history outlives its last refusal. History is what
	// makes the backoff grow, so forgetting it too eagerly turns a persistently unsatisfiable policy
	// back into a thirty second retry loop.
	//
	// Scaled to the cap rather than to the shape breaker's: the whole schedule runs its course in
	// under eight minutes, so an hour is already several incidents' worth of memory, and remembering
	// longer would make a single refusal today wait the full cap on the strength of a blip this
	// morning.
	historyTTL = time.Hour
)

// Tracker records which nodeclasses have had a launch refused while a fail-policy group was in their
// desired set, and for how long each should stop launching.
//
// Held in memory rather than on the nodeclass, for the same reason the shape breaker is: it is a
// record of what this process has observed, and losing it on a restart costs one wasted launch to
// rediscover. Writing it to the nodeclass status would also mean the launch path patching an object
// the nodeclass controller owns.
type Tracker struct {
	mu      sync.Mutex
	entries map[string]*entry

	// now and jitter are injectable so tests can drive the backoff on a fake clock and compare exact
	// waits. Nothing outside this package sets them.
	now    func() time.Time
	jitter func(time.Duration) time.Duration
}

// entry is one nodeclass's refusal history.
type entry struct {
	// consecutive is how many back-off windows in a row have opened with a refusal. It only resets
	// when a launch actually succeeds, which is the only thing that proves the policy is satisfiable.
	consecutive int
	// lastRefusal is when the most recent refusal happened, and is what the history ages out from.
	lastRefusal time.Time
	// until is when the nodeclass may launch again. A refusal arriving before it is a launch that was
	// already in flight, and does not lengthen the wait.
	until time.Time
	// message is the diagnosis as it was reported on the NodeClaim, kept so the condition can say what
	// was actually refused rather than only that something was.
	message string
}

// State is what a nodeclass's back-off looks like to a reader.
type State struct {
	// Consecutive is how many times running the policy has been found unsatisfiable.
	Consecutive int
	// Until is when the nodeclass may launch again.
	Until time.Time
	// Message is the refusal as it was diagnosed.
	Message string
}

// Remaining is how long the back-off still has to run, and is never negative.
func (s State) Remaining(now time.Time) time.Duration {
	return max(s.Until.Sub(now), 0)
}

func NewTracker() *Tracker {
	return &Tracker{
		entries: map[string]*entry{},
		now:     time.Now,
		jitter:  randomJitter,
	}
}

// Attributable reports whether a refused launch should be attributed to an unsatisfiable fail policy
// rather than to the rack having no room.
//
// The Oxide API does not say, so this asks the only question that can be asked: did the nodeclass ask
// for something the rack refuses launches over? A nodeclass with no fail-policy group in its desired
// set cannot have been refused by one, so its refusals stay with the shape breaker where they warn
// every nodeclass. A nodeclass with one may have been, and the cost of being wrong is asymmetric --
// misattributing a capacity refusal costs the other nodeclasses one wasted launch each to discover
// the rack is full, whereas misattributing a policy refusal takes a shape and everything larger out
// of consideration cluster-wide for a limit it never hit.
//
// TODO: replace this with a check on the error itself once the rack distinguishes the two. See the
// package comment.
func Attributable(groups []v1alpha1.OxideAffinityGroup) bool {
	for _, group := range groups {
		if group.Policy == policyFail {
			return true
		}
	}
	return false
}

// policyFail is the Oxide affinity policy that refuses a launch rather than placing it anyway.
const policyFail = "fail"

// RecordRefusal notes that a launch for this nodeclass was refused while a fail-policy group was in
// its desired set. It returns the window this refusal opened and whether that window holds the
// nodeclass out of provisioning, which the first one does not. See gateThreshold.
//
// A refusal arriving inside an open window neither lengthens it nor counts. Karpenter provisions in
// bursts, so one unsatisfiable policy produces a handful of refusals within a second of each other,
// and counting each of them would jump straight to the cap on the first incident -- and would defeat
// the point of the threshold, since a single burst would satisfy it on its own.
func (t *Tracker) RecordRefusal(
	ctx context.Context,
	nodeClass, message string,
) (window time.Duration, gated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	current, ok := t.entries[nodeClass]
	if !ok {
		current = &entry{}
		t.entries[nodeClass] = current
	}
	current.lastRefusal = now
	current.message = message

	if ok && now.Before(current.until) {
		// Inside the open window. This launch was in flight before the window opened, or is one of the
		// retries the window exists to absorb.
		return current.until.Sub(now), current.consecutive >= gateThreshold
	}

	current.consecutive++
	window = t.backoffFor(current.consecutive)
	current.until = now.Add(window)
	gated = current.consecutive >= gateThreshold

	if gated {
		log.FromContext(ctx).Info(
			"a fail-policy affinity group could not be satisfied again, holding the nodeclass out of provisioning",
			"nodeclass", nodeClass,
			"consecutive", current.consecutive,
			"backoff", window.String(),
		)
	} else {
		// Not an error and not yet a verdict. Logged so that a launch refused this way is visible
		// even when it turns out to have been a race and nothing further happens.
		log.FromContext(ctx).V(1).Info(
			"a fail-policy affinity group could not be satisfied, retrying before holding the nodeclass out of provisioning",
			"nodeclass", nodeClass,
			"consecutive", current.consecutive,
			"retrying-for", window.String(),
		)
	}
	return window, gated
}

// RecordSuccess forgets a nodeclass's refusal history. A launch that placed is the only proof the
// policy is satisfiable, and it contradicts everything recorded before it.
func (t *Tracker) RecordSuccess(ctx context.Context, nodeClass string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.entries[nodeClass]; !ok {
		return
	}
	delete(t.entries, nodeClass)
	log.FromContext(ctx).V(1).Info(
		"a launch satisfied the nodeclass's fail-policy affinity groups, clearing its back-off",
		"nodeclass", nodeClass,
	)
}

// State returns the nodeclass's back-off if it is currently holding the nodeclass out of
// provisioning, and reports whether it is.
//
// Two things are deliberately not reported. A single refusal, because one is not yet a verdict and
// the retries through its window are what decide whether it was a race; and history outliving a
// window, because that is what makes the next wait longer rather than a reason to keep the nodeclass
// un-Ready. In both cases the nodeclass is Ready and a launch is free to probe the policy.
func (t *Tracker) State(nodeClass string) (State, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	current, ok := t.entries[nodeClass]
	if !ok {
		return State{}, false
	}
	now := t.now()
	// Aged out entirely. Dropped here rather than by a sweep, since this is the only reader and there
	// is nothing else the history could be affecting in the meantime.
	if now.Sub(current.lastRefusal) > historyTTL {
		delete(t.entries, nodeClass)
		return State{}, false
	}
	if current.consecutive < gateThreshold || !now.Before(current.until) {
		return State{}, false
	}
	return State{
		Consecutive: current.consecutive,
		Until:       current.until,
		Message:     current.message,
	}, true
}

// Forget drops a nodeclass's history outright, for a nodeclass that no longer exists or no longer
// asks for a fail policy. Unlike RecordSuccess this is not evidence about the rack, so it says
// nothing and is silent.
func (t *Tracker) Forget(nodeClass string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, nodeClass)
}

// backoffFor is the wait after this many consecutive refusals, doubling each time up to the cap.
func (t *Tracker) backoffFor(consecutive int) time.Duration {
	backoff := maxBackoff
	// Shifting past the cap would overflow, and anything at or beyond it is capped anyway.
	if consecutive >= 1 && consecutive <= 10 {
		backoff = min(baseBackoff<<(consecutive-1), maxBackoff)
	}
	return backoff + t.jitter(backoff)
}

// randomJitter is a delay of up to jitterFraction of the backoff, added on top of it. Provisioning
// refuses launches in bursts, and without jitter every nodeclass in a burst reopens in the same
// instant and walks into the same wall together.
func randomJitter(backoff time.Duration) time.Duration {
	return time.Duration(rand.Float64() * jitterFraction * float64(backoff))
}
