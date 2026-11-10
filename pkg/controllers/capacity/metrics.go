package capacity

import (
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
)

func (c *Controller) publishMetrics(view observation) {
	reading := view.Availability
	rack, inventory := reading.Rack()
	now := reading.ObservedAt()
	metrics.ResetShapeGauges()
	metrics.ResetSledGauges()
	ranges := len(reading.Entries())
	if inventory == availability.InventoryFresh {
		ranges += len(instancetype.RackHeadroom(rack))
	}
	metrics.UnavailableShapes.Set(float64(ranges))
	metrics.UnavailableInstanceTypes.Set(float64(view.Unavailable))
	metrics.InstanceTypes.Set(float64(view.Total))
	for _, entry := range reading.Entries() {
		label := instancetype.SpecsToInstanceTypeName(entry.Shape.Ncpus, entry.Shape.MemInGiB)
		metrics.ConsecutiveICEs.WithLabelValues(label, string(entry.Platform)).
			Set(float64(entry.ConsecutiveICEs))
		remaining := 0.0
		if entry.State == availability.StateBackoff {
			remaining = metrics.Seconds(entry.RetryAfter.Sub(now))
		}
		metrics.BackoffSeconds.WithLabelValues(label, string(entry.Platform)).Set(remaining)
		if inventory == availability.InventoryFresh {
			metrics.FeasibleInstances.WithLabelValues(label).
				Set(float64(rack.FeasibleCount(capacity.Resources{
					CPUs: entry.Shape.Ncpus, Memory: uint64(entry.Shape.MemInGiB) * GiB,
				})))
		}
	}
	metrics.StorageRefusalsConsecutive.Set(0)
	if view.HasStorage {
		metrics.StorageRefusalsConsecutive.Set(float64(view.Storage.Consecutive))
	}
	metrics.SnapshotFresh.Set(0)
	if inventory == availability.InventoryFresh {
		metrics.SnapshotFresh.Set(1)
	}
	metrics.SnapshotAgeSeconds.Set(0)
	if rack.Known() {
		metrics.SnapshotAgeSeconds.Set(metrics.Seconds(now.Sub(rack.TakenAt())))
	}
	for _, sled := range rack.Sleds() {
		free := sled.Free()
		if !sled.Provisionable {
			free = capacity.Resources{}
		}
		metrics.SledFreeCPUs.WithLabelValues(sled.ID).Set(float64(free.CPUs))
		metrics.SledFreeMemoryBytes.WithLabelValues(sled.ID).Set(float64(free.Memory))
	}
	metrics.LargestFeasibleCPUs.Set(0)
	metrics.LargestFeasibleMemoryBytes.Set(0)
	if shape, ok := c.instanceTypes.LargestShapeOnRack(rack); ok {
		metrics.LargestFeasibleCPUs.Set(float64(shape.Ncpus))
		metrics.LargestFeasibleMemoryBytes.Set(float64(shape.MemInGiB) * GiB)
	}
}
