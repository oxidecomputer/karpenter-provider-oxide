package availability

import (
	"context"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	corev1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

type Controller struct{ availability *availability.Manager }

func NewController(availabilityManager *availability.Manager) *Controller {
	return &Controller{availability: availabilityManager}
}

func (c *Controller) Name() string { return "availability" }

func (c *Controller) Reconcile(
	ctx context.Context,
	claim *karpv1.NodeClaim,
) (reconcile.Result, error) {
	ref := claim.Spec.NodeClassRef
	if ref == nil || ref.Group != apis.Group || ref.Kind != "OxideNodeClass" ||
		!claim.DeletionTimestamp.IsZero() || !claim.StatusConditions().Get(karpv1.ConditionTypeLaunched).IsTrue() {
		return reconcile.Result{}, nil
	}
	shape, err := instancetype.InstanceTypeToSpecs(claim.Labels[corev1.LabelInstanceTypeStable])
	if err == nil {
		c.availability.SeedLaunchable(
			ctx,
			shape,
			availability.Platform(claim.Labels[v1alpha1.LabelInstanceCPUPlatform]),
		)
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).Named(c.Name()).For(&karpv1.NodeClaim{}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
