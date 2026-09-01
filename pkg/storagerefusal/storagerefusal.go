// Package storagerefusal remembers the last launch refused for want of boot
// disk storage. It is diagnostic history: no shape is taken out of
// consideration by it, so it is kept apart from shape availability.
package storagerefusal

import (
	"context"
	"sync"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const historyTTL = 6 * time.Hour

// Refusal is the most recent launch refused because the boot disk could not be provisioned.
type Refusal struct {
	// Reason is which limit was diagnosed as binding, on the same terms as a shape's.
	Reason availability.Reason
	// Consecutive is how many launches in a row have been refused this way.
	Consecutive int
	// LastRefusal is when the most recent one happened, and is what this ages out from.
	LastRefusal time.Time
	// Message is the diagnosis as it was reported on the NodeClaim.
	Message string
}

// Tracker holds the current storage refusal, if there is one.
type Tracker struct {
	mu      sync.Mutex
	current *Refusal

	now func() time.Time
}

// NewTracker builds an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{now: time.Now}
}

// Record reports that a launch was refused because its boot disk could not be provisioned.
func (t *Tracker) Record(ctx context.Context, reason availability.Reason, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.current == nil {
		t.current = &Refusal{}
	}
	t.current.Reason = reason
	t.current.Consecutive++
	t.current.LastRefusal = t.now()
	t.current.Message = message

	metrics.StorageRefusals.WithLabelValues(string(reason)).Inc()

	log.FromContext(ctx).V(1).Info(
		"launch refused for want of boot disk storage",
		"reason", reason,
		"consecutive-refusals", t.current.Consecutive,
		"diagnosis", message,
	)
}

// Clear reports that a launch created its boot disk, which is every launch that succeeds.
func (t *Tracker) Clear(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.current == nil {
		return
	}
	log.FromContext(ctx).V(1).Info(
		"boot disk storage is available again",
		"reason", "launch-succeeded",
		"consecutive-refusals", t.current.Consecutive,
	)
	t.current = nil
}

// Last returns the current refusal, and whether there is one to report.
func (t *Tracker) Last() (Refusal, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.current == nil {
		return Refusal{}, false
	}
	if t.now().Sub(t.current.LastRefusal) > historyTTL {
		t.current = nil
		return Refusal{}, false
	}
	return *t.current, true
}
