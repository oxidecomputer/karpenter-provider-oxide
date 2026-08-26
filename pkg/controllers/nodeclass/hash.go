package nodeclass

import (
	"context"
	"fmt"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// HashController stamps the nodeclass with the hash of its launch-time configuration.
type HashController struct {
	kubeClient client.Client
}

func NewHashController(kubeClient client.Client) *HashController {
	return &HashController{kubeClient: kubeClient}
}

func (h *HashController) Name() string {
	return "oxidenodeclass.hash"
}

func (h *HashController) Reconcile(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (reconcile.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", h.Name()))

	if !nodeClass.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	stored := nodeClass.DeepCopy()

	if nodeClass.Annotations[v1alpha1.AnnotationOxideNodeClassHashVersion] != v1alpha1.OxideNodeClassHashVersion {
		if err := h.updateNodeClaimHashes(ctx, nodeClass); err != nil {
			return reconcile.Result{}, err
		}
	}

	nodeClass.Annotations = lo.Assign(nodeClass.Annotations, map[string]string{
		v1alpha1.AnnotationOxideNodeClassHash:        nodeClass.Hash(),
		v1alpha1.AnnotationOxideNodeClassHashVersion: v1alpha1.OxideNodeClassHashVersion,
	})

	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		if err := h.kubeClient.Patch(ctx, nodeClass, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{}, nil
}

func (h *HashController) updateNodeClaimHashes(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) error {
	nodeClaims := &karpv1.NodeClaimList{}
	if err := h.kubeClient.List(ctx, nodeClaims); err != nil {
		return fmt.Errorf("unable to list NodeClaims, %w", err)
	}
	for i := range nodeClaims.Items {
		nodeClaim := &nodeClaims.Items[i]
		if !ownedBy(nodeClaim, nodeClass) {
			continue
		}
		if nodeClaim.Annotations[v1alpha1.AnnotationOxideNodeClassHashVersion] == v1alpha1.OxideNodeClassHashVersion {
			continue
		}
		stored := nodeClaim.DeepCopy()
		annotations := map[string]string{
			v1alpha1.AnnotationOxideNodeClassHashVersion: v1alpha1.OxideNodeClassHashVersion,
		}
		if nodeClaim.StatusConditions().Get(karpv1.ConditionTypeDrifted) == nil {
			annotations[v1alpha1.AnnotationOxideNodeClassHash] = nodeClass.Hash()
		}
		nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, annotations)
		if err := h.kubeClient.Patch(ctx, nodeClaim, client.MergeFrom(stored)); err != nil {
			return client.IgnoreNotFound(
				fmt.Errorf("unable to update the NodeClaim's nodeclass hash, %w", err),
			)
		}
	}
	return nil
}

func ownedBy(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.OxideNodeClass) bool {
	ref := nodeClaim.Spec.NodeClassRef
	if ref == nil {
		return false
	}
	return ref.Group == apis.Group && ref.Name == nodeClass.Name
}

func (h *HashController) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(h.Name()).
		For(&v1alpha1.OxideNodeClass{}).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), h))
}
