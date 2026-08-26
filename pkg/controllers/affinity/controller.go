// Package affinity reconciles node affinity groups.
package affinity

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cloudprovider"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

const resyncPeriod = 10 * time.Minute

// Controller removes the group memberships a node's nodeclass no longer wants.
type Controller struct {
	kubeClient client.Client
	ox         *oxide.Client
}

func NewController(kubeClient client.Client, ox *oxide.Client) *Controller {
	return &Controller{kubeClient: kubeClient, ox: ox}
}

func (c *Controller) Name() string {
	return "nodeclaim-affinity"
}

// Reconcile drops every membership recorded on a NodeClaim that its nodeclass's desired set no longer contains.
func (c *Controller) Reconcile(
	ctx context.Context,
	nodeClaim *karpv1.NodeClaim,
) (reconcile.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	recorded := v1alpha1.ParseAffinityGroups(
		nodeClaim.Annotations[v1alpha1.AnnotationAffinityGroups],
	)
	if len(recorded) == 0 {

		return reconcile.Result{}, c.recordOnNode(ctx, nodeClaim, "")
	}

	instanceID, err := cloudprovider.InstanceIDFromProviderID(nodeClaim.Status.ProviderID)
	if err != nil {
		return reconcile.Result{}, nil
	}

	nodeClass := &v1alpha1.OxideNodeClass{}
	if err := c.kubeClient.Get(
		ctx,
		client.ObjectKey{Name: nodeClaim.Spec.NodeClassRef.Name},
		nodeClass,
	); err != nil {

		if kerrors.IsNotFound(err) {
			return reconcile.Result{RequeueAfter: resyncPeriod}, nil
		}
		return reconcile.Result{}, fmt.Errorf("unable to retrieve the OxideNodeClass, %w", err)
	}
	desired := v1alpha1.AffinityGroupIDs(nodeClass.Status.AffinityGroups)

	var errs []error
	remaining := make([]v1alpha1.AffinityGroupRef, 0, len(recorded))
	for _, ref := range recorded {
		if desired.Has(ref.ID) {
			remaining = append(remaining, ref)
			continue
		}
		if err := c.leave(ctx, ref, instanceID); err != nil {

			remaining = append(remaining, ref)
			errs = append(errs, err)
			continue
		}
		log.FromContext(ctx).Info(
			"removed a group membership the nodeclass no longer wants",
			"group-id", ref.ID,
			"group-type", ref.Type,
			"instance-id", instanceID,
		)
	}
	if err := errors.Join(errs...); err != nil {
		return reconcile.Result{}, err
	}

	value := v1alpha1.EncodeAffinityGroups(remaining)
	if err := c.record(ctx, nodeClaim, value); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(
			fmt.Errorf("unable to record the NodeClaim's group memberships, %w", err),
		)
	}
	if err := c.recordOnNode(ctx, nodeClaim, value); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: resyncPeriod}, nil
}

func (c *Controller) recordOnNode(
	ctx context.Context,
	nodeClaim *karpv1.NodeClaim,
	value string,
) error {
	node, err := nodeclaimutils.NodeForNodeClaim(ctx, c.kubeClient, nodeClaim)
	if err != nil {

		if nodeclaimutils.IsNodeNotFoundError(err) || nodeclaimutils.IsDuplicateNodeError(err) {
			return nil
		}
		return fmt.Errorf("unable to find the node for the NodeClaim: %w", err)
	}
	if err := c.record(ctx, node, value); err != nil {
		if kerrors.IsConflict(err) {

			return err
		}
		return client.IgnoreNotFound(
			fmt.Errorf("unable to record the node's group memberships, %w", err),
		)
	}
	return nil
}

func (c *Controller) record(ctx context.Context, obj client.Object, value string) error {
	annotations := obj.GetAnnotations()
	current, present := annotations[v1alpha1.AnnotationAffinityGroups]
	switch {
	case value == "" && !present:
		return nil
	case value != "" && current == value:
		return nil
	}

	stored, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("unable to copy a %T to patch it", obj)
	}
	updated := maps.Clone(annotations)
	if updated == nil {
		updated = map[string]string{}
	}
	if value == "" {
		delete(updated, v1alpha1.AnnotationAffinityGroups)
	} else {
		updated[v1alpha1.AnnotationAffinityGroups] = value
	}
	obj.SetAnnotations(updated)
	return c.kubeClient.Patch(ctx, obj, client.MergeFrom(stored))
}

func (c *Controller) leave(
	ctx context.Context,
	ref v1alpha1.AffinityGroupRef,
	instanceID string,
) error {
	switch ref.Type {
	case v1alpha1.AffinityGroupTypeAffinity:
		err := c.ox.ExperimentalAffinityGroupMemberInstanceDelete(
			ctx,
			oxide.AffinityGroupMemberInstanceDeleteParams{
				AffinityGroup: oxide.NameOrId(ref.ID),
				Instance:      oxide.NameOrId(instanceID),
			},
		)
		if err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
			return fmt.Errorf(
				"unable to remove instance %s from affinity group %s, %w",
				instanceID, ref.ID, err,
			)
		}
		return nil
	case v1alpha1.AffinityGroupTypeAntiAffinity:
		err := c.ox.AntiAffinityGroupMemberInstanceDelete(
			ctx,
			oxide.AntiAffinityGroupMemberInstanceDeleteParams{
				AntiAffinityGroup: oxide.NameOrId(ref.ID),
				Instance:          oxide.NameOrId(instanceID),
			},
		)
		if err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
			return fmt.Errorf(
				"unable to remove instance %s from anti-affinity group %s, %w",
				instanceID, ref.ID, err,
			)
		}
		return nil
	default:

		log.FromContext(ctx).V(1).Info(
			"leaving a recorded group membership of an unrecognized kind alone",
			"group-id", ref.ID,
		)
		return nil
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&karpv1.NodeClaim{}).
		Watches(&v1alpha1.OxideNodeClass{}, c.nodeClassEventHandler()).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func (c *Controller) nodeClassEventHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, o client.Object) []reconcile.Request {
			nodeClass, ok := o.(*v1alpha1.OxideNodeClass)
			if !ok {
				return nil
			}
			nodeClaims := &karpv1.NodeClaimList{}
			if err := c.kubeClient.List(
				ctx,
				nodeClaims,
				nodeclaimutils.ForNodeClass(nodeClass),
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
