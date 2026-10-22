package availability

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const GiB = 1 << 30

// Shape is the CPU and memory requested from one sled.
type Shape struct {
	Ncpus    int
	MemInGiB int
}

func shapeKey(shape Shape) string {
	return fmt.Sprintf("%d-%d", shape.Ncpus, shape.MemInGiB)
}

// Platform is the CPU platform a launch is pinned to, by its Oxide API name.
// Each pin narrows the sleds a launch can land on: unpinned takes any sled, a
// Milan pin takes Milan and Turin sleds, Turin pins take Turin sleds only. So a
// refusal at a broader platform also holds for the narrower ones, and a
// success at a narrower platform also clears the broader ones.
type Platform string

// Unpinned is a launch that left the platform to the rack.
const Unpinned Platform = ""

var platformTiers = map[Platform]int{
	"amd_milan":    1,
	"amd_turin":    2,
	"amd_turin_v2": 2,
}

// Covers reports whether what the rack said about a launch pinned to p also
// holds for one pinned to q. Platforms the tiers don't know are only covered
// by themselves and by unpinned.
func (p Platform) Covers(q Platform) bool {
	if p == Unpinned || p == q {
		return true
	}
	tp, ok := platformTiers[p]
	tq, okq := platformTiers[q]
	return ok && okq && tp <= tq
}

func entryKey(shape Shape, platform Platform) string {
	if platform == Unpinned {
		return shapeKey(shape)
	}
	return shapeKey(shape) + "@" + string(platform)
}

// Request is one shaped launch under an optional pin and placement scope.
type Request struct {
	Shape     Shape
	Platform  Platform
	Placement Placement
}

// Decision is the manager's current judgment about a request.
type Decision struct {
	Available bool
	Reason    Reason
}

// RefusalScope identifies which part of a request refused a launch.
type RefusalScope string

const (
	RefusalScopeGlobal    RefusalScope = "global"
	RefusalScopePlacement RefusalScope = "placement"
)

// Refusal describes a diagnosed launch refusal.
type Refusal struct {
	Scope   RefusalScope
	Reason  Reason
	Message string
}

// Probe owns one attempt admitted by the manager. Exactly one terminal method
// takes effect; callers should defer Release and then report an outcome when one
// reaches the rack.
type Probe struct {
	manager *Manager
	request Request
	done    sync.Once
}

// Release returns a probe that did not produce an availability outcome.
func (p *Probe) Release() {
	p.done.Do(func() {
		p.manager.release(p.request)
	})
}

// RecordSuccess reports that the request launched successfully.
func (p *Probe) RecordSuccess(ctx context.Context) {
	p.done.Do(func() {
		p.manager.RecordSuccess(ctx, p.request.Shape, p.request.Platform)
		p.manager.RecordPlacementSuccess(ctx, p.request.Placement, p.request.Shape)
		p.manager.release(p.request)
	})
}

// RecordRefusal reports a diagnosed refusal and returns its backoff window and
// whether it immediately gated launches. Global refusals always gate.
func (p *Probe) RecordRefusal(
	ctx context.Context,
	refusal Refusal,
) (windowDuration time.Duration, gated bool) {
	p.done.Do(func() {
		switch refusal.Scope {
		case RefusalScopePlacement:
			windowDuration, gated = p.manager.RecordPlacementRefusal(
				ctx,
				p.request.Placement,
				p.request.Shape,
				refusal.Message,
			)
		default:
			p.manager.RecordICE(ctx, p.request.Shape, p.request.Platform, refusal.Reason)
			gated = true
		}
		p.manager.release(p.request)
	})
	return windowDuration, gated
}
