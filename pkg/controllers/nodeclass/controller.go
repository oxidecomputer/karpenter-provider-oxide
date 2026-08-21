package nodeclass

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/google/uuid"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/affinitypolicy"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/image"
	"github.com/oxidecomputer/oxide.go/oxide"
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

// kubeletArgsAlways are the kubelet arguments every launched node needs regardless of how it is
// bootstrapped. The Oxide CCM assigns provider IDs, so the kubelet must not do it itself.
var kubeletArgsAlways = []string{"cloud-provider=external"}

// maxStatusImages caps how many resolved images are reported on the nodeclass status. Only the
// first is launched; the rest are there to make a wildcard version selector auditable without
// growing the object unboundedly when a selector matches a large image list.
const maxStatusImages = 3

// groupCollectionRequeue is how long a nodeclass waits before re-checking a managed affinity group
// that is not empty yet. Emptying one is a fleet replacement, which is paced by disruption budgets,
// so this only has to be short relative to a node's lifetime rather than to a node's replacement.
const groupCollectionRequeue = time.Minute

type Controller struct {
	kubeClient client.Client
	// reader is the manager's uncached reader, used for the one-off lookups that would otherwise
	// make controller-runtime list and watch an entire resource type cluster-wide just to read a
	// single object by name.
	reader      client.Reader
	oxideClient *oxide.Client
	// project is the Oxide project instances are launched into, which is what scopes the resources
	// resolved by name rather than by ID.
	project string
	// images resolves and caches what the nodeclass's image selectors match. Held as the provider
	// rather than called as a function so that the cache is shared across every nodeclass.
	images *image.Provider
	// affinityPolicy is what the launch path recorded about fail-policy groups it could not satisfy.
	// Read rather than written here: this controller only publishes it as a condition.
	affinityPolicy *affinitypolicy.Tracker
	bootstrappers  bootstrap.Bootstrappers
}

func NewController(
	kubeClient client.Client,
	reader client.Reader,
	oxideClient *oxide.Client,
	project string,
	images *image.Provider,
	affinityPolicy *affinitypolicy.Tracker,
	bootstrappers bootstrap.Bootstrappers,
) *Controller {
	return &Controller{
		kubeClient:     kubeClient,
		reader:         reader,
		oxideClient:    oxideClient,
		project:        project,
		images:         images,
		affinityPolicy: affinityPolicy,
		bootstrappers:  bootstrappers,
	}
}

// projectFor returns the project a lookup should be scoped by. A project is what makes a name
// unambiguous, but the API rejects one passed alongside an ID, which is unambiguous already.
func (c *Controller) projectFor(ref string) oxide.NameOrId {
	if _, err := uuid.Parse(ref); err == nil {
		return ""
	}
	return oxide.NameOrId(c.project)
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
	// Added before anything is created, so that a delete landing immediately after this reconcile
	// cannot leave behind a group with nothing left to clean it up. It goes on every nodeclass rather
	// than only on those configuring a managed group, because whether one owns a group is a fact
	// about the project rather than about the current spec: a managed group that has just been unset
	// still has to be collected.
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

	// Add the args Karpenter requires, deduplicated in case the user already specified them.
	nodeClass.Status.KubeletArgs = lo.Uniq(append(nodeClass.Spec.KubeletArgs, kubeletArgsAlways...))

	cluster, err := c.resolveCluster(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Everything bootstrap-provider-specific, including the final form of Status.Server and the
	// conditions the provider owns, is resolved behind this call.
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
	// Only the first image is ever launched. The runners-up are still reported so that a wildcard
	// selector can be understood from the nodeclass alone: what it matched, and why the winner won.
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

	subnets, err := c.resolveSubnets(ctx, nodeClass.Spec.SubnetSelector)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to retrieve subnets: %w", err)
	}
	nodeClass.Status.Subnets = subnets

	sshKeys, err := c.resolveSSHKeys(ctx, nodeClass.Spec.SSHKeySelector)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to retrieve SSH keys: %w", err)
	}
	nodeClass.Status.SSHKeys = sshKeys

	ipPools, err := c.resolveIPPools(ctx, nodeClass.Spec.IPPoolSelector)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to retrieve IP Pools: %w", err)
	}
	nodeClass.Status.IPPools = ipPools

	affinity, err := c.resolveAffinityGroups(ctx, nodeClass)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to retrieve affinity groups: %w", err)
	}
	switch {
	case nodeClass.Spec.Affinity == nil:
		// Nothing is desired, so nothing is published. The condition is not registered for this
		// nodeclass either, so there is none to set.
		nodeClass.Status.AffinityGroups = nil
	case affinity.resolved():
		nodeClass.Status.AffinityGroups = affinity.groups
		nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeAffinityGroupsResolved)
	default:
		// Status.AffinityGroups is deliberately left as it was. It governs which memberships are
		// allowed to persist, so replacing it with an incomplete set would evict memberships across
		// the fleet on the strength of a resolution that failed.
		nodeClass.StatusConditions().SetFalse(
			v1alpha1.ConditionTypeAffinityGroupsResolved,
			affinity.reason,
			affinity.message,
		)
	}

	// Whether the rack can currently satisfy a fail-policy group is not resolved from the API, only
	// remembered from the last launch that tried. Held here rather than in the shape breaker because
	// such a refusal is not about a shape, and published as a condition because a nodeclass that is
	// not Ready is what stops the scheduler from producing NodeClaims that cannot launch.
	policyRequeue := time.Duration(0)
	if nodeClass.Spec.Affinity != nil {
		if state, backingOff := c.affinityPolicy.State(nodeClass.Name); backingOff {
			nodeClass.StatusConditions().SetFalse(
				v1alpha1.ConditionTypeAffinityPolicySatisfiable,
				v1alpha1.ConditionReasonAffinityPolicyUnsatisfiable,
				state.Message,
			)
			// Requeued at the moment the wait elapses, since nothing else will prompt the reconcile
			// that makes the nodeclass Ready again and lets one launch probe the policy.
			policyRequeue = state.Remaining(time.Now())
		} else {
			nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeAffinityPolicySatisfiable)
		}
	}

	// Ready is not set here. It is recomputed from the dependent conditions every time one of them
	// is set, so asserting it after the fact would just overwrite a false the dependents earned.

	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		if err := c.kubeClient.Status().
			Patch(ctx, nodeClass, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if kerrors.IsConflict(err) {
				return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}

	// Deliberately after the status patch. A launch that read the status a moment before a group was
	// deleted would try to join a group that is gone, so the desired set is published first and only
	// then is anything outside it torn down.
	held, err := c.collectManagedGroups(
		ctx,
		nodeClass,
		managedGroupIDs(nodeClass.Status.AffinityGroups),
	)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to collect managed affinity groups: %w", err)
	}
	requeue := 30 * time.Minute
	if held {
		// A group waiting on its members to be replaced away. Nothing here can hurry that along, but
		// checking back sooner than the resync is what makes a superseded group disappear promptly
		// once the last node has left it, rather than half an hour later.
		requeue = groupCollectionRequeue
	}
	// Whichever comes first. The back-off expiring is the only thing that makes the nodeclass Ready
	// again, so it must not be missed by a longer wait for something else.
	if policyRequeue > 0 && policyRequeue < requeue {
		requeue = policyRequeue
	}
	return reconcile.Result{RequeueAfter: requeue}, nil
}

// finalize removes every group this nodeclass owns before letting the nodeclass itself go, so that a
// managed group cannot outlive the object that describes it.
//
// It collects regardless of what the spec still says, because a nodeclass being deleted wants none of
// its groups. A group that still has members is not deleted, so this holds the finalizer until the
// nodes have gone, which is the only thing that empties it.
func (c *Controller) finalize(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(nodeClass, v1alpha1.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}

	held, err := c.collectManagedGroups(ctx, nodeClass, nil)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("unable to collect managed affinity groups: %w", err)
	}
	if held {
		log.FromContext(ctx).V(1).Info(
			"waiting for the nodeclass's managed affinity groups to empty before deleting them",
		)
		return reconcile.Result{RequeueAfter: groupCollectionRequeue}, nil
	}

	// Nothing else drops this, and a nodeclass recreated under the same name is a different nodeclass
	// that should not inherit the old one's back-off.
	c.affinityPolicy.Forget(nodeClass.Name)

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

// resolveCluster discovers where the kube-apiserver can be reached. Bootstrap providers join on
// different ports, so this deliberately stops at the host and port and leaves rendering to them.
func (c *Controller) resolveCluster(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (bootstrap.Cluster, error) {
	cluster := bootstrap.Cluster{ServerOverride: nodeClass.Spec.Server}
	// A user-supplied server is used verbatim, so there is no reason to query the cluster for one.
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

func (c *Controller) resolveIPPools(
	ctx context.Context,
	ipPoolSelectors []v1alpha1.IPPoolSelector,
) ([]v1alpha1.OxideIPPool, error) {
	var listSelectors []v1alpha1.IPPoolSelector
	var resolvedIPPools []oxide.SiloIpPool
	for _, ipPoolSelector := range ipPoolSelectors {
		nameOrID := ""
		if ipPoolSelector.ID != "" {
			nameOrID = ipPoolSelector.ID
		} else if ipPoolSelector.Name != "" {
			nameOrID = ipPoolSelector.Name
		}
		if nameOrID != "" {
			ipPool, err := c.oxideClient.IpPoolView(ctx, oxide.IpPoolViewParams{
				Pool: oxide.NameOrId(nameOrID),
			})
			if err != nil {
				return nil, fmt.Errorf("IP Pool %q: %w", nameOrID, err)
			}
			resolvedIPPools = append(resolvedIPPools, *ipPool)
		} else {
			listSelectors = append(listSelectors, ipPoolSelector)
		}
	}

	var ipPools []oxide.SiloIpPool
	if len(listSelectors) > 0 {
		var err error
		ipPools, err = c.oxideClient.IpPoolListAllPages(ctx, oxide.IpPoolListParams{})
		if err != nil {
			return nil, fmt.Errorf("list error: %w", err)
		}
	}

	for _, ipPoolSelector := range listSelectors {
		for _, ipPool := range ipPools {
			if ipPoolSelector.Default && !*ipPool.IsDefault {
				continue
			}
			if ipPoolSelector.IPVersion != "" &&
				!strings.HasSuffix(ipPoolSelector.IPVersion, string(ipPool.IpVersion)) {
				continue
			}
			resolvedIPPools = append(resolvedIPPools, ipPool)
		}
	}

	resolvedIPPools = lo.UniqBy(
		resolvedIPPools,
		func(ipPool oxide.SiloIpPool) string { return ipPool.Id },
	)
	return lo.Map(resolvedIPPools, func(ipPool oxide.SiloIpPool, _ int) v1alpha1.OxideIPPool {
		return v1alpha1.OxideIPPool{
			ID:        ipPool.Id,
			Name:      string(ipPool.Name),
			IPVersion: string(ipPool.IpVersion),
			Default:   *ipPool.IsDefault,
		}
	}), nil
}

func (c *Controller) resolveSubnets(
	ctx context.Context,
	subnetSelectors []v1alpha1.SubnetSelector,
) ([]v1alpha1.OxideSubnet, error) {
	var resolvedSubnets []oxide.VpcSubnet
	vpcIDToName := map[string]string{}
	for _, subnetSelector := range subnetSelectors {
		nameOrID := ""
		project := subnetSelector.Project
		vpcNameOrID := ""

		if subnetSelector.VPCID != "" {
			vpcNameOrID = subnetSelector.VPCID
			// project cannot be used to query if ID is used
			project = ""
		} else if subnetSelector.VPCName != "" {
			vpcNameOrID = subnetSelector.VPCName
		}

		if subnetSelector.ID != "" {
			nameOrID = subnetSelector.ID
			// project cannot be used to query if ID is used
			project = ""
		} else if subnetSelector.Name != "" {
			nameOrID = subnetSelector.Name
		}

		if nameOrID != "" {
			subnet, err := c.oxideClient.VpcSubnetView(ctx, oxide.VpcSubnetViewParams{
				Subnet:  oxide.NameOrId(nameOrID),
				Vpc:     oxide.NameOrId(vpcNameOrID),
				Project: oxide.NameOrId(project),
			})
			if err != nil {
				if errors.Is(err, oxide.ErrObjectNotFound) {
					continue
				}
				return nil, fmt.Errorf(
					"subnet %q from VPC %q and project %q: %w",
					nameOrID,
					vpcNameOrID,
					project,
					err,
				)
			}
			resolvedSubnets = append(resolvedSubnets, *subnet)
		} else if vpcNameOrID != "" { // Only VPC is passed
			subnets, err := c.oxideClient.VpcSubnetListAllPages(ctx, oxide.VpcSubnetListParams{
				Vpc:     oxide.NameOrId(vpcNameOrID),
				Project: oxide.NameOrId(project),
			})
			if err != nil {
				if errors.Is(err, oxide.ErrObjectNotFound) {
					continue
				}
				return nil, fmt.Errorf(
					"subnets from VPC %q and project %q: %w",
					vpcNameOrID,
					project,
					err,
				)
			}
			resolvedSubnets = append(resolvedSubnets, subnets...)
		}

		// Cache the VPC ID to Name mapping if a name is used in the selector since we need the VPC
		// Name in the instance create to create a network interface.
		// InstanceCreate#CreateNetworkInterface does not accept a VPC ID, only a Name.
		if subnetSelector.VPCName != "" {
			vpcIDToName[resolvedSubnets[len(resolvedSubnets)-1].VpcId] = subnetSelector.VPCName
		}

	}

	var oxideSubnets []v1alpha1.OxideSubnet
	for _, subnet := range lo.Uniq(resolvedSubnets) {
		vpcName, ok := vpcIDToName[subnet.VpcId]
		if !ok {
			vpc, err := c.oxideClient.VpcView(ctx, oxide.VpcViewParams{
				Vpc: oxide.NameOrId(subnet.VpcId),
			})
			if err != nil {
				return nil, fmt.Errorf("vpc information for subnet %q: %w", subnet.Id, err)
			}
			vpcName = string(vpc.Name)
		}
		oxideSubnets = append(oxideSubnets, v1alpha1.OxideSubnet{
			ID:        subnet.Id,
			Name:      string(subnet.Name),
			VPCID:     subnet.VpcId,
			VPCName:   vpcName,
			IPv4Block: string(subnet.Ipv4Block),
			IPv6Block: string(subnet.Ipv6Block),
		})

	}
	return oxideSubnets, nil
}

func (c *Controller) resolveSSHKeys(
	ctx context.Context,
	sshKeySelectors []v1alpha1.SSHKeySelector,
) ([]v1alpha1.OxideSSHKey, error) {
	var sshKeys []v1alpha1.OxideSSHKey
	for _, sshKeySelector := range sshKeySelectors {
		nameOrID := lo.Ternary(sshKeySelector.ID != "", sshKeySelector.ID, sshKeySelector.Name)
		sshKey, err := c.oxideClient.CurrentUserSshKeyView(ctx, oxide.CurrentUserSshKeyViewParams{
			SshKey: oxide.NameOrId(nameOrID),
		})
		if err != nil {
			return nil, fmt.Errorf("ssh key %q: %w", nameOrID, err)
		}
		sshKeys = append(sshKeys, v1alpha1.OxideSSHKey{
			ID:   sshKey.Id,
			Name: string(sshKey.Name),
		})
	}
	return sshKeys, nil
}

// logStale notes that a resolution was served from a reading nothing has been able to refresh.
//
// Quiet on purpose. A reading that has not meaningfully aged is still worth publishing, so this is
// the whole of the response to a failure inside the staleness ceiling; past the ceiling the provider
// returns the error instead and the reconcile fails loudly on its own.
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
