package nodeclass

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/affinitygroup"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/image"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/ippool"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/snapshot"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/sshkey"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/subnet"
	"github.com/samber/lo"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var kubeletArgsAlways = []string{"cloud-provider=external"}

const maxStatusImages = 3

const resolveInterval = providers.ResolveInterval

const groupCollectionRequeue = time.Minute

type Controller struct {
	kubeClient client.Client

	reader client.Reader

	images *image.Provider

	subnets *subnet.Provider

	sshKeys *sshkey.Provider

	ipPools *ippool.Provider

	affinityGroups *affinitygroup.Provider

	snapshots *snapshot.Provider

	bootstrappers bootstrap.Bootstrappers
}

func NewController(
	kubeClient client.Client,
	reader client.Reader,
	images *image.Provider,
	subnets *subnet.Provider,
	sshKeys *sshkey.Provider,
	ipPools *ippool.Provider,
	affinityGroups *affinitygroup.Provider,
	snapshots *snapshot.Provider,
	bootstrappers bootstrap.Bootstrappers,
) *Controller {
	return &Controller{
		kubeClient:     kubeClient,
		reader:         reader,
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

	bootstrapper, err := c.bootstrappers.For(nodeClass)
	if err != nil {
		return reconcile.Result{}, err
	}

	nodeClass.Status.KubeletArgs = lo.Uniq(append(nodeClass.Spec.KubeletArgs, kubeletArgsAlways...))

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
	if len(images.Resolved) == 0 {
		nodeClass.StatusConditions().SetFalse(
			v1alpha1.ConditionTypeImagesResolved,
			"ImagesNotFound",
			image.DescribeSelectors(images.Unmatched),
		)
	} else {
		nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesResolved)
	}

	if err := c.resolveStorage(ctx, nodeClass); err != nil {
		return reconcile.Result{}, err
	}

	subnets, err := c.subnets.Resolve(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to retrieve subnets: %w", err)
	}
	logStale(ctx, "subnet", subnets.Stale, subnets.Age)
	nodeClass.Status.Subnets = subnets.Resolved
	setSubnetCondition(nodeClass, subnets)

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
	switch {
	case len(nodeClass.Spec.EphemeralIPs) == 0:

		nodeClass.Status.EphemeralIPs = nil
	case ephemeralIPs.Complete():
		nodeClass.Status.EphemeralIPs = ephemeralIPs.Resolved
		nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeEphemeralIPsResolved)
	default:

		nodeClass.StatusConditions().SetFalse(
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
	switch {
	case nodeClass.Spec.Affinity == nil:

		nodeClass.Status.AffinityGroups = nil
	case affinity.Complete():
		nodeClass.Status.AffinityGroups = affinity.Resolved
		nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeAffinityGroupsResolved)
	default:

		nodeClass.StatusConditions().SetFalse(
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

func setSubnetCondition(nodeClass *v1alpha1.OxideNodeClass, result subnet.Result) {
	if len(result.Resolved) == 0 {
		nodeClass.StatusConditions().SetFalse(
			v1alpha1.ConditionTypeSubnetsResolved,
			"SubnetsNotFound",
			"no subnets matched "+subnet.DescribeSelectors(nodeClass.Spec.SubnetSelector),
		)
		return
	}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeSubnetsResolved)
}

func (c *Controller) finalize(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(nodeClass, v1alpha1.TerminationFinalizer) {
		return reconcile.Result{}, nil
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
