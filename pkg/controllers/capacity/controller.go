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

// Package capacity publishes what the provider believes about capacity, as an object and as metrics.
//
// The state it reports lives in memory in the instance type provider and moves on the timescale of
// launches: which shapes have been refused, how long each is waiting, what the rack had room for when
// it was last read. None of that is discoverable without reading logs, which is the wrong way to
// answer "why is nothing scaling up" - the question is about the present, and a log is a record of the
// past.
//
// So this samples rather than streams. It renders the current state on a fixed interval and writes it
// only when it has changed, which keeps a rack that is refusing launches steadily from turning into a
// stream of writes to etcd. Nothing reads the object back; it exists to be looked at.
package capacity

import (
	"context"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
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
	// GiB is how memory is reported here. Shapes are named in whole GiB, so bytes would make the view
	// harder to read against the instance types it describes.
	GiB = 1 << 30

	// heartbeat is how stale the published observedAt is allowed to get while nothing else changes.
	//
	// Without it, a view that has not changed would never be rewritten and its timestamp would age
	// indefinitely, leaving no way to tell a quiet cluster from a controller that has stopped
	// publishing. With it, that distinction costs one write per heartbeat.
	heartbeat = 5 * time.Minute
)

// Controller publishes the capacity view.
type Controller struct {
	kubeClient    client.Client
	instanceTypes *instancetype.Provider
	period        time.Duration
	// crdMissingLogged records that the CRD has already been reported as absent. It is the expected
	// state between upgrading the controller and upgrading the CRD chart, and it recurs every period,
	// so it is said once at error and quietly after that.
	crdMissingLogged bool
}

func NewController(
	kubeClient client.Client,
	instanceTypes *instancetype.Provider,
	period time.Duration,
) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		instanceTypes: instanceTypes,
		period:        period,
	}
}

func (c *Controller) Name() string {
	return "capacity"
}

// Reconcile renders the current state, publishes the metrics from it, and writes the object if it has
// changed.
//
// It never returns an error. This is a view: nothing launches nodes off the back of it, and an
// apiserver that will not accept it, or a CRD that has not been installed yet, must not put the
// controller into backoff or report the provider unhealthy. The metrics are updated either way, since
// they do not depend on the write succeeding.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	// Done before rendering so the view describes a provider that has already been told what is
	// running. Nodes that are up are the only evidence of a launchable shape that outlives a restart,
	// and the search needs it before the first refusal rather than after.
	c.seedLaunchableFloor(ctx)

	// Read once and rendered from that reading. A list of unavailable shapes taken before a rack
	// reading taken after it could report a shape as refused for capacity next to a rack that plainly
	// has room for it.
	rack, inventory := c.instanceTypes.Rack()
	status := c.render(rack, inventory)
	publishMetrics(status, rack, inventory)

	if err := c.publish(ctx, status); err != nil {
		c.logPublishFailure(ctx, err)
		return reconciler.Result{RequeueAfter: c.period}, nil
	}
	c.crdMissingLogged = false

	return reconciler.Result{RequeueAfter: c.period}, nil
}

// seedLaunchableFloor teaches the instance type provider which shapes the cluster is running.
//
// The refusal search descends by halving until something launches under it, and across a restart
// nothing has: the largest shape known to launch is empty, so one refusal of a mid-size shape rules
// out everything at or above half of it -- including, in the case this was written for, the shape
// every node in the cluster was running. A registered NodeClaim is proof the rack placed its shape,
// and it is proof this process did not have to make itself.
//
// Errors are swallowed. A floor is an optimisation on the search, not a correctness requirement, and
// failing to read NodeClaims is not a reason to stop publishing the capacity view below.
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
		// A NodeClaim on its way out may well be going because its instance stopped being placeable, and
		// one that never launched proves nothing at all.
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
		c.instanceTypes.SeedLaunchable(ctx, specs)
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

// render turns the provider's current state into the published status.
func (c *Controller) render(
	rack capacity.Snapshot,
	inventory instancetype.InventoryState,
) v1alpha1.OxideCapacityStatus {
	now := timestamp(time.Now())

	status := v1alpha1.OxideCapacityStatus{
		ObservedAt: now,
		Inventory:  v1alpha1.InventoryStatus{State: string(inventory)},
	}

	for _, entry := range c.instanceTypes.Unavailable() {
		// The corner is where ruling out begins, which is the entry's reach rather than the shape it
		// refused. During a walk down through sizes those differ, and the reach is the one that
		// describes what the scheduler is actually not being offered.
		corner := entry.Reach
		if corner.Ncpus == 0 || corner.MemInGiB == 0 {
			corner = entry.Specs
		}
		unavailable := v1alpha1.UnavailableRange{
			Range:         rangeDescription(corner),
			FromNcpus:     corner.Ncpus,
			FromMemoryGiB: corner.MemInGiB,
			RefusedInstanceType: instancetype.SpecsToInstanceTypeName(
				entry.Specs.Ncpus,
				entry.Specs.MemInGiB,
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
		// Only meaningful while the range is still waiting. A half-open one is being offered now, and
		// reporting the moment that started would read as a retry that is still pending.
		if entry.State == instancetype.StateBackoff {
			unavailable.RetryAfter = timestamp(entry.RetryAfter)
		}
		status.InstanceTypes.UnavailableRanges = append(
			status.InstanceTypes.UnavailableRanges,
			unavailable,
		)
	}

	if rack.Known() {
		renderRack(rack, &status.Inventory)
		// Ranges are published only while the reading is worth believing. A stale reading is ignored
		// when shapes are judged, so publishing ranges from it would describe a constraint that is not
		// being applied. The reading itself is still reported, next to the state that says as much.
		if inventory == instancetype.InventoryFresh {
			status.InstanceTypes.UnavailableRanges = append(
				status.InstanceTypes.UnavailableRanges,
				rackRanges(rack, status.Inventory.LargestFeasibleInstanceType)...,
			)
		}
	}

	// Sorted so that the object does not churn on map iteration order, which would rewrite it every
	// period with the same contents in a different arrangement.
	sortRanges(status.InstanceTypes.UnavailableRanges)
	status.InstanceTypes.UnavailableRangeCount = len(status.InstanceTypes.UnavailableRanges)
	// Reported alongside the boundaries rather than instead of them, because they answer different
	// questions: the list says where the edges are, and this says what they cost the scheduler. One
	// range covering every shape above it makes those two numbers differ by orders of magnitude.
	status.InstanceTypes.UnavailableCount, status.InstanceTypes.Count =
		c.instanceTypes.UnavailableInstanceTypeCount()
	if quota, err := c.instanceTypes.Quota(); err == nil {
		status.Quota = renderQuota(quota)
	}
	if refusal, ok := c.instanceTypes.StorageRefusal(); ok {
		status.Storage = &v1alpha1.StorageRefusal{
			Reason:              string(refusal.Reason),
			Message:             refusal.Message,
			ConsecutiveRefusals: refusal.Consecutive,
			LastRefusal:         timestamp(refusal.LastRefusal),
		}
	}
	return status
}

// rackRanges is what the rack reading itself rules out, as ranges alongside the refused ones. Each is
// one class of sled's headroom, so a rack of like sleds contributes exactly one entry.
//
// This is the difference between the two numbers an operator is most likely to be staring at. A rack
// whose emptiest sled holds 128 CPUs and 880 GiB leaves 656 of 1104 instance types unavailable and not
// one refusal to show for it, because nothing was refused: the shapes were never offered. Before this,
// the only field that said anything about it was largestFeasibleInstanceType, and reading a count of
// 656 off a single shape name is work nobody should be asked to do.
//
// Only called for a fresh reading, since a stale one rules nothing out and publishing ranges from it
// would describe a constraint that is not being applied.
func rackRanges(rack capacity.Snapshot, largestLaunchable string) []v1alpha1.UnavailableRange {
	headrooms := instancetype.RackHeadroom(rack)
	ranges := make([]v1alpha1.UnavailableRange, 0, len(headrooms))
	for _, headroom := range headrooms {
		ranges = append(ranges, v1alpha1.UnavailableRange{
			Range:        headroomDescription(headroom),
			MaxNcpus:     headroom.Ncpus,
			MaxMemoryGiB: headroom.MemInGiB,
			Reason:       v1alpha1.RangeReasonNoSledWithRoom,
			// The other end of the same boundary, and the field that makes a range actionable: the
			// largest shape the rack does have room for right now.
			LargestLaunchable: largestLaunchable,
		})
	}
	return ranges
}

// renderRack fills in the per-sled view, leaving the state already on it alone.
func renderRack(rack capacity.Snapshot, rendered *v1alpha1.InventoryStatus) {
	rendered.ObservedAt = timestamp(rack.TakenAt())

	for _, sled := range rack.Sleds() {
		free := sled.Free()
		// A sled that accepts no placements reports no headroom, because that is what it has to offer.
		// Reporting its real free space next to "provisionable: false" would invite the arithmetic that
		// the feasibility calculation deliberately refuses to do.
		if !sled.Provisionable {
			free = capacity.Resources{}
		}
		entry := v1alpha1.SledCapacity{
			ID:            sled.ID,
			Provisionable: sled.Provisionable,
			FreeCPUs:      free.CPUs,
			FreeMemoryGiB: int(free.Memory / GiB),
		}
		if shape, ok := instancetype.LargestShapeThatFits(free); ok {
			entry.LargestFeasibleInstanceType = instancetype.SpecsToInstanceTypeName(
				shape.Ncpus,
				shape.MemInGiB,
			)
		}
		rendered.Sleds = append(rendered.Sleds, entry)
	}
	sortSleds(rendered.Sleds)

	// Chosen across the whole rack by the provider, which owns both the shape catalog and the ordering
	// that makes "largest" mean something when neither CPU nor memory dominates.
	if shape, ok := instancetype.LargestShapeOnRack(rack); ok {
		rendered.LargestFeasibleInstanceType = instancetype.SpecsToInstanceTypeName(
			shape.Ncpus,
			shape.MemInGiB,
		)
	}
}

// timestamp is how every time in the view is built: truncated to the second, and nil for a zero time.
//
// Truncating is what makes the published view comparable to the one being rendered. Times serialize at
// second granularity, so a value read back from the apiserver has lost its sub-second part, and
// comparing it against a freshly rendered nanosecond would report a change on every single period and
// rewrite the object forever.
func timestamp(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	truncated := metav1.NewTime(t.Truncate(time.Second))
	return &truncated
}

// renderQuota turns the silo's quota into the headroom left in it, in the units the rest of the view
// uses.
func renderQuota(quota utilization.Quota) *v1alpha1.QuotaHeadroom {
	return &v1alpha1.QuotaHeadroom{
		CPUs:       max(quota.CapacityCPUs-quota.ProvisionedCPUs, 0),
		MemoryGiB:  int64(headroom(quota.CapacityMemory, quota.ProvisionedMemory) / GiB),
		StorageGiB: int64(headroom(quota.CapacityStorage, quota.ProvisionedStorage) / GiB),
	}
}

// headroom is what is left of an unsigned quota dimension, floored at zero: a silo can be provisioned
// beyond a lowered quota, and that has to read as nothing left rather than wrap around.
func headroom(entitled, provisioned uint64) uint64 {
	if entitled <= provisioned {
		return 0
	}
	return entitled - provisioned
}

// publish writes the status, creating the object first if nothing has published it yet.
//
// The write is skipped when nothing has changed, which is the common case: a stable cluster publishes
// once and then only heartbeats. Comparison ignores the timestamp, since that moves on every render
// and would make every period a change.
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

// unchanged reports whether the published status still describes the current state closely enough to
// leave alone, which is to say it is identical apart from when it was observed, and was observed
// recently enough to show the controller is alive.
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

// logPublishFailure explains a failed write, saying the interesting one loudly exactly once.
//
// The interesting one is the CRD not being installed. It is what an operator sees when they upgrade
// the controller without upgrading the CRD chart, and the fix is a chart upgrade rather than anything
// to do with capacity, so it deserves to be said plainly rather than buried in a generic error every
// period.
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
			"reported by metrics only. Upgrade the karpenter-crd chart to the same version as the "+
			"controller. Node provisioning is unaffected",
	)
	c.crdMissingLogged = true
}

// crdMissing reports whether a failure is the OxideCapacity type not being usable at all, as opposed
// to a write that could succeed later.
//
// It arrives in more than one shape. A cluster without the CRD installed has no REST mapping for the
// kind, and a client built against a scheme that does not know the type cannot encode it. Both mean
// the same thing to an operator and have the same fix, so both are reported the same way.
func crdMissing(err error) bool {
	return meta.IsNoMatchError(err) ||
		runtime.IsNotRegisteredError(err) ||
		kerrors.IsNotFound(err)
}

// publishMetrics mirrors the rendered view into the metric series.
//
// The per-shape and per-sled series are reset first: a shape that is no longer unavailable has no
// value to write, and leaving its last one behind would report a countdown that finished long ago as
// though it were still running.
func publishMetrics(
	status v1alpha1.OxideCapacityStatus,
	rack capacity.Snapshot,
	inventory instancetype.InventoryState,
) {
	metrics.ResetShapeGauges()
	metrics.ResetSledGauges()

	metrics.UnavailableShapes.Set(float64(status.InstanceTypes.UnavailableRangeCount))
	metrics.UnavailableInstanceTypes.Set(float64(status.InstanceTypes.UnavailableCount))
	metrics.InstanceTypes.Set(float64(status.InstanceTypes.Count))
	for _, unavailable := range status.InstanceTypes.UnavailableRanges {
		// Labelled by the shape that was refused rather than by the bounds of the range. Those
		// move as the search narrows, which would leave a new series behind on every step, whereas the
		// refused shape is what the entry is keyed on and only changes when the entry does.
		label := unavailable.RefusedInstanceType
		// A range read off sled inventory refused nothing, so there is no shape to label it with and
		// nothing per-shape to say: no refusal count, no backoff, and a feasible count that is zero by
		// construction. What it contributes is already in the counts above.
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
		// Only worth publishing while the reading is being acted on, and only for the shape that was
		// actually refused: a zero here next to a backoff says the rack agrees the shape does not fit,
		// and a non-zero says the refusal came from something the reading cannot see. Asking it of the
		// range bounds instead would answer for a shape nothing ever tried.
		if inventory != instancetype.InventoryFresh {
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

	// Set on every publish rather than only when there is a refusal, so that storage becoming available
	// again shows up as a zero instead of as a series that stopped reporting.
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
