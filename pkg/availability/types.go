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

// Request is one shaped launch under an optional resolved placement scope.
type Request struct {
	Shape     Shape
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
		p.manager.RecordSuccess(ctx, p.request.Shape)
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
			p.manager.RecordICE(ctx, p.request.Shape, refusal.Reason)
			gated = true
		}
		p.manager.release(p.request)
	})
	return windowDuration, gated
}
