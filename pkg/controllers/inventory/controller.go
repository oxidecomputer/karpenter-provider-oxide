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

// Package inventory records which physical sled each node landed on.
//
// Nothing in the provider requires this. Reading it requires fleet-level access to the Oxide system
// API, which is a much broader grant than launching instances into one project, and an operator may
// reasonably decline to give it. So the whole package is opt-in, and everything downstream of it is
// built to work without it: with inventory disabled, or enabled against a token that cannot reach the
// system API, the provider launches nodes exactly as it does without this package.
//
// What the controllers here write is additive. The rack walk they drive is not: the same snapshot
// that produces these labels also tells the instance type provider what each sled has room for, which
// turns "no sled can take this shape" into a shape that is never offered rather than one discovered
// by a refused launch. See pkg/capacity.
//
// The placement is only knowable after the fact. Karpenter asks for an instance of a given size and
// the provision saga decides which sled has room for it, so a sled is not something a NodePool can
// select on, and these labels are deliberately not offering requirements. They are for reading:
// correlating a node with the hardware under it, spreading pods across sleds as a failure domain, and
// spotting nodes sitting on a sled that is being expunged from the rack.
package inventory

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cloudprovider"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/sled"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

// missRequeue is how long a NodeClaim whose instance is not in the snapshot waits before trying
// again. An instance launched since the last refresh is the common reason, so this only has to
// outlast a refresh, not a whole refresh period.
const missRequeue = 30 * time.Second

// conditionReasons maps the reason a sled is out of service to the Reason recorded on the Node
// condition, keeping the sled package free of Kubernetes vocabulary.
var conditionReasons = map[sled.UnhealthyReason]string{
	sled.ReasonExpunged:         v1alpha1.ConditionReasonSledExpunged,
	sled.ReasonNonProvisionable: v1alpha1.ConditionReasonSledNonProvisonable,
}

// Controller labels each node with the sled its instance is running on.
type Controller struct {
	kubeClient   client.Client
	sledProvider *sled.Provider
	// resyncPeriod is how often a placed node is re-examined. Sled state and policy change under a
	// running node, so a node that is already labeled still has to be revisited.
	resyncPeriod time.Duration
}

func NewController(
	kubeClient client.Client,
	sledProvider *sled.Provider,
	resyncPeriod time.Duration,
) *Controller {
	return &Controller{
		kubeClient:   kubeClient,
		sledProvider: sledProvider,
		resyncPeriod: resyncPeriod,
	}
}

func (c *Controller) Name() string {
	return "inventory"
}

// Reconcile resolves the sled for one NodeClaim's instance and writes it to the NodeClaim and its
// Node.
//
// Errors here are only ever about writing to the cluster. A placement that cannot be resolved is not
// an error: the instance may have been launched since the last snapshot, or the system API may be
// unreadable entirely, and neither is something this controller should escalate. Both simply leave
// the node unlabeled and requeue.
func (c *Controller) Reconcile(
	ctx context.Context,
	nodeClaim *karpv1.NodeClaim,
) (reconcile.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	// A NodeClaim being torn down is about to take its Node with it, so labeling it is wasted work at
	// best and a conflict with the termination controller at worst.
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	// Only instances this provider launched can be looked up, and a NodeClaim that has not launched
	// yet has no instance to look up at all.
	instanceID, err := cloudprovider.InstanceIDFromProviderID(nodeClaim.Status.ProviderID)
	if err != nil {
		return reconcile.Result{}, nil
	}

	placement, found, err := c.sledProvider.PlacementFor(instanceID)
	if err != nil || !found {
		// A miss can mean the instance is newer than the snapshot, so ask for a refresh. The provider
		// rate limits this, so every unplaceable node in the cluster asking at once still costs at most
		// one rack walk.
		if _, refreshErr := c.sledProvider.Refresh(ctx, false); refreshErr != nil {
			// Already reported by the refresh controller, which owns saying why the system API is
			// unreadable. Repeating it per NodeClaim would bury everything else in the log.
			log.FromContext(ctx).V(1).Info(
				"unable to refresh the sled inventory to place a node",
				"reason", refreshErr.Error(),
			)
			return reconcile.Result{RequeueAfter: c.resyncPeriod}, nil
		}
		placement, found, err = c.sledProvider.PlacementFor(instanceID)
	}
	if err != nil || !found {
		log.FromContext(ctx).V(1).Info(
			"no sled placement is known for this instance yet",
			"instance-id", instanceID,
		)
		return reconcile.Result{RequeueAfter: missRequeue}, nil
	}

	observedAt := time.Now()
	if age, ok := c.sledProvider.SnapshotAge(observedAt); ok {
		observedAt = observedAt.Add(-age)
	}
	labels, annotations := decorations(placement, observedAt)

	if err := c.applyToNodeClaim(ctx, nodeClaim, annotations); err != nil {
		return reconcile.Result{}, err
	}
	if err := c.applyToNode(ctx, nodeClaim, placement, labels, annotations); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: c.resyncPeriod}, nil
}

// applyToNodeClaim records the placement on the NodeClaim as annotations only.
//
// The NodeClaim's labels are the scheduling contract Karpenter provisioned against, and the
// disruption and drift controllers compare them against the NodePool's requirements. Placement is
// discovered after provisioning and is not part of that contract, so it is kept out of the labels
// entirely; the Node carries the selectable copy.
func (c *Controller) applyToNodeClaim(
	ctx context.Context,
	nodeClaim *karpv1.NodeClaim,
	annotations map[string]string,
) error {
	if !needsPatch(nodeClaim.Annotations, annotations) {
		return nil
	}

	stored := nodeClaim.DeepCopy()
	nodeClaim.Annotations = merged(nodeClaim.Annotations, annotations)
	if err := c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("unable to annotate the NodeClaim: %w", err))
	}
	return nil
}

// applyToNode writes the placement to the Node, where the labels are selectable, and records the
// sled's health as a status condition.
func (c *Controller) applyToNode(
	ctx context.Context,
	nodeClaim *karpv1.NodeClaim,
	placement sled.Placement,
	labels, annotations map[string]string,
) error {
	node, err := nodeclaimutils.NodeForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {
		// A NodeClaim that has launched but whose node has not registered yet, or that briefly has two.
		// Both resolve on their own, and the resync picks the node up once it does.
		if nodeclaimutils.IsNodeNotFoundError(err) || nodeclaimutils.IsDuplicateNodeError(err) {
			return nil
		}
		return fmt.Errorf("unable to find the node for the NodeClaim: %w", err)
	}

	if needsPatch(node.Labels, labels) || needsPatch(node.Annotations, annotations) {
		stored := node.DeepCopy()
		node.Labels = merged(node.Labels, labels)
		node.Annotations = merged(node.Annotations, annotations)
		if err := c.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			if kerrors.IsConflict(err) {
				// The node is patched by the kubelet and by Karpenter's own lifecycle controllers, so
				// losing a race here is routine and only means retrying.
				return err
			}
			return client.IgnoreNotFound(fmt.Errorf("unable to label the node: %w", err))
		}
		log.FromContext(ctx).V(1).Info(
			"recorded sled placement on the node",
			"node", node.Name,
			"sled-id", labels[v1alpha1.LabelSledID],
		)
	}

	return c.applyCondition(ctx, node, placement)
}

// applyCondition records whether the node's sled is being taken out of service, as a Node status
// condition.
//
// This is the one thing here that can cause a node to be replaced, and only in combination with
// Karpenter's NodeRepair feature gate: core's health controller matches Node conditions against the
// cloud provider's RepairPolicies, waits out the toleration duration, and then deletes the NodeClaim,
// which drains the node and provisions a replacement. Writing the condition when the gate is off is
// inert, so it is written unconditionally and reads as sled health either way.
//
// The condition is only ever derived from a placement actually resolved from the API. A node whose
// placement is unknown never reaches here, so an unreadable system API cannot mark anything unhealthy.
func (c *Controller) applyCondition(
	ctx context.Context,
	node *corev1.Node,
	placement sled.Placement,
) error {
	unhealthy, reason, message := placement.Unhealthy()
	desired := corev1.NodeCondition{
		Type:    v1alpha1.ConditionTypeSledUnhealthy,
		Status:  corev1.ConditionFalse,
		Reason:  v1alpha1.ConditionReasonSledHealthy,
		Message: "the sled is in service",
	}
	if unhealthy {
		desired.Status = corev1.ConditionTrue
		desired.Reason = conditionReasons[reason]
		desired.Message = message
	}

	current := nodeutils.GetCondition(node, v1alpha1.ConditionTypeSledUnhealthy)
	// Only the status is compared. Rewriting on a changed message would reset LastTransitionTime, and
	// that timestamp is what core measures the repair toleration duration from: a node would then never
	// age into being repaired.
	if current.Status == desired.Status {
		return nil
	}

	now := metav1.Now()
	desired.LastTransitionTime = now
	desired.LastHeartbeatTime = now

	stored := node.DeepCopy()
	node.Status.Conditions = upsertCondition(node.Status.Conditions, desired)
	if err := c.kubeClient.Status().Patch(ctx, node, client.MergeFrom(stored)); err != nil {
		if kerrors.IsConflict(err) {
			return err
		}
		return client.IgnoreNotFound(
			fmt.Errorf("unable to set the %s condition on the node: %w",
				v1alpha1.ConditionTypeSledUnhealthy, err),
		)
	}

	if unhealthy {
		// Logged at info because this is the point a node becomes a candidate for replacement, which an
		// operator should be able to find without turning up the log level.
		log.FromContext(ctx).Info(
			"marking the node unhealthy because its sled is being taken out of service",
			"node", node.Name,
			"sled-id", placement.SledID,
			"sled-policy", placement.Policy,
			"sled-provision-policy", placement.ProvisionPolicy,
			"reason", reason,
		)
	} else {
		log.FromContext(ctx).V(1).Info(
			"the node's sled is in service",
			"node", node.Name,
			"sled-id", placement.SledID,
		)
	}
	return nil
}

// upsertCondition replaces a condition of the same type, or appends it, leaving every other condition
// on the node untouched.
func upsertCondition(
	conditions []corev1.NodeCondition,
	desired corev1.NodeCondition,
) []corev1.NodeCondition {
	for i, condition := range conditions {
		if condition.Type == desired.Type {
			conditions[i] = desired
			return conditions
		}
	}
	return append(conditions, desired)
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&karpv1.NodeClaim{}).
		// Nodes are watched so that a node registering, or having these labels stripped off it, is
		// noticed without waiting out a resync period.
		Watches(&corev1.Node{}, c.nodeEventHandler()).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// nodeEventHandler maps a Node back to the NodeClaim that owns it, so that a node registering or
// losing these labels is reconciled promptly.
//
// The lookup goes through the status.providerID index Karpenter's operator registers, which is what
// makes this a lookup rather than a scan of every NodeClaim on every node update.
func (c *Controller) nodeEventHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, o client.Object) []reconcile.Request {
			node, ok := o.(*corev1.Node)
			if !ok || node.Spec.ProviderID == "" {
				return nil
			}
			nodeClaims := &karpv1.NodeClaimList{}
			if err := c.kubeClient.List(
				ctx,
				nodeClaims,
				nodeclaimutils.ForProviderID(node.Spec.ProviderID),
			); err != nil {
				return nil
			}
			return lo.Map(
				nodeClaims.Items,
				func(nodeClaim karpv1.NodeClaim, _ int) reconcile.Request {
					return reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&nodeClaim),
					}
				},
			)
		},
	)
}

// decorations renders a placement into the labels and annotations to write.
//
// Values that cannot legally be a label value are dropped rather than truncated or rewritten: a
// baseboard serial is only useful if it is the actual serial, and a mangled one that looks real is
// worse than an absent one. The annotation copy is unconstrained, so nothing is lost.
func decorations(
	placement sled.Placement,
	observedAt time.Time,
) (labels map[string]string, annotations map[string]string) {
	labels = map[string]string{}
	for key, value := range map[string]string{
		v1alpha1.LabelSledID:          placement.SledID,
		v1alpha1.LabelSledRackID:      placement.RackID,
		v1alpha1.LabelSledSerial:      placement.Serial,
		v1alpha1.LabelSledState:       placement.State,
		v1alpha1.LabelSledPolicy:      placement.Policy,
		v1alpha1.LabelSledProvisiable: placement.ProvisionPolicy,
	} {
		if value == "" || len(validation.IsValidLabelValue(value)) > 0 {
			continue
		}
		labels[key] = value
	}

	annotations = map[string]string{
		v1alpha1.AnnotationSledObservedAt: observedAt.UTC().Format(time.RFC3339),
	}
	if placement.Part != "" {
		annotations[v1alpha1.AnnotationSledBaseboardPart] = placement.Part
	}
	if placement.Revision != 0 {
		annotations[v1alpha1.AnnotationSledBaseboardRevision] = strconv.Itoa(placement.Revision)
	}
	if placement.UsableHardwareThreads != 0 {
		annotations[v1alpha1.AnnotationSledHardwareThreads] = strconv.Itoa(
			placement.UsableHardwareThreads,
		)
	}
	if placement.UsablePhysicalRam != 0 {
		annotations[v1alpha1.AnnotationSledPhysicalRAM] = strconv.FormatUint(
			placement.UsablePhysicalRam, 10,
		)
	}
	// The serial is the operator-facing handle on the sled, so it is also annotated for the case where
	// it was too unusual to be a legal label value.
	if placement.Serial != "" {
		annotations[v1alpha1.LabelSledSerial] = placement.Serial
	}
	return labels, annotations
}

// needsPatch reports whether any desired entry is missing or different, ignoring the observation
// timestamp on its own.
//
// The timestamp changes on every refresh, so treating it as a difference would rewrite every node in
// the cluster every period for no new information. It is only worth writing alongside a placement
// that actually changed, which is exactly when it is meaningful.
func needsPatch(current, desired map[string]string) bool {
	for key, value := range desired {
		if key == v1alpha1.AnnotationSledObservedAt {
			continue
		}
		if current[key] != value {
			return true
		}
	}
	return false
}

// merged returns current with desired applied over it, leaving keys this controller does not own
// alone.
func merged(current, desired map[string]string) map[string]string {
	if current == nil {
		current = map[string]string{}
	}
	maps.Copy(current, desired)
	return current
}
