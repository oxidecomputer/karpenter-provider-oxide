/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package capacity

import (
	"cmp"
	"slices"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *Controller) render(view observation) v1alpha1.OxideCapacityStatus {
	reading := view.Availability
	rack, inventory := reading.Rack()
	status := v1alpha1.OxideCapacityStatus{
		ObservedAt: timestamp(reading.ObservedAt()),
		GlobalInstanceTypes: v1alpha1.GlobalInstanceTypeSummary{
			Count:            view.Total,
			UnavailableCount: view.Unavailable,
		},
		Inventory: v1alpha1.InventoryStatus{State: string(inventory)},
		Quota:     renderQuota(reading.Quota()),
	}
	if rack.Known() {
		renderRack(c.instanceTypes, rack, &status.Inventory)
	}
	for _, entry := range reading.Entries() {
		status.LaunchBackoffs = append(status.LaunchBackoffs, renderGlobalBackoff(entry))
	}
	var history v1alpha1.CapacityHistory
	for _, refusal := range reading.Placements() {
		placement := renderPlacement(refusal.Placement)
		if !refusal.HasBackoff() {
			history.Placement = append(history.Placement, v1alpha1.PlacementFailure{
				Placement: placement, Shape: renderShape(refusal.Shape),
				LastRefusal: timestamp(refusal.LastRefusal), Message: refusal.Message,
			})
			continue
		}
		backoff := v1alpha1.LaunchBackoff{
			Scope: v1alpha1.CapacityScope{
				Type:      v1alpha1.CapacityScopePlacement,
				Placement: placement,
			},
			MinimumShape:        renderShape(refusal.Shape),
			LastRefusedShape:    renderShape(refusal.Shape),
			State:               backoffState(refusal.Gated, refusal.ProbeInFlight),
			Reason:              string(availability.ReasonAffinityPolicy),
			ConsecutiveRefusals: refusal.Consecutive,
			LastRefusal:         timestamp(refusal.LastRefusal),
			Message:             refusal.Message,
		}
		if refusal.Gated {
			backoff.RetryAfter = timestamp(refusal.RetryAfter)
		}
		status.LaunchBackoffs = append(status.LaunchBackoffs, backoff)
	}
	if refusal := view.Storage; view.HasStorage {
		history.Storage = &v1alpha1.StorageRefusal{
			Reason: string(refusal.Reason), Message: refusal.Message,
			ConsecutiveRefusals: refusal.Consecutive, LastRefusal: timestamp(refusal.LastRefusal),
		}
	}
	if history.Storage != nil || len(history.Placement) > 0 {
		slices.SortFunc(history.Placement, func(a, b v1alpha1.PlacementFailure) int {
			if order := comparePlacement(a.Placement, b.Placement); order != 0 {
				return order
			}
			return compareShapes(a.Shape, b.Shape)
		})
		status.History = &history
	}
	slices.SortFunc(status.LaunchBackoffs, compareBackoffs)
	return status
}

func renderGlobalBackoff(entry availability.Entry) v1alpha1.LaunchBackoff {
	minimum := entry.Reach
	if minimum.Ncpus == 0 || minimum.MemInGiB == 0 {
		minimum = entry.Shape
	}
	backoff := v1alpha1.LaunchBackoff{
		Scope:        v1alpha1.CapacityScope{Type: v1alpha1.CapacityScopeGlobal},
		MinimumShape: renderShape(minimum), LastRefusedShape: renderShape(entry.Shape),
		State:  backoffState(entry.State == availability.StateBackoff, entry.ProbeInFlight()),
		Reason: string(entry.Reason), ConsecutiveRefusals: entry.ConsecutiveICEs,
		LastRefusal: timestamp(entry.LastICE),
	}
	if entry.Platform != availability.Unpinned {
		backoff.Scope.CPUPlatforms = []string{string(entry.Platform)}
		for _, platform := range v1alpha1.CPUPlatforms {
			if entry.Platform.Covers(availability.Platform(platform)) {
				backoff.Scope.CPUPlatforms = append(backoff.Scope.CPUPlatforms, platform)
			}
		}
		slices.Sort(backoff.Scope.CPUPlatforms)
		backoff.Scope.CPUPlatforms = slices.Compact(backoff.Scope.CPUPlatforms)
	}
	if entry.Floor.Ncpus > 0 && entry.Floor.MemInGiB > 0 {
		shape := renderShape(entry.Floor)
		backoff.KnownLaunchableShape = &shape
	}
	if entry.State == availability.StateBackoff {
		backoff.RetryAfter = timestamp(entry.RetryAfter)
	}
	return backoff
}

func renderShape(shape availability.Shape) v1alpha1.CapacityShape {
	return v1alpha1.CapacityShape{CPUs: shape.Ncpus, MemoryGiB: shape.MemInGiB}
}

func renderPlacement(placement availability.Placement) []v1alpha1.CapacityPlacementConstraint {
	constraints := placement.Constraints()
	groups := make([]v1alpha1.CapacityPlacementConstraint, 0, len(constraints))
	for _, constraint := range constraints {
		groups = append(
			groups,
			v1alpha1.CapacityPlacementConstraint{Type: constraint.Kind, ID: constraint.ID},
		)
	}
	return groups
}

func backoffState(waiting, probing bool) string {
	if waiting {
		return v1alpha1.BackoffWaiting
	}
	if probing {
		return v1alpha1.BackoffProbing
	}
	return v1alpha1.BackoffProbeEligible
}

func compareBackoffs(a, b v1alpha1.LaunchBackoff) int {
	if order := cmp.Compare(backoffPriority(a.State), backoffPriority(b.State)); order != 0 {
		return order
	}
	if order := cmp.Compare(a.Scope.Type, b.Scope.Type); order != 0 {
		return order
	}
	if order := slices.Compare(a.Scope.CPUPlatforms, b.Scope.CPUPlatforms); order != 0 {
		return order
	}
	if order := comparePlacement(a.Scope.Placement, b.Scope.Placement); order != 0 {
		return order
	}
	if order := compareShapes(a.MinimumShape, b.MinimumShape); order != 0 {
		return order
	}
	return compareShapes(a.LastRefusedShape, b.LastRefusedShape)
}

func backoffPriority(state string) int {
	switch state {
	case v1alpha1.BackoffWaiting:
		return 0
	case v1alpha1.BackoffProbing:
		return 1
	default:
		return 2
	}
}

func comparePlacement(a, b []v1alpha1.CapacityPlacementConstraint) int {
	return slices.CompareFunc(a, b, func(a, b v1alpha1.CapacityPlacementConstraint) int {
		if order := cmp.Compare(a.Type, b.Type); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

func compareShapes(a, b v1alpha1.CapacityShape) int {
	if order := cmp.Compare(a.CPUs, b.CPUs); order != 0 {
		return order
	}
	return cmp.Compare(a.MemoryGiB, b.MemoryGiB)
}

func sortSleds(sleds []v1alpha1.SledCapacity) {
	slices.SortFunc(sleds, func(a, b v1alpha1.SledCapacity) int { return cmp.Compare(a.ID, b.ID) })
}

func renderRack(
	instanceTypes *instancetype.Provider,
	rack capacity.Snapshot,
	rendered *v1alpha1.InventoryStatus,
) {
	rendered.ObservedAt = timestamp(rack.TakenAt())

	for _, sled := range rack.Sleds() {
		free := sled.Free()

		if !sled.Provisionable {
			free = capacity.Resources{}
		}
		entry := v1alpha1.SledCapacity{
			ID:            sled.ID,
			Provisionable: sled.Provisionable,
			FreeCPUs:      free.CPUs,
			FreeMemoryGiB: int(free.Memory / GiB),
		}
		if shape, ok := instanceTypes.LargestShapeThatFits(free); ok {
			entry.LargestFeasibleInstanceType = instancetype.SpecsToInstanceTypeName(
				shape.Ncpus,
				shape.MemInGiB,
			)
		}
		rendered.Sleds = append(rendered.Sleds, entry)
	}
	sortSleds(rendered.Sleds)

	if shape, ok := instanceTypes.LargestShapeOnRack(rack); ok {
		rendered.LargestFeasibleInstanceType = instancetype.SpecsToInstanceTypeName(
			shape.Ncpus,
			shape.MemInGiB,
		)
	}
}

func timestamp(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	truncated := metav1.NewTime(t.Truncate(time.Second))
	return &truncated
}

func renderQuota(reading utilization.Snapshot) v1alpha1.QuotaStatus {
	status := v1alpha1.QuotaStatus{State: v1alpha1.QuotaUnknown}
	if !reading.Known {
		return status
	}
	status.State = v1alpha1.QuotaFresh
	if reading.Stale {
		status.State = v1alpha1.QuotaStale
	}
	status.ObservedAt = timestamp(reading.TakenAt)
	quota := reading.Quota
	status.Remaining = &v1alpha1.QuotaHeadroom{
		CPUs:       max(quota.CapacityCPUs-quota.ProvisionedCPUs, 0),
		MemoryGiB:  int64(headroom(quota.CapacityMemory, quota.ProvisionedMemory) / GiB),
		StorageGiB: int64(headroom(quota.CapacityStorage, quota.ProvisionedStorage) / GiB),
	}
	return status
}

func headroom(entitled, provisioned uint64) uint64 {
	if entitled <= provisioned {
		return 0
	}
	return entitled - provisioned
}
