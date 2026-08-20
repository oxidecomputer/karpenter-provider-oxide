// Package affinity keeps a node's Oxide group memberships in step with its nodeclass's desired set,
// in the one direction that can be done to a running instance.
//
// Everything here follows from a single rule: an instance is a member of exactly the groups in its
// nodeclass's desired set, memberships are added only at launch, and are removed as soon as the group
// leaves the desired set.
//
// Additions belong to the launch because placement is fixed at start: joining a group afterwards
// records a membership against a sled that was chosen without it. That is what makes an addition a
// drift concern, handled by OxideCloudProvider.IsDrifted, rather than something this controller could
// fix in place.
//
// Removals are the opposite. Dropping a membership neither stops nor relocates the instance, so it
// happens here and now, and is not drift at all.
//
// Scoping removal to a NodeClaim controller is also what keeps Karpenter from touching memberships
// that are not its own. A selected group may hold instances from other nodeclasses or from outside
// Kubernetes entirely; iterating a group's members and deciding which to evict would have to
// reconstruct that ownership, whereas iterating NodeClaims has it by construction.
package affinity

import (
	"context"
	"errors"
	"fmt"
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

// resyncPeriod is how often a node's memberships are re-examined without an event to prompt it. The
// nodeclass is watched, so a change to the desired set is acted on immediately; this only covers a
// removal whose API call failed and has since become possible again.
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

// Reconcile drops every membership recorded on a NodeClaim that its nodeclass's desired set no longer
// contains, and rewrites the record. It never adds one.
func (c *Controller) Reconcile(
	ctx context.Context,
	nodeClaim *karpv1.NodeClaim,
) (reconcile.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	// A NodeClaim being torn down releases its memberships with the instance delete, so there is
	// nothing here worth racing the termination controller over.
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	recorded := v1alpha1.ParseAffinityGroups(
		nodeClaim.Annotations[v1alpha1.AnnotationAffinityGroups],
	)
	if len(recorded) == 0 {
		return reconcile.Result{}, nil
	}
	// The instance is what leaves the group, so a NodeClaim that has not launched has nothing to
	// remove. It cannot have a recorded membership either, since the launch is what records one.
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
		// A nodeclass that is gone is not a nodeclass that wants nothing. Dropping every membership
		// in the fleet because the object could not be read is exactly the failure mode the desired
		// set is published carefully to avoid.
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
			// Kept in the record. A membership that is still held has to stay addressable, or the
			// next reconcile has no way to find the group it failed to leave.
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

	if len(remaining) != len(recorded) {
		stored := nodeClaim.DeepCopy()
		nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{
			v1alpha1.AnnotationAffinityGroups: v1alpha1.EncodeAffinityGroups(remaining),
		})
		if err := c.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(
				fmt.Errorf("unable to record the NodeClaim's group memberships, %w", err),
			)
		}
	}
	return reconcile.Result{RequeueAfter: resyncPeriod}, nil
}

// leave removes one instance from one group, on whichever endpoint the group's kind lives behind.
//
// A group or a membership that is already gone counts as success: the outcome this is asking for has
// been reached, whoever reached it.
func (c *Controller) leave(
	ctx context.Context,
	ref v1alpha1.AffinityGroupRef,
	instanceID string,
) error {
	switch ref.Type {
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
		// A code this version does not understand, written by a newer one. Left in place rather than
		// guessed at, since guessing the endpoint wrong would report a membership as dropped that is
		// still held.
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
		// The desired set lives on the nodeclass, so a change to it is what makes memberships due for
		// removal. Watching it is also what empties a superseded managed group promptly, which is the
		// only thing that lets the nodeclass controller delete it.
		Watches(&v1alpha1.OxideNodeClass{}, c.nodeClassEventHandler()).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// nodeClassEventHandler enqueues every NodeClaim using a nodeclass whose desired set may have
// changed. The lookup goes through the spec.nodeClassRef field indexes Karpenter's operator
// registers, which is what makes this a lookup rather than a scan of every NodeClaim in the cluster.
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
