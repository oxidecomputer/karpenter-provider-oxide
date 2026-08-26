package cloudprovider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	"github.com/google/uuid"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/affinitypolicy"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	karpoptions "sigs.k8s.io/karpenter/pkg/operator/options"
)

const (
	GiB                = 1 << 30
	InstanceNamePrefix = "karpenter-"
)

type OxideCloudProvider struct {
	kubeClient client.Client
	ox         *oxide.Client
	project    string

	clusterUID types.UID

	instanceProvider     *instance.Provider
	instanceTypeProvider *instancetype.Provider
	instanceTypeStore    instanceTypeOverlayStore

	affinityPolicy *affinitypolicy.Tracker
	bootstrappers  bootstrap.Bootstrappers
	repairPolicies []cloudprovider.RepairPolicy

	drifters []providers.Drifter
}

type instanceTypeOverlayStore interface {
	Apply(string, *cloudprovider.InstanceType) (*cloudprovider.InstanceType, error)
}

func NewOxideCloudProvider(
	kubeClient client.Client,
	ox *oxide.Client,
	project string,
	clusterUID types.UID,
	instanceProvider *instance.Provider,
	instanceTypeProvider *instancetype.Provider,
	instanceTypeStore *nodeoverlay.InstanceTypeStore,
	affinityPolicy *affinitypolicy.Tracker,
	bootstrappers bootstrap.Bootstrappers,
	repairPolicies []cloudprovider.RepairPolicy,
	drifters []providers.Drifter,
) *OxideCloudProvider {
	return &OxideCloudProvider{
		kubeClient: kubeClient,
		ox:         ox,
		project:    project,
		clusterUID: clusterUID,

		instanceProvider:     instanceProvider,
		instanceTypeProvider: instanceTypeProvider,
		instanceTypeStore:    instanceTypeStore,
		affinityPolicy:       affinityPolicy,
		bootstrappers:        bootstrappers,
		repairPolicies:       repairPolicies,
		drifters:             drifters,
	}
}

// Name returns the CloudProvider implementation name.
func (c *OxideCloudProvider) Name() string {
	return "oxide"
}

// Create launches a NodeClaim with the given resource requests and requirements.
func (c *OxideCloudProvider) Create(
	ctx context.Context,
	nodeClaim *karpcorev1.NodeClaim,
) (*karpcorev1.NodeClaim, error) {

	nodeClass := &v1alpha1.OxideNodeClass{}
	if err := c.kubeClient.Get(
		ctx,
		client.ObjectKey{Name: nodeClaim.Spec.NodeClassRef.Name},
		nodeClass,
	); err != nil {
		return nil, fmt.Errorf("unable to retrieve the OxideNodeClass, %w", err)
	}

	nodeTaints := append(
		append(nodeClaim.Spec.Taints, nodeClaim.Spec.StartupTaints...),
		corev1.Taint{
			Key:    karpcorev1.UnregisteredTaintKey,
			Effect: corev1.TaintEffectNoExecute,
		},
	)
	nodeTaints = lo.UniqBy(nodeTaints, func(t corev1.Taint) string { return t.Key })

	bootstrapper, err := c.bootstrappers.For(nodeClass)
	if err != nil {
		return nil, err
	}

	var candidates []string

	for _, req := range nodeClaim.Spec.Requirements {
		if req.Key != corev1.LabelInstanceTypeStable {
			continue
		}
		candidates = req.Values
	}

	blockedBy := instancetype.Reason("")
	available := lo.Filter(candidates, func(candidate string, _ int) bool {
		specs, err := instancetype.InstanceTypeToSpecs(candidate)
		if err != nil {
			return false
		}
		unavailable, reason := c.instanceTypeProvider.IsUnavailable(specs)
		if unavailable && (blockedBy == "" || reason == instancetype.ReasonQuota) {
			blockedBy = reason
		}
		return !unavailable
	})

	if len(candidates) > 0 && len(available) == 0 {
		return nil, cloudprovider.NewInsufficientCapacityError(
			c.noCandidatesError(candidates, blockedBy),
		)
	}

	ordered := lo.FilterMap(available, func(candidate string, _ int) (instancetype.Specs, bool) {
		specs, err := instancetype.InstanceTypeToSpecs(candidate)
		return specs, err == nil
	})
	slices.SortFunc(ordered, func(a, b instancetype.Specs) int {
		if cpuDelta := a.Ncpus - b.Ncpus; cpuDelta != 0 {
			return cpuDelta
		}
		return a.MemInGiB - b.MemInGiB
	})

	if len(ordered) == 0 {
		return nil, fmt.Errorf("no instance types match NodeClaim requirements")
	}

	instanceType, held := instancetype.Specs{}, false
	for _, specs := range ordered {
		if c.instanceTypeProvider.TryAcquire(specs) {
			instanceType, held = specs, true
			break
		}
	}
	if !held {

		return nil, fmt.Errorf(
			"all %d instance types the NodeClaim can use are being probed by another launch",
			len(ordered),
		)
	}

	defer c.instanceTypeProvider.ReleaseProbe(instanceType)
	effectiveInstanceType, err := c.effectiveInstanceType(
		ctx,
		nodeClaim,
		instancetype.SpecsToInstanceTypeName(instanceType.Ncpus, instanceType.MemInGiB),
	)
	if err != nil {
		return nil, err
	}
	name := InstanceNamePrefix + nodeClaim.Name

	launchToken := uuid.NewString()

	plan, err := storage.Build(name, launchToken, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("unable to plan the disks for %s: %w", nodeClaim.Name, err)
	}

	request := utilization.Request{
		CPUs:    int64(instanceType.Ncpus),
		Memory:  uint64(instanceType.MemInGiB) * GiB,
		Storage: uint64(plan.DistributedBytes()),
	}
	if quota, err := c.instanceTypeProvider.Quota(); err == nil {
		if admits, resource := quota.Admits(request); !admits {

			if resource != utilization.ResourceStorage {
				c.instanceTypeProvider.RecordICE(ctx, instanceType, instancetype.ReasonQuota)
			}
			return nil, cloudprovider.NewInsufficientCapacityError(
				fmt.Errorf("%s", quota.Explain(request, resource)),
			)
		}
	}

	subnet := nodeClass.Status.Subnets[rand.IntN(len(nodeClass.Status.Subnets))]

	describe := c.ownership(nodeClaim)

	userData, err := bootstrapper.UserData(ctx, nodeClass, bootstrap.Request{
		NodeClaim:  nodeClaim,
		NodeTaints: nodeTaints,
		Hostname:   name,
		Storage:    &plan,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to generate %s user-data: %w", bootstrapper.Name(), err)
	}

	launched, err := c.instanceProvider.Create(ctx, oxide.InstanceCreateParams{
		Project: oxide.NameOrId(c.project),
		Body: &oxide.InstanceCreate{
			Name:        oxide.Name(name),
			Hostname:    oxide.Hostname(name),
			Description: describe(ownership.KindInstance, "Karpenter provisioned node by NodePool"),
			Memory:      oxide.ByteCount(instanceType.MemInGiB * GiB),
			Ncpus:       oxide.InstanceCpuCount(instanceType.Ncpus),
			UserData:    base64.StdEncoding.EncodeToString([]byte(userData)),

			AntiAffinityGroups: antiAffinityGroups(nodeClass.Status.AffinityGroups),

			BootDisk: plan.BootAttachment(describe(ownership.KindBootDisk, "Boot disk")),
			Disks: plan.DataAttachments(func(disk storage.Disk) string {
				return describe(
					ownership.KindDataDisk,
					fmt.Sprintf("diskSet %s disk %d", disk.Set, disk.Ordinal),
				)
			}),
			NetworkInterfaces: oxide.InstanceNetworkInterfaceAttachment{
				Value: &oxide.InstanceNetworkInterfaceAttachmentCreate{
					Params: []oxide.InstanceNetworkInterfaceCreate{
						{
							Name:        oxide.Name(name),
							Description: "Karpenter provisioned network interface for node",
							SubnetName:  oxide.Name(subnet.Name),
							VpcName:     oxide.Name(subnet.VPCName),
						},
					},
				},
			},
			ExternalIps: externalIPs(nodeClass.Status.EphemeralIPs),
			SshPublicKeys: lo.Map(
				nodeClass.Status.SSHKeys,
				func(sshKey v1alpha1.OxideSSHKey, _ int) oxide.NameOrId {
					return oxide.NameOrId(sshKey.Name)
				},
			),
		},
	}, instance.Options{

		AffinityGroups: affinityGroups(nodeClass.Status.AffinityGroups),
	})
	if err != nil {
		return nil, c.diagnoseLaunchFailure(ctx, err, nodeClass, instanceType, request)
	}

	c.instanceTypeProvider.RecordSuccess(ctx, instanceType)

	c.affinityPolicy.RecordSuccess(ctx, nodeClass.Name)

	resolvedNodeClaim, err := c.resolveLaunched(
		ctx,
		launched,
		nodeClass,
		nodeClaim,
		name,
		subnet.ID,
		plan.Prefix,
	)
	if err != nil {
		return nil, err
	}
	annotateHourlyPrice(resolvedNodeClaim, effectiveInstanceType)

	return resolvedNodeClaim, nil
}

func (c *OxideCloudProvider) diagnoseLaunchFailure(
	ctx context.Context,
	err error,
	nodeClass *v1alpha1.OxideNodeClass,
	instanceType instancetype.Specs,
	request utilization.Request,
) error {

	var stuck *instance.StuckLaunchResourceError
	if errors.As(err, &stuck) {
		metrics.StuckLaunchResources.WithLabelValues(stuck.Kind, stuck.State).Inc()
		log.FromContext(ctx).Error(
			err,
			"abandoning a launch whose Oxide resource is stuck in a transitional state; "+
				"it is left in place for the orphan reaper to remove once this NodeClaim is gone",
			"kind", stuck.Kind,
			"resource", stuck.Name,
			"resource-id", stuck.ID,
			"state", stuck.State,
		)
		return cloudprovider.NewInsufficientCapacityError(err)
	}

	if errors.Is(err, instance.ErrInsufficientStorage) {

		reason, diagnosis := c.diagnoseRefusal(
			utilization.Request{Storage: request.Storage},
			err,
		)

		c.instanceTypeProvider.RecordStorageRefusal(ctx, reason, diagnosis.Error())
		if reason == instancetype.ReasonQuota {

			c.instanceTypeProvider.RefreshQuota(ctx)
		}
		return cloudprovider.NewInsufficientCapacityError(diagnosis)
	}
	if errors.Is(err, instance.ErrInsufficientCapacity) {

		reason, diagnosis := c.diagnoseRefusal(
			utilization.Request{CPUs: request.CPUs, Memory: request.Memory},
			err,
		)

		if reason == instancetype.ReasonCapacity &&
			affinitypolicy.Attributable(nodeClass.Status.AffinityGroups) {
			c.affinityPolicy.RecordRefusal(ctx, nodeClass.Name, diagnosis.Error())
			return cloudprovider.NewInsufficientCapacityError(diagnosis)
		}

		c.instanceTypeProvider.RecordICE(ctx, instanceType, reason)
		if reason == instancetype.ReasonQuota {

			c.instanceTypeProvider.RefreshQuota(ctx)
		}
		return cloudprovider.NewInsufficientCapacityError(diagnosis)
	}
	return err
}

func (c *OxideCloudProvider) resolveLaunched(
	ctx context.Context,
	launched *oxide.Instance,
	nodeClass *v1alpha1.OxideNodeClass,
	nodeClaim *karpcorev1.NodeClaim,
	instanceName string,
	subnetID string,
	diskPrefix string,
) (*karpcorev1.NodeClaim, error) {
	resolved, err := c.InstanceToNodeClaim(launched, nodeClass, nodeClaim.DeepCopy())
	if err != nil {
		return nil, err
	}

	ephemeralIPs, err := c.ephemeralIPRecords(ctx, nodeClass, instanceName)
	if err != nil {
		return nil, err
	}

	resolved.Annotations = lo.Assign(resolved.Annotations, map[string]string{
		v1alpha1.AnnotationOxideNodeClassHash:        nodeClass.Hash(),
		v1alpha1.AnnotationOxideNodeClassHashVersion: v1alpha1.OxideNodeClassHashVersion,
		v1alpha1.AnnotationSubnet:                    subnetID,

		v1alpha1.AnnotationDiskPrefix: diskPrefix,

		v1alpha1.AnnotationSSHKeys: v1alpha1.EncodeIDs(
			v1alpha1.SSHKeyIDs(nodeClass.Status.SSHKeys),
		),

		v1alpha1.AnnotationEphemeralIPs: v1alpha1.EncodeEphemeralIPs(ephemeralIPs),

		v1alpha1.AnnotationDiskSetSources: v1alpha1.EncodeDiskSetSources(
			v1alpha1.DiskSetSources(nodeClass.Status.Storage),
		),
	})

	if len(nodeClass.Status.AffinityGroups) > 0 {
		resolved.Annotations[v1alpha1.AnnotationAffinityGroups] = v1alpha1.EncodeAffinityGroups(
			v1alpha1.AffinityGroupRefsFor(nodeClass.Status.AffinityGroups),
		)
	}
	return resolved, nil
}

func (c *OxideCloudProvider) ownership(
	nodeClaim *karpcorev1.NodeClaim,
) func(ownership.Kind, string) string {
	return func(kind ownership.Kind, human string) string {
		return ownership.Marker{
			ClusterUID:    c.clusterUID,
			NodeClaimUID:  nodeClaim.UID,
			NodeClaimName: nodeClaim.Name,
			Kind:          kind,
		}.Description(human)
	}
}

func externalIPs(ephemeralIPs []v1alpha1.OxideEphemeralIP) []oxide.ExternalIpCreate {
	return lo.Map(
		ephemeralIPs,
		func(ephemeralIP v1alpha1.OxideEphemeralIP, _ int) oxide.ExternalIpCreate {

			selector := oxide.PoolSelector{
				Value: &oxide.PoolSelectorAuto{IpVersion: oxide.IpVersion(ephemeralIP.IPVersion)},
			}
			if ephemeralIP.PoolName != "" {
				selector = oxide.PoolSelector{
					Value: &oxide.PoolSelectorExplicit{Pool: oxide.NameOrId(ephemeralIP.PoolName)},
				}
			}
			return oxide.ExternalIpCreate{
				Value: &oxide.ExternalIpCreateEphemeral{PoolSelector: selector},
			}
		},
	)
}

func (c *OxideCloudProvider) ephemeralIPRecords(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	instanceName string,
) ([]v1alpha1.EphemeralIPRecord, error) {
	if len(nodeClass.Status.EphemeralIPs) == 0 {
		return nil, nil
	}
	listed, err := c.ox.InstanceExternalIpList(ctx, oxide.InstanceExternalIpListParams{
		Project:  oxide.NameOrId(c.project),
		Instance: oxide.NameOrId(instanceName),
	})
	if err != nil {
		return nil, fmt.Errorf(
			"unable to read the external addresses of instance %q: %w",
			instanceName,
			err,
		)
	}

	records := make([]v1alpha1.EphemeralIPRecord, 0, len(listed.Items))
	for _, externalIP := range listed.Items {
		ephemeral, ok := externalIP.Value.(*oxide.ExternalIpEphemeral)
		if !ok {
			continue
		}
		address, err := netip.ParseAddr(ephemeral.Ip)
		if err != nil {
			return nil, fmt.Errorf(
				"instance %q reported the unparseable external address %q: %w",
				instanceName,
				ephemeral.Ip,
				err,
			)
		}
		records = append(records, v1alpha1.EphemeralIPRecord{

			IPVersion: lo.Ternary(
				address.Is4(),
				string(oxide.IpVersionV4),
				string(oxide.IpVersionV6),
			),
			Address: address.String(),
			PoolID:  ephemeral.IpPoolId,
		})
	}
	return records, nil
}

func (c *OxideCloudProvider) effectiveInstanceType(
	ctx context.Context,
	nodeClaim *karpcorev1.NodeClaim,
	instanceTypeName string,
) (*cloudprovider.InstanceType, error) {
	instanceType, ok := c.instanceTypeProvider.Get(instanceTypeName)
	if !ok {
		return nil, fmt.Errorf("selected instance type %q is no longer available", instanceTypeName)
	}
	if !karpoptions.FromContext(ctx).FeatureGates.NodeOverlay {
		return instanceType, nil
	}
	nodePoolName, ok := nodeClaim.Labels[karpcorev1.NodePoolLabelKey]
	if !ok {

		return instanceType, nil
	}
	effective, err := c.instanceTypeStore.Apply(nodePoolName, instanceType)
	if err != nil {
		return nil, fmt.Errorf(
			"applying nodeoverlays to instance type %q, %w",
			instanceTypeName,
			err,
		)
	}
	return effective, nil
}

func annotateHourlyPrice(
	nodeClaim *karpcorev1.NodeClaim,
	instanceType *cloudprovider.InstanceType,
) {
	price, ok := instanceType.OfferingPrice(
		nodeClaim.Labels[corev1.LabelTopologyZone],
		nodeClaim.Labels[karpcorev1.CapacityTypeLabelKey],
	)
	if !ok {
		return
	}
	if nodeClaim.Annotations == nil {
		nodeClaim.Annotations = map[string]string{}
	}
	nodeClaim.Annotations[v1alpha1.AnnotationHourlyPrice] = strconv.FormatFloat(price, 'f', -1, 64)
}

// Delete removes a NodeClaim from the cloudprovider by its provider id.
func (c *OxideCloudProvider) Delete(ctx context.Context, nodeClaim *karpcorev1.NodeClaim) error {
	instanceID, err := InstanceIDFromProviderID(nodeClaim.Status.ProviderID)
	if err != nil {
		return fmt.Errorf("invalid provider id, must use the oxide ccm: %w", err)
	}

	name := InstanceNamePrefix + nodeClaim.Name

	if err := c.instanceProvider.Delete(ctx, instanceID, teardownFor(nodeClaim, name)); err != nil {
		if errors.Is(err, instance.ErrNotFound) {
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return err
	}

	return cloudprovider.NewNodeClaimNotFoundError(instance.ErrNotFound)
}

func teardownFor(nodeClaim *karpcorev1.NodeClaim, instanceName string) instance.Teardown {
	if prefix := nodeClaim.Annotations[v1alpha1.AnnotationDiskPrefix]; prefix != "" {

		return instance.Teardown{DiskPrefix: prefix}
	}
	return instance.Teardown{
		BootDiskName: instanceName,
		DiskPrefix:   storage.LegacyPrefix(instanceName),
	}
}

// Get retrieves a NodeClaim by provider ID.
func (c *OxideCloudProvider) Get(
	ctx context.Context,
	providerID string,
) (*karpcorev1.NodeClaim, error) {
	instanceID, err := InstanceIDFromProviderID(providerID)
	if err != nil {
		return nil, err
	}
	oxideInstance, err := c.ox.InstanceView(
		ctx,
		oxide.InstanceViewParams{Instance: oxide.NameOrId(instanceID)},
	)
	if err != nil {

		if errors.Is(err, oxide.ErrObjectNotFound) {
			return nil, cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return nil, fmt.Errorf("unable to get instance with instance ID %s, %w", instanceID, err)
	}
	nodeClaim, err := c.InstanceToNodeClaim(oxideInstance, nil, nil)
	if err != nil {
		return nil, err
	}
	return nodeClaim, nil
}

func (c *OxideCloudProvider) InstanceToNodeClaim(
	oxideInstance *oxide.Instance,
	nodeClass *v1alpha1.OxideNodeClass,
	nodeClaim *karpcorev1.NodeClaim,
) (*karpcorev1.NodeClaim, error) {
	if nodeClaim == nil {
		nodeClaim = &karpcorev1.NodeClaim{
			ObjectMeta: v1.ObjectMeta{
				Name: strings.TrimPrefix(oxideInstance.Hostname, InstanceNamePrefix),
			},
		}
	}
	nodeClaim.Status.ProviderID = fmt.Sprintf("oxide://%s", oxideInstance.Id)
	labels := map[string]string{}
	if it, ok := c.instanceTypeProvider.Get(
		instancetype.SpecsToInstanceTypeName(
			int(oxideInstance.Ncpus),
			int(oxideInstance.Memory)/GiB,
		),
	); ok {
		for key, req := range it.Requirements {
			if req.Operator() != corev1.NodeSelectorOpIn {
				continue
			}
			labels[key] = req.Values()[0]
		}
		for key, req := range it.Offerings.Cheapest().Requirements {
			if req.Operator() != corev1.NodeSelectorOpIn {
				continue
			}
			labels[key] = req.Values()[0]
		}
		nodeClaim.Status.Allocatable = it.Allocatable()
		nodeClaim.Status.Capacity = it.Capacity
	}
	labels = lo.Assign(labels, nodeClaim.Labels)
	nodeClaim.Labels = labels
	if nodeClass != nil {
		nodeClaim.Status.ImageID = lo.FirstOrEmpty(nodeClass.Status.Images).ID
	}
	return nodeClaim, nil
}

// InstanceIDFromProviderID extracts the Oxide instance ID from a provider ID.
func InstanceIDFromProviderID(providerID string) (string, error) {
	if providerID == "" {
		return "", errors.New("provider id is empty")
	}

	if !strings.HasPrefix(providerID, "oxide://") {
		return "", errors.New("provider id does not have 'oxide://' prefix")
	}

	instanceID := strings.TrimPrefix(providerID, "oxide://")

	if _, err := uuid.Parse(instanceID); err != nil {
		return "", fmt.Errorf("provider id contains invalid uuid: %w", err)
	}

	return instanceID, nil
}

// List retrieves all NodeClaims.
func (c *OxideCloudProvider) List(ctx context.Context) ([]*karpcorev1.NodeClaim, error) {
	instances, err := c.ox.InstanceListAllPages(
		ctx,
		oxide.InstanceListParams{Project: oxide.NameOrId(c.project)},
	)
	if err != nil {
		return nil, err
	}
	var nodeClaims []*karpcorev1.NodeClaim
	for _, oxideInstance := range instances {
		if !strings.HasPrefix(string(oxideInstance.Name), InstanceNamePrefix) {
			continue
		}
		nodeClaim, err := c.InstanceToNodeClaim(&oxideInstance, nil, nil)
		if err != nil {
			return nil, err
		}
		nodeClaims = append(nodeClaims, nodeClaim)
	}
	return nodeClaims, nil
}

// GetInstanceTypes returns instance types supported by the cloudprovider.
func (c *OxideCloudProvider) GetInstanceTypes(
	context.Context,
	*karpcorev1.NodePool,
) ([]*cloudprovider.InstanceType, error) {
	return c.instanceTypeProvider.InstanceTypes(), nil
}

func (c *OxideCloudProvider) noCandidatesError(
	candidates []string,
	reason instancetype.Reason,
) error {
	if reason != instancetype.ReasonQuota {
		return fmt.Errorf(
			"all %d instance types the NodeClaim can use were recently refused for insufficient capacity",
			len(candidates),
		)
	}

	quota, err := c.instanceTypeProvider.Quota()
	if err != nil {
		return fmt.Errorf(
			"all %d instance types the NodeClaim can use exceed the silo's quota",
			len(candidates),
		)
	}

	smallest := smallestSpecs(candidates)
	request := utilization.Request{
		CPUs:   int64(smallest.Ncpus),
		Memory: uint64(smallest.MemInGiB) * GiB,
	}
	_, resource := quota.Admits(request)
	if resource == "" {
		return fmt.Errorf(
			"all %d instance types the NodeClaim can use exceed the silo's quota",
			len(candidates),
		)
	}
	return fmt.Errorf(
		"none of the %d instance types the NodeClaim can use fit in the silo's quota: %s",
		len(candidates),
		quota.Explain(request, resource),
	)
}

func (c *OxideCloudProvider) diagnoseRefusal(
	request utilization.Request,
	cause error,
) (instancetype.Reason, error) {
	quota, err := c.instanceTypeProvider.Quota()
	if err != nil {
		return instancetype.ReasonCapacity, cause
	}
	admits, resource := quota.Admits(request)
	if admits {
		return instancetype.ReasonCapacity, cause
	}
	return instancetype.ReasonQuota, fmt.Errorf(
		"%s, so the launch was refused: %w",
		quota.Explain(request, resource),
		cause,
	)
}

func antiAffinityGroups(groups []v1alpha1.OxideAffinityGroup) []oxide.NameOrId {
	return lo.FilterMap(
		groups,
		func(group v1alpha1.OxideAffinityGroup, _ int) (oxide.NameOrId, bool) {
			return oxide.NameOrId(group.ID), group.Type == v1alpha1.AffinityGroupTypeAntiAffinity
		},
	)
}

func affinityGroups(groups []v1alpha1.OxideAffinityGroup) []string {
	return lo.FilterMap(
		groups,
		func(group v1alpha1.OxideAffinityGroup, _ int) (string, bool) {
			return group.ID, group.Type == v1alpha1.AffinityGroupTypeAffinity
		},
	)
}

func smallestSpecs(candidates []string) instancetype.Specs {
	smallest := instancetype.Specs{}
	for _, candidate := range candidates {
		specs, err := instancetype.InstanceTypeToSpecs(candidate)
		if err != nil {
			continue
		}
		if smallest.Ncpus == 0 ||
			specs.Ncpus < smallest.Ncpus ||
			(specs.Ncpus == smallest.Ncpus && specs.MemInGiB < smallest.MemInGiB) {
			smallest = specs
		}
	}
	return smallest
}

// RepairPolicy is for CloudProviders to define a set Unhealthy condition for Karpenter to monitor on the node.
func (c *OxideCloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return c.repairPolicies
}

// GetSupportedNodeClasses returns CloudProvider NodeClass that implements status.Object NOTE.
func (c *OxideCloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.OxideNodeClass{}}
}
