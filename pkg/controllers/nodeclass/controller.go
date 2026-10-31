package nodeclass

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/affinitygroup"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/image"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/ippool"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/snapshot"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/sshkey"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/subnet"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

// kubeletArgsDefault is what a node needs to be managed by an out-of-tree cloud provider,
// which is the only way Oxide nodes are managed.
const cloudProviderExternal = "cloud-provider=external"

var kubeletArgsDefault = []string{cloudProviderExternal}

const maxStatusImages = 3

const resolveInterval = providers.ResolveInterval

const groupCollectionRequeue = time.Minute

type Controller struct {
	kubeClient client.Client

	reader client.Reader

	recorder events.Recorder

	images *image.Provider

	subnets *subnet.Provider

	sshKeys *sshkey.Provider

	ipPools *ippool.Provider

	affinityGroups *affinitygroup.Provider

	snapshots *snapshot.Provider

	bootstrappers *bootstrap.Bootstrappers
}

func NewController(
	kubeClient client.Client,
	reader client.Reader,
	recorder events.Recorder,
	images *image.Provider,
	subnets *subnet.Provider,
	sshKeys *sshkey.Provider,
	ipPools *ippool.Provider,
	affinityGroups *affinitygroup.Provider,
	snapshots *snapshot.Provider,
	bootstrappers *bootstrap.Bootstrappers,
) *Controller {
	return &Controller{
		kubeClient:     kubeClient,
		reader:         reader,
		recorder:       recorder,
		images:         images,
		subnets:        subnets,
		sshKeys:        sshKeys,
		ipPools:        ipPools,
		affinityGroups: affinityGroups,
		snapshots:      snapshots,
		bootstrappers:  bootstrappers,
	}
}

func (c *Controller) Name() string {
	return "oxidenodeclass"
}

func (c *Controller) Reconcile(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (reconcile.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	if !nodeClass.DeletionTimestamp.IsZero() {
		return c.finalize(ctx, nodeClass)
	}

	if !controllerutil.ContainsFinalizer(nodeClass, v1alpha1.TerminationFinalizer) {
		stored := nodeClass.DeepCopy()
		controllerutil.AddFinalizer(nodeClass, v1alpha1.TerminationFinalizer)
		if err := c.kubeClient.Patch(ctx, nodeClass, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}

	stored := nodeClass.DeepCopy()

	bootstrapper, err := c.bootstrappers.For(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, err
	}
	if nodeClass.Status.Bootstrap == nil {
		nodeClass.Status.Bootstrap = &v1alpha1.BootstrapStatus{}
	}
	nodeClass.Status.Bootstrap.Kind = bootstrapper.Name()

	// Reservations are per shape, so bootstrap adds them at launch; status carries
	// the flags that hold for every node of the class.
	kubelet, err := instancetype.ParseKubelet(nodeClass.Spec.Kubelet)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to resolve kubelet configuration: %w", err)
	}
	nodeClass.Status.KubeletArgs = bootstrap.MergeKubeletArgs(kubeletArgsDefault, kubelet.Args)

	cluster, err := c.resolveCluster(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, err
	}

	if err := bootstrapper.Resolve(ctx, nodeClass, cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf(
			"unable to resolve %s bootstrap configuration: %w",
			bootstrapper.Name(),
			err,
		)
	}

	images, err := c.images.Resolve(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to retrieve image: %w", err)
	}
	logStale(ctx, "image", images.Stale, images.Age)

	nodeClass.Status.Images = images.Resolved[:min(len(images.Resolved), maxStatusImages)]
	setCondition(nodeClass, v1alpha1.ConditionTypeImagesResolved, images.Reason, images.Message)

	if err := c.resolveStorage(ctx, nodeClass); err != nil {
		return reconcile.Result{}, err
	}

	subnets, err := c.subnets.Resolve(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to retrieve subnets: %w", err)
	}
	logStale(ctx, "subnet", subnets.Stale, subnets.Age)
	nodeClass.Status.Subnets = subnets.Resolved
	setCondition(
		nodeClass,
		v1alpha1.ConditionTypeSubnetsResolved,
		subnets.Reason,
		subnets.Message,
	)

	sshKeys, err := c.sshKeys.Resolve(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to retrieve SSH keys: %w", err)
	}
	logStale(ctx, "sshkey", sshKeys.Stale, sshKeys.Age)
	nodeClass.Status.SSHKeys = sshKeys.Resolved

	ephemeralIPs, err := c.ipPools.Resolve(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to retrieve IP Pools: %w", err)
	}
	logStale(ctx, "ippool", ephemeralIPs.Stale, ephemeralIPs.Age)
	if len(nodeClass.Spec.EphemeralIPs) == 0 {
		nodeClass.Status.EphemeralIPs = nil
	} else {
		// A resolution that fell short leaves the last good addresses in status.
		if ephemeralIPs.Complete() {
			nodeClass.Status.EphemeralIPs = ephemeralIPs.Resolved
		}
		setCondition(
			nodeClass,
			v1alpha1.ConditionTypeEphemeralIPsResolved,
			ephemeralIPs.Reason,
			ephemeralIPs.Message,
		)
	}

	affinity, err := c.affinityGroups.Resolve(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to retrieve affinity groups: %w", err)
	}
	logStale(ctx, "affinitygroup", affinity.Stale, affinity.Age)
	if nodeClass.Spec.Affinity == nil {
		nodeClass.Status.AffinityGroups = nil
	} else {
		if affinity.Complete() {
			nodeClass.Status.AffinityGroups = affinity.Resolved
		}
		setCondition(
			nodeClass,
			v1alpha1.ConditionTypeAffinityGroupsResolved,
			affinity.Reason,
			affinity.Message,
		)
	}

	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		if err := c.kubeClient.Status().
			Patch(ctx, nodeClass, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if kerrors.IsConflict(err) {
				return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}

	held, err := c.affinityGroups.Collect(
		ctx,
		nodeClass,
		affinitygroup.ManagedIDs(nodeClass.Status.AffinityGroups),
	)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to collect managed affinity groups: %w", err)
	}
	requeue := resolveInterval
	if held {

		requeue = groupCollectionRequeue
	}

	return reconcile.Result{RequeueAfter: requeue}, nil
}

// setCondition records one provider resolution: true when its reason is empty, otherwise that
// reason and message.
func setCondition(nodeClass *v1alpha1.OxideNodeClass, conditionType, reason, message string) {
	if reason != "" {
		nodeClass.StatusConditions().SetFalse(conditionType, reason, message)
		return
	}
	nodeClass.StatusConditions().SetTrue(conditionType)
}

func (c *Controller) finalize(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(nodeClass, v1alpha1.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}

	nodeClaims := &karpcorev1.NodeClaimList{}
	if err := c.kubeClient.List(
		ctx,
		nodeClaims,
		nodeclaimutils.ForNodeClass(nodeClass),
	); err != nil {
		return reconcile.Result{}, fmt.Errorf(
			"unable to list NodeClaims for the nodeclass: %w",
			err,
		)
	}
	// Dropping the finalizer while NodeClaims still reference the nodeclass strands them: drift
	// can no longer resolve the nodeclass, so those nodes are never replaced.
	if len(nodeClaims.Items) > 0 {
		names := lo.Map(nodeClaims.Items, func(nodeClaim karpcorev1.NodeClaim, _ int) string {
			return nodeClaim.Name
		})
		c.recorder.Publish(events.Event{
			InvolvedObject: nodeClass,
			Type:           corev1.EventTypeNormal,
			Reason:         "WaitingOnNodeClaimTermination",
			Message: fmt.Sprintf(
				"Waiting on NodeClaim termination for %s",
				pretty.Slice(names, 5),
			),
			DedupeValues: []string{string(nodeClass.UID)},
		})
		log.FromContext(ctx).
			V(1).
			Info("waiting for the nodeclass's NodeClaims to terminate before deleting it")
		return reconcile.Result{RequeueAfter: groupCollectionRequeue}, nil
	}

	held, err := c.affinityGroups.Collect(ctx, nodeClass, nil)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to collect managed affinity groups: %w", err)
	}
	if held {
		log.FromContext(ctx).V(1).Info(
			"waiting for the nodeclass's managed affinity groups to empty before deleting them",
		)
		return reconcile.Result{RequeueAfter: groupCollectionRequeue}, nil
	}

	stored := nodeClass.DeepCopy()
	controllerutil.RemoveFinalizer(nodeClass, v1alpha1.TerminationFinalizer)
	if err := c.kubeClient.Patch(ctx, nodeClass, client.MergeFrom(stored)); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1alpha1.OxideNodeClass{}).
		Watches(
			&karpcorev1.NodeClaim{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
				nodeClaim := o.(*karpcorev1.NodeClaim)
				if nodeClaim.Spec.NodeClassRef == nil {
					return nil
				}
				return []reconcile.Request{
					{Name: nodeClaim.Spec.NodeClassRef.Name},
				}
			}),
			// Only deletions matter: they're what lets a terminating nodeclass finish finalizing.
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(event.CreateEvent) bool { return false },
				UpdateFunc:  func(event.UpdateEvent) bool { return false },
				DeleteFunc:  func(event.DeleteEvent) bool { return true },
				GenericFunc: func(event.GenericEvent) bool { return false },
			}),
		).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func (c *Controller) resolveCluster(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (bootstrap.Cluster, error) {
	cluster := bootstrap.Cluster{ServerOverride: nodeClass.Spec.Server}

	if cluster.ServerOverride != "" {
		return cluster, nil
	}

	kubeEndpoint := &discoveryv1.EndpointSlice{}
	if err := c.reader.Get(
		ctx,
		client.ObjectKey{Namespace: "default", Name: "kubernetes"},
		kubeEndpoint,
	); err != nil {
		return cluster, fmt.Errorf(
			"unable to retrieve the Kubernetes API server IP from default/kubernetes service (endpoint slice), %w",
			err,
		)
	}
	firstEndpoint, ok := lo.First(kubeEndpoint.Endpoints)
	if !ok || len(firstEndpoint.Addresses) == 0 {
		return cluster, fmt.Errorf(
			"no server specified in spec and the default/kubernetes service has no endpoint addresses",
		)
	}
	cluster.Host = firstEndpoint.Addresses[0]
	if port, ok := lo.First(kubeEndpoint.Ports); ok && port.Port != nil {
		cluster.Port = *port.Port
	}
	return cluster, nil
}

func logStale(ctx context.Context, resource string, stale bool, age time.Duration) {
	if !stale {
		return
	}
	log.FromContext(ctx).V(1).Info(
		"serving a resource resolution from a reading that could not be refreshed",
		"resource", resource,
		"age", age.Round(time.Second).String(),
	)
}
