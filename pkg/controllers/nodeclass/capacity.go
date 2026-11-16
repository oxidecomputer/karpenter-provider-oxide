package nodeclass

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
)

// CapacityController records the memory registered nodes report on their nodeclass, so
// the catalog stops estimating a shape and image once one node has shown the real figure.
type CapacityController struct {
	kubeClient client.Client
}

func NewCapacityController(kubeClient client.Client) *CapacityController {
	return &CapacityController{kubeClient: kubeClient}
}

func (c *CapacityController) Name() string {
	return "oxidenodeclass.capacity"
}

func (c *CapacityController) Reconcile(
	ctx context.Context,
	node *corev1.Node,
) (reconcile.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	instanceType := node.Labels[corev1.LabelInstanceTypeStable]
	memory, ok := node.Status.Capacity[corev1.ResourceMemory]
	if !registered(node) || instanceType == "" || !ok || memory.Sign() <= 0 {
		return reconcile.Result{}, nil
	}
	nodeClaim, err := nodeutils.NodeClaimForNode(ctx, c.kubeClient, node)
	if err != nil {
		if nodeutils.IsDuplicateNodeClaimError(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, nodeutils.IgnoreNodeClaimNotFoundError(err)
	}
	ref := nodeClaim.Spec.NodeClassRef
	if ref == nil || ref.Group != apis.Group || nodeClaim.Status.ImageID == "" {
		return reconcile.Result{}, nil
	}
	nodeClass := &v1alpha1.OxideNodeClass{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: ref.Name}, nodeClass); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !nodeClass.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	stored := nodeClass.DeepCopy()
	observeCapacity(nodeClass, instanceType, nodeClaim.Status.ImageID, memory)
	if equality.Semantic.DeepEqual(stored, nodeClass) {
		return reconcile.Result{}, nil
	}
	if err := c.kubeClient.Status().
		Patch(ctx, nodeClass, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
		if kerrors.IsConflict(err) {
			return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return reconcile.Result{}, client.IgnoreNotFound(
			fmt.Errorf("unable to record the node's capacity on its nodeclass, %w", err),
		)
	}
	log.FromContext(ctx).Info(
		"recorded the memory a node registered with, launches of its shape now use it",
		"node", node.Name,
		"nodeclass", nodeClass.Name,
		"instance-type", instanceType,
		"image-id", nodeClaim.Status.ImageID,
		"memory", memory.String(),
	)
	return reconcile.Result{}, nil
}

// observeCapacity keeps the smallest memory seen per shape and image, and drops
// entries for images the nodeclass no longer resolves. A node launched from an image
// already gone from status is skipped, since nothing would read it.
func observeCapacity(
	nodeClass *v1alpha1.OxideNodeClass,
	instanceType, imageID string,
	memory resource.Quantity,
) {
	current := func(id string) bool {
		return slices.ContainsFunc(nodeClass.Status.Images, func(image v1alpha1.OxideImage) bool {
			return image.ID == id
		})
	}
	if !current(imageID) {
		return
	}
	observed := slices.DeleteFunc(
		slices.Clone(nodeClass.Status.ObservedCapacity),
		func(entry v1alpha1.ObservedCapacity) bool { return !current(entry.ImageID) },
	)
	i := slices.IndexFunc(observed, func(entry v1alpha1.ObservedCapacity) bool {
		return entry.InstanceType == instanceType && entry.ImageID == imageID
	})
	switch {
	case i < 0:
		observed = append(observed, v1alpha1.ObservedCapacity{
			InstanceType: instanceType, ImageID: imageID, Memory: memory,
		})
	case memory.Cmp(observed[i].Memory) < 0:
		observed[i].Memory = memory
	}
	if len(observed) == 0 {
		observed = nil
	}
	nodeClass.Status.ObservedCapacity = observed
}

func registered(node *corev1.Node) bool {
	return node.Labels[karpv1.NodeRegisteredLabelKey] == "true"
}

var nodeClassLabel = karpv1.NodeClassLabelKey(
	schema.GroupKind{Group: apis.Group, Kind: "OxideNodeClass"},
)

func (c *CapacityController) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&corev1.Node{}, builder.WithPredicates(predicate.Funcs{
			// Every registered node on startup, so a restart refreshes what is recorded.
			CreateFunc: func(e event.CreateEvent) bool {
				return registered(e.Object.(*corev1.Node))
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return !registered(e.ObjectOld.(*corev1.Node)) &&
					registered(e.ObjectNew.(*corev1.Node))
			},
			DeleteFunc:  func(event.DeleteEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		}, predicate.NewPredicateFuncs(func(o client.Object) bool {
			_, ok := o.GetLabels()[nodeClassLabel]
			return ok
		}))).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 1,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
