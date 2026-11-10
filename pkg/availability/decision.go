package availability

import (
	"fmt"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
)

type PlacementRefusal struct {
	Placement   Placement
	Shape       Shape
	Consecutive int
	LastRefusal time.Time
	RetryAfter  time.Time
	Message     string
	Gated       bool
}

func (s Snapshot) WithQuota(quota utilization.Snapshot) Snapshot {
	s.quota = quota
	return s
}

func (s Snapshot) Quota() utilization.Snapshot    { return s.quota }
func (s Snapshot) ObservedAt() time.Time          { return s.observedAt }
func (s Snapshot) Generation() uint64             { return s.generation }
func (s Snapshot) Placements() []PlacementRefusal { return s.placements }

func (s Snapshot) Evaluate(request Request) Decision {
	if s.quota.Known {
		wanted := utilization.Request{
			CPUs: int64(request.Shape.Ncpus), Memory: uint64(request.Shape.MemInGiB) * GiB,
			Storage: request.Storage,
		}
		if admits, resource := s.quota.Quota.Admits(wanted); !admits {
			return Decision{
				Reason: ReasonQuota, Scope: RefusalScopeGlobal, Source: SourceQuota,
				Resource: resource, ObservedAt: s.quota.TakenAt, Stale: s.quota.Stale,
				Message: s.quota.Quota.Explain(wanted, resource),
			}
		}
	}
	if s.inventory == InventoryFresh && !s.rack.CanFit(resourcesFor(request.Shape)) {
		return Decision{Reason: ReasonCapacity, Scope: RefusalScopeGlobal,
			Source: SourceInventory, ObservedAt: s.rack.TakenAt(),
			Message: fmt.Sprintf("no sled in the inventory has room for %d CPUs and %d GiB",
				request.Shape.Ncpus, request.Shape.MemInGiB)}
	}
	decision := Decision{Available: true}
	for _, entry := range s.entries {
		if entry.State != StateBackoff || !covers(entry.reachOrSpecs(), request.Shape) ||
			!entry.Platform.Covers(request.Platform) {
			continue
		}
		if !decision.Available && decision.Reason == ReasonQuota && entry.Reason != ReasonQuota {
			continue
		}
		if decision.Available || (entry.Reason == ReasonQuota && decision.Reason != ReasonQuota) ||
			entry.RetryAfter.After(decision.RetryAfter) {
			decision = Decision{
				Reason:     entry.Reason,
				Scope:      RefusalScopeGlobal,
				Source:     SourceRefusal,
				RetryAfter: entry.RetryAfter,
				ObservedAt: entry.LastICE,
				Message: fmt.Sprintf(
					"%d CPUs and %d GiB under CPU platform %q are in backoff after a %s refusal until %s",
					request.Shape.Ncpus,
					request.Shape.MemInGiB,
					request.Platform,
					entry.Reason,
					entry.RetryAfter.Format(time.RFC3339),
				),
			}
		}
	}
	if !decision.Available {
		return decision
	}
	for _, entry := range s.placements {
		if !entry.Gated || entry.Placement != request.Placement ||
			!covers(entry.Shape, request.Shape) {
			continue
		}
		if decision.Available || entry.RetryAfter.After(decision.RetryAfter) {
			decision = Decision{Reason: ReasonAffinityPolicy, Scope: RefusalScopePlacement,
				Source: SourceRefusal, RetryAfter: entry.RetryAfter, ObservedAt: entry.LastRefusal,
				Message: fmt.Sprintf("placement %q is in backoff until %s: %s",
					request.Placement, entry.RetryAfter.Format(time.RFC3339), entry.Message)}
		}
	}
	return decision
}
