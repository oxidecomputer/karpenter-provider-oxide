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

// HashController stamps the nodeclass with the hash of its launch-time configuration, which is the
// value a NodeClaim's own recorded hash is compared against to decide static drift.
//
// Separate from the reconciler that resolves status because the two must not share a fate. A launch
// records the hash it computed from the nodeclass it read, so if the nodeclass's own annotation could
// only be written by a reconcile that also had to reach the Oxide API and the cluster's
// EndpointSlices, then any failure in those would leave the stamp behind the spec -- and every node
// launched in the meantime would read as drifted from a nodeclass that had simply not caught up.
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

	// A nodeclass on its way out has no configuration worth stamping, and its nodes are going with
	// it. Drift on a node that is already being torn down changes nothing about what happens to it.
	if !nodeClass.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	stored := nodeClass.DeepCopy()

	// Before the new stamp is written, not after. A NodeClaim carrying a hash from a different
	// version cannot be compared against this one -- the same configuration hashes differently
	// across versions -- so it is brought up to the current version first. That covers the initial
	// rollout as well as a version bump: a nodeclass with no annotations at all is at "", so every
	// node that predates this controller adopts the current hash rather than reading as drifted
	// against a value it never had the chance to record.
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

// updateNodeClaimHashes brings every NodeClaim of this nodeclass onto the current hash version.
//
// The hash a NodeClaim recorded at an older version says nothing about whether its configuration
// still matches, so it is replaced with the nodeclass's current hash rather than compared against it.
// A NodeClaim that is already marked drifted keeps what it has: it was found drifted while the two
// were comparable, and re-stamping it now would erase a finding this version cannot reproduce.
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

// ownedBy is whether a NodeClaim was launched from this nodeclass. The group is checked as well as
// the name, since nothing stops another provider's nodeclass from sharing a name with this one.
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
