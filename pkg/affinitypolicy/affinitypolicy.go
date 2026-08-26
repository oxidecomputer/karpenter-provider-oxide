// Package affinitypolicy backs off unsatisfiable placement policies.
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
	baseBackoff = 30 * time.Second

	maxBackoff = 5 * time.Minute

	gateThreshold = 2

	jitterFraction = 0.2

	historyTTL = time.Hour
)

// Tracker records which nodeclasses have had a launch refused while a fail-policy group was in their desired set.
type Tracker struct {
	mu      sync.Mutex
	entries map[string]*entry

	now    func() time.Time
	jitter func(time.Duration) time.Duration
}

type entry struct {
	consecutive int

	lastRefusal time.Time

	until time.Time

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

// Attributable reports whether a refusal may come from a fail policy.
func Attributable(groups []v1alpha1.OxideAffinityGroup) bool {
	for _, group := range groups {
		if group.Policy == policyFail {
			return true
		}
	}
	return false
}

const policyFail = "fail"

// RecordRefusal notes that a launch for this nodeclass was refused while a fail-policy group was in its desired set.
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

		log.FromContext(ctx).V(1).Info(
			"a fail-policy affinity group could not be satisfied, retrying before holding the nodeclass out of provisioning",
			"nodeclass", nodeClass,
			"consecutive", current.consecutive,
			"retrying-for", window.String(),
		)
	}
	return window, gated
}

// RecordSuccess forgets a nodeclass's refusal history.
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

// State returns the nodeclass's back-off if it is currently holding the nodeclass out of provisioning.
func (t *Tracker) State(nodeClass string) (State, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	current, ok := t.entries[nodeClass]
	if !ok {
		return State{}, false
	}
	now := t.now()

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

// Forget drops a nodeclass's history.
func (t *Tracker) Forget(nodeClass string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, nodeClass)
}

func (t *Tracker) backoffFor(consecutive int) time.Duration {
	backoff := maxBackoff

	if consecutive >= 1 && consecutive <= 10 {
		backoff = min(baseBackoff<<(consecutive-1), maxBackoff)
	}
	return backoff + t.jitter(backoff)
}

func randomJitter(backoff time.Duration) time.Duration {
	return time.Duration(rand.Float64() * jitterFraction * float64(backoff))
}
