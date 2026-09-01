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

// Package capacity publishes provider capacity.
package capacity

import (
	"context"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/storagerefusal"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	// GiB is how memory is reported here.
	GiB = 1 << 30

	heartbeat = 5 * time.Minute
)

// Controller publishes the capacity view.
type Controller struct {
	kubeClient    client.Client
	instanceTypes *instancetype.Provider
	availability  *availability.Manager
	storage       *storagerefusal.Tracker
	period        time.Duration

	crdMissingLogged bool
}

func NewController(
	kubeClient client.Client,
	instanceTypes *instancetype.Provider,
	availabilityManager *availability.Manager,
	storageRefusals *storagerefusal.Tracker,
	period time.Duration,
) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		instanceTypes: instanceTypes,
		availability:  availabilityManager,
		storage:       storageRefusals,
		period:        period,
	}
}

func (c *Controller) Name() string {
	return "capacity"
}

// Reconcile renders the current state, publishes the metrics from it, and writes the object if it has changed.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	c.seedLaunchableFloor(ctx)

	reading := c.availability.Snapshot()
	rack, inventory := reading.Rack()
	status := c.render(reading)
	publishMetrics(status, rack, inventory)

	if err := c.publish(ctx, status); err != nil {
		c.logPublishFailure(ctx, err)
		return reconciler.Result{RequeueAfter: c.period}, nil
	}
	c.crdMissingLogged = false

	return reconciler.Result{RequeueAfter: c.period}, nil
}

func (c *Controller) seedLaunchableFloor(ctx context.Context) {
	nodeClaims := &karpv1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaims); err != nil {
		log.FromContext(ctx).V(1).Info(
			"unable to list nodeclaims to seed the launchable floor",
			"reason", err.Error(),
		)
		return
	}

	for i := range nodeClaims.Items {
		nodeClaim := &nodeClaims.Items[i]

		if !nodeClaim.DeletionTimestamp.IsZero() {
			continue
		}
		if !nodeClaim.StatusConditions().Get(karpv1.ConditionTypeLaunched).IsTrue() {
			continue
		}
		name, ok := nodeClaim.Labels[corev1.LabelInstanceTypeStable]
		if !ok {
			continue
		}
		specs, err := instancetype.InstanceTypeToSpecs(name)
		if err != nil {
			continue
		}
		c.availability.SeedLaunchable(ctx, specs)
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

func (c *Controller) render(reading availability.Snapshot) v1alpha1.OxideCapacityStatus {
	rack, inventory := reading.Rack()
	now := timestamp(time.Now())

	status := v1alpha1.OxideCapacityStatus{
		ObservedAt: now,
		Inventory:  v1alpha1.InventoryStatus{State: string(inventory)},
	}

	for _, entry := range reading.Entries() {

		corner := entry.Reach
		if corner.Ncpus == 0 || corner.MemInGiB == 0 {
			corner = entry.Shape
		}
		unavailable := v1alpha1.UnavailableRange{
			Range:         rangeDescription(corner),
			FromNcpus:     corner.Ncpus,
			FromMemoryGiB: corner.MemInGiB,
			RefusedInstanceType: instancetype.SpecsToInstanceTypeName(
				entry.Shape.Ncpus,
				entry.Shape.MemInGiB,
			),
			Reason:              string(entry.Reason),
			State:               shapeState(entry.State),
			ConsecutiveRefusals: entry.ConsecutiveICEs,
		}
		if entry.Floor.Ncpus > 0 && entry.Floor.MemInGiB > 0 {
			unavailable.LargestLaunchable = instancetype.SpecsToInstanceTypeName(
				entry.Floor.Ncpus,
				entry.Floor.MemInGiB,
			)
		}
		unavailable.LastRefusal = timestamp(entry.LastICE)

		if entry.State == availability.StateBackoff {
			unavailable.RetryAfter = timestamp(entry.RetryAfter)
		}
		status.InstanceTypes.UnavailableRanges = append(
			status.InstanceTypes.UnavailableRanges,
			unavailable,
		)
	}

	if rack.Known() {
		renderRack(c.instanceTypes, rack, &status.Inventory)

		if inventory == availability.InventoryFresh {
			status.InstanceTypes.UnavailableRanges = append(
				status.InstanceTypes.UnavailableRanges,
				rackRanges(rack, status.Inventory.LargestFeasibleInstanceType)...,
			)
		}
	}

	sortRanges(status.InstanceTypes.UnavailableRanges)
	status.InstanceTypes.UnavailableRangeCount = len(status.InstanceTypes.UnavailableRanges)

	status.InstanceTypes.UnavailableCount, status.InstanceTypes.Count =
		c.instanceTypes.UnavailableInstanceTypeCount()
	if quota, err := c.instanceTypes.Quota(); err == nil {
		status.Quota = renderQuota(quota)
	}
	if refusal, ok := c.storage.Last(); ok {
		status.Storage = &v1alpha1.StorageRefusal{
			Reason:              string(refusal.Reason),
			Message:             refusal.Message,
			ConsecutiveRefusals: refusal.Consecutive,
			LastRefusal:         timestamp(refusal.LastRefusal),
		}
	}
	return status
}

func rackRanges(rack capacity.Snapshot, largestLaunchable string) []v1alpha1.UnavailableRange {
	headrooms := instancetype.RackHeadroom(rack)
	ranges := make([]v1alpha1.UnavailableRange, 0, len(headrooms))
	for _, headroom := range headrooms {
		ranges = append(ranges, v1alpha1.UnavailableRange{
			Range:        headroomDescription(headroom),
			MaxNcpus:     headroom.Ncpus,
			MaxMemoryGiB: headroom.MemInGiB,
			Reason:       v1alpha1.RangeReasonNoSledWithRoom,

			LargestLaunchable: largestLaunchable,
		})
	}
	return ranges
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

func renderQuota(quota utilization.Quota) *v1alpha1.QuotaHeadroom {
	return &v1alpha1.QuotaHeadroom{
		CPUs:       max(quota.CapacityCPUs-quota.ProvisionedCPUs, 0),
		MemoryGiB:  int64(headroom(quota.CapacityMemory, quota.ProvisionedMemory) / GiB),
		StorageGiB: int64(headroom(quota.CapacityStorage, quota.ProvisionedStorage) / GiB),
	}
}

func headroom(entitled, provisioned uint64) uint64 {
	if entitled <= provisioned {
		return 0
	}
	return entitled - provisioned
}

func (c *Controller) publish(ctx context.Context, status v1alpha1.OxideCapacityStatus) error {
	existing := &v1alpha1.OxideCapacity{}
	err := c.kubeClient.Get(ctx, client.ObjectKey{Name: v1alpha1.OxideCapacityName}, existing)
	switch {
	case kerrors.IsNotFound(err):
		created := &v1alpha1.OxideCapacity{
			ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.OxideCapacityName},
		}
		if err := c.kubeClient.Create(ctx, created); err != nil {
			return err
		}
		created.Status = status
		return c.kubeClient.Status().Update(ctx, created)
	case err != nil:
		return err
	}

	if unchanged(existing.Status, status) {
		return nil
	}
	existing.Status = status
	return c.kubeClient.Status().Update(ctx, existing)
}

func unchanged(published, current v1alpha1.OxideCapacityStatus) bool {
	if published.ObservedAt == nil || current.ObservedAt == nil {
		return false
	}
	if current.ObservedAt.Sub(published.ObservedAt.Time) >= heartbeat {
		return false
	}
	withoutTime := published
	withoutTime.ObservedAt = current.ObservedAt
	return equality.Semantic.DeepEqual(withoutTime, current)
}

func (c *Controller) logPublishFailure(ctx context.Context, err error) {
	if !crdMissing(err) {
		log.FromContext(ctx).V(1).Info(
			"unable to publish the capacity view, retrying next period",
			"reason", err.Error(),
		)
		return
	}
	if c.crdMissingLogged {
		log.FromContext(ctx).V(1).Info(
			"the OxideCapacity CRD is still not installed, capacity is reported by metrics only",
			"reason", err.Error(),
		)
		return
	}
	log.FromContext(ctx).Error(
		err,
		"the OxideCapacity CRD is not installed, so which instance shapes are unavailable is "+
			"reported by metrics only. Upgrade the karpenter-provider-oxide-crds chart to the same version as the "+
			"controller. Node provisioning is unaffected",
	)
	c.crdMissingLogged = true
}

func crdMissing(err error) bool {
	return meta.IsNoMatchError(err) ||
		runtime.IsNotRegisteredError(err) ||
		kerrors.IsNotFound(err)
}

func publishMetrics(
	status v1alpha1.OxideCapacityStatus,
	rack capacity.Snapshot,
	inventory availability.InventoryState,
) {
	metrics.ResetShapeGauges()
	metrics.ResetSledGauges()

	metrics.UnavailableShapes.Set(float64(status.InstanceTypes.UnavailableRangeCount))
	metrics.UnavailableInstanceTypes.Set(float64(status.InstanceTypes.UnavailableCount))
	metrics.InstanceTypes.Set(float64(status.InstanceTypes.Count))
	for _, unavailable := range status.InstanceTypes.UnavailableRanges {

		label := unavailable.RefusedInstanceType

		if label == "" {
			continue
		}
		metrics.ConsecutiveICEs.WithLabelValues(label).
			Set(float64(unavailable.ConsecutiveRefusals))
		remaining := time.Duration(0)
		if unavailable.RetryAfter != nil && status.ObservedAt != nil {
			remaining = unavailable.RetryAfter.Sub(status.ObservedAt.Time)
		}
		metrics.BackoffSeconds.WithLabelValues(label).Set(metrics.Seconds(remaining))

		if inventory != availability.InventoryFresh {
			continue
		}
		specs, err := instancetype.InstanceTypeToSpecs(label)
		if err != nil {
			continue
		}
		metrics.FeasibleInstances.WithLabelValues(label).Set(float64(
			rack.FeasibleCount(capacity.Resources{
				CPUs:   specs.Ncpus,
				Memory: uint64(specs.MemInGiB) * GiB,
			}),
		))
	}

	metrics.StorageRefusalsConsecutive.Set(0)
	if status.Storage != nil {
		metrics.StorageRefusalsConsecutive.Set(float64(status.Storage.ConsecutiveRefusals))
	}

	metrics.SnapshotFresh.Set(0)
	if status.Inventory.State == v1alpha1.InventoryFresh {
		metrics.SnapshotFresh.Set(1)
	}
	if status.Inventory.ObservedAt != nil && status.ObservedAt != nil {
		age := status.ObservedAt.Sub(status.Inventory.ObservedAt.Time)
		metrics.SnapshotAgeSeconds.Set(metrics.Seconds(age))
	}
	for _, sled := range status.Inventory.Sleds {
		metrics.SledFreeCPUs.WithLabelValues(sled.ID).Set(float64(sled.FreeCPUs))
		metrics.SledFreeMemoryBytes.WithLabelValues(sled.ID).
			Set(float64(sled.FreeMemoryGiB) * GiB)
	}
	if specs, err := instancetype.InstanceTypeToSpecs(
		status.Inventory.LargestFeasibleInstanceType,
	); err == nil {
		metrics.LargestFeasibleCPUs.Set(float64(specs.Ncpus))
		metrics.LargestFeasibleMemoryBytes.Set(float64(specs.MemInGiB) * GiB)
	}
}
