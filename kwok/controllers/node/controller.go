package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cloudprovider"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// InstanceReader intentionally exposes no cloud mutations.
type InstanceReader interface {
	InstanceView(context.Context, oxide.InstanceViewParams) (*oxide.Instance, error)
}

type Controller struct {
	Client client.Client
	// Reader must bypass the cache for deletion/UID checks and collision detection.
	Reader                   client.Reader
	Instances                InstanceReader
	ClusterUID               types.UID
	ProjectID                string
	KubernetesVersion        string
	PollInterval, StaleAfter time.Duration
	Clock                    clock.Clock
	RemoveStartupTaints      []string // key:effect, and only if present in claim startupTaints
}

func (c *Controller) Setup(mgr ctrl.Manager, concurrency int) error {
	return ctrl.NewControllerManagedBy(mgr).Named("oxide-kwok-node").For(&karpv1.NodeClaim{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
			if obj.GetLabels()[Managed] != managedValue {
				return nil
			}
			for _, ref := range obj.GetOwnerReferences() {
				if ref.Kind == claimKind && ref.APIVersion == claimAPIVersion {
					return []ctrl.Request{{Name: ref.Name}}
				}
			}
			return nil
		}), builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return e.ObjectOld.GetDeletionTimestamp().IsZero() &&
					!e.ObjectNew.GetDeletionTimestamp().IsZero()
			},
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: concurrency,
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](
				time.Second,
				30*time.Second,
			),
		}).
		Complete(c)
}

func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	again := ctrl.Result{RequeueAfter: c.PollInterval}
	claim := &karpv1.NodeClaim{}
	if err := c.Reader.Get(ctx, req.NamespacedName, claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if claim.Spec.NodeClassRef == nil ||
		claim.Spec.NodeClassRef.Group != "karpenter.oxide.computer" ||
		claim.Spec.NodeClassRef.Kind != "OxideNodeClass" ||
		!claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if !claim.StatusConditions().Get(karpv1.ConditionTypeLaunched).IsTrue() ||
		claim.Status.ProviderID == "" ||
		!complete(claim) {
		return again, nil
	}
	id, err := cloudprovider.InstanceIDFromProviderID(claim.Status.ProviderID)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := Project(claim, c.ClusterUID, id, c.KubernetesVersion)
	node, err := c.findNode(ctx, claim, id, desired.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if node != nil && !node.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if node == nil && claim.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
		return again, nil
	}
	state, readErr := c.observe(ctx, claim, id, desired.Name)
	if state == "" {
		return ctrl.Result{}, readErr
	}
	observed := readErr == nil
	if node == nil {
		if readErr != nil {
			return again, fmt.Errorf("observing backend: %w", readErr)
		}
		if state != stateRunning {
			return again, nil
		}
		if err := c.live(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
		if claim.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
			return again, nil
		}
		desired.Annotations[ObservedAt] = c.Clock.Now().UTC().Format(time.RFC3339Nano)
		// Status is a separate write, even when a fake API accepts status during Create.
		desired.Status = corev1.NodeStatus{}
		if err := c.Client.Create(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
		node = &corev1.Node{}
		if err := c.Reader.Get(ctx, client.ObjectKey{Name: desired.Name}, node); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !observed {
		last, err := time.Parse(time.RFC3339Nano, node.Annotations[ObservedAt])
		if err == nil && c.Clock.Now().Sub(last) < c.StaleAfter {
			return again, fmt.Errorf("observing backend: %w", readErr)
		}
	}
	if err := c.updateNode(ctx, claim, node, id, state, observed); err != nil {
		return ctrl.Result{}, err
	}
	if !observed {
		return again, fmt.Errorf("observing backend: %w", readErr)
	}
	return again, nil
}

func (c *Controller) updateNode(
	ctx context.Context,
	claim *karpv1.NodeClaim,
	node *corev1.Node,
	id, state string,
	observed bool,
) error {
	if !node.DeletionTimestamp.IsZero() {
		return nil
	}
	if !c.owned(node, claim, id) {
		return fmt.Errorf("node identity changed for claim %s", claim.Name)
	}
	if err := c.live(ctx, claim); err != nil {
		return err
	}
	before := node.DeepCopy()
	node.Labels[BackendState] = state
	if observed {
		node.Annotations[ObservedAt] = c.Clock.Now().UTC().Format(time.RFC3339Nano)
	}
	if claim.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
		node.Spec.Taints = slices.DeleteFunc(node.Spec.Taints, func(t corev1.Taint) bool {
			if !slices.Contains(c.RemoveStartupTaints, t.Key+":"+string(t.Effect)) {
				return false
			}
			for _, permanent := range claim.Spec.Taints {
				if permanent.Key == t.Key && permanent.Effect == t.Effect {
					return false
				}
			}
			return slices.ContainsFunc(claim.Spec.StartupTaints, func(start corev1.Taint) bool {
				return start.Key == t.Key && start.Effect == t.Effect && start.Value == t.Value
			})
		})
	}
	if err := c.Client.Patch(
		ctx,
		node,
		client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}),
	); err != nil {
		return err
	}
	return c.initializeResources(ctx, claim, node, id)
}

// Karpenter registration may finish before nodes/status is initialized. This durable
// marker closes that race without taking ownership of ongoing kubelet resource status.
func (c *Controller) initializeResources(
	ctx context.Context,
	claim *karpv1.NodeClaim,
	node *corev1.Node,
	id string,
) error {
	if node.Annotations[ResourcesInitialized] == managedValue {
		return nil
	}
	if err := c.live(ctx, claim); err != nil {
		return err
	}
	if err := c.Reader.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		return err
	}
	if !node.DeletionTimestamp.IsZero() || !c.owned(node, claim, id) {
		return nil
	}
	if node.Annotations[ResourcesInitialized] == managedValue {
		return nil
	}
	// If a previous status write succeeded but its marker failed, preserve the status
	// already present instead of replaying it over another controller's changes.
	if !completeResources(node.Status.Capacity, node.Status.Allocatable) {
		before := node.DeepCopy()
		projection := Project(claim, c.ClusterUID, id, c.KubernetesVersion)
		node.Status.Capacity, node.Status.Allocatable = projection.Status.Capacity, projection.Status.Allocatable
		node.Status.NodeInfo, node.Status.Addresses = projection.Status.NodeInfo, projection.Status.Addresses
		if err := c.Client.Status().
			Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	if err := c.live(ctx, claim); err != nil {
		return err
	}
	before := node.DeepCopy()
	node.Annotations[ResourcesInitialized] = managedValue
	return c.Client.Patch(
		ctx,
		node,
		client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}),
	)
}

func (c *Controller) owned(n *corev1.Node, claim *karpv1.NodeClaim, id string) bool {
	if n.Labels[Managed] != managedValue || n.Labels[simulationLabel] != simulationValue ||
		n.Annotations[Cluster] != string(c.ClusterUID) ||
		n.Annotations[ClaimUID] != string(claim.UID) ||
		n.Annotations[InstanceID] != id ||
		n.Spec.ProviderID != claim.Status.ProviderID {
		return false
	}
	return slices.ContainsFunc(n.OwnerReferences, func(r metav1.OwnerReference) bool {
		return r.UID == claim.UID && r.Name == claim.Name && r.Kind == claimKind &&
			r.APIVersion == claimAPIVersion
	})
}

func (c *Controller) live(ctx context.Context, claim *karpv1.NodeClaim) error {
	fresh := &karpv1.NodeClaim{}
	if err := c.Reader.Get(ctx, client.ObjectKeyFromObject(claim), fresh); err != nil {
		return err
	}
	if fresh.UID != claim.UID || !fresh.DeletionTimestamp.IsZero() ||
		fresh.Status.ProviderID != claim.Status.ProviderID {
		return apierrors.NewConflict(
			schema.GroupResource{Group: "karpenter.sh", Resource: "nodeclaims"},
			claim.Name,
			errors.New("claim changed during registration"),
		)
	}
	// Use the fresh registration condition to avoid restoring status after registration.
	claim.Status = fresh.Status
	return nil
}

func (c *Controller) findNode(
	ctx context.Context,
	claim *karpv1.NodeClaim,
	id, name string,
) (*corev1.Node, error) {
	nodes := &corev1.NodeList{}
	if err := c.Reader.List(ctx, nodes); err != nil {
		return nil, err
	}
	var node *corev1.Node
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Name != name && n.Spec.ProviderID != claim.Status.ProviderID {
			continue
		}
		if n.Name != name || !c.owned(n, claim, id) {
			return nil, fmt.Errorf("node identity collision for claim %s", claim.Name)
		}
		node = n
	}
	return node, nil
}

// An empty state signals an identity conflict, rather than a failed observation.
func (c *Controller) observe(
	ctx context.Context,
	claim *karpv1.NodeClaim,
	id, name string,
) (string, error) {
	instance, err := c.Instances.InstanceView(
		ctx,
		oxide.InstanceViewParams{Instance: oxide.NameOrId(id)},
	)
	if errors.Is(err, oxide.ErrObjectNotFound) {
		return stateMissing, nil
	}
	if err != nil {
		return stateUnknown, err
	}
	if instance == nil || instance.Id != id || instance.ProjectId != c.ProjectID ||
		instance.Hostname != name {
		return "", fmt.Errorf(
			"instance identity/project/hostname mismatch for claim %s",
			claim.Name,
		)
	}
	if err := ownership.Verify(
		instance.Description,
		ownership.Marker{
			ClusterUID:    c.ClusterUID,
			NodeClaimUID:  claim.UID,
			NodeClaimName: claim.Name,
			Kind:          ownership.KindInstance,
		},
	); err != nil {
		return "", err
	}
	switch instance.RunState {
	case stateRunning:
		return stateRunning, nil
	case oxide.InstanceStateStopped, oxide.InstanceStateFailed, oxide.InstanceStateDestroyed:
		return stateTerminal, nil
	default:
		return stateUnknown, nil
	}
}
