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

const missRequeue = 30 * time.Second

var conditionReasons = map[sled.UnhealthyReason]string{
	sled.ReasonExpunged:         v1alpha1.ConditionReasonSledExpunged,
	sled.ReasonNonProvisionable: v1alpha1.ConditionReasonSledNonProvisonable,
}

// Controller labels each node with the sled its instance is running on.
type Controller struct {
	kubeClient   client.Client
	sledProvider *sled.Provider

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

// Reconcile resolves the sled for one NodeClaim's instance and writes it to the NodeClaim and its Node.
func (c *Controller) Reconcile(
	ctx context.Context,
	nodeClaim *karpv1.NodeClaim,
) (reconcile.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	instanceID, err := cloudprovider.InstanceIDFromProviderID(nodeClaim.Status.ProviderID)
	if err != nil {
		return reconcile.Result{}, nil
	}

	placement, found, err := c.sledProvider.PlacementFor(instanceID)
	if err != nil || !found {

		if _, refreshErr := c.sledProvider.Refresh(ctx, false); refreshErr != nil {

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

func (c *Controller) applyToNode(
	ctx context.Context,
	nodeClaim *karpv1.NodeClaim,
	placement sled.Placement,
	labels, annotations map[string]string,
) error {
	node, err := nodeclaimutils.NodeForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {

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
		Watches(&corev1.Node{}, c.nodeEventHandler()).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

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

	if placement.Serial != "" {
		annotations[v1alpha1.LabelSledSerial] = placement.Serial
	}
	return labels, annotations
}

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

func merged(current, desired map[string]string) map[string]string {
	if current == nil {
		current = map[string]string{}
	}
	maps.Copy(current, desired)
	return current
}
