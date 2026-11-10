package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/google/uuid"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/bootdisk"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	projectscope "github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/storagerefusal"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	karpoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	GiB                = 1 << 30
	InstanceNamePrefix = "karpenter-"

	bootDiskReleaseTimeout = 30 * time.Second
)

type OxideCloudProvider struct {
	kubeClient client.Client
	ox         *oxide.Client
	project    string

	clusterUID types.UID

	instanceProvider     *instance.Provider
	bootDiskPool         *bootdisk.Provider
	instanceTypeProvider *instancetype.Provider
	instanceTypeStore    instanceTypeOverlayStore
	availability         *availability.Manager
	storageRefusals      *storagerefusal.Tracker

	bootstrappers  *bootstrap.Bootstrappers
	repairPolicies []cloudprovider.RepairPolicy

	drifters []providers.Drifter
}

type instanceTypeOverlayStore interface {
	Apply(string, *cloudprovider.InstanceType) (*cloudprovider.InstanceType, error)
}

type launchBootDisk struct {
	attachment   oxide.InstanceDiskAttachment
	name         string
	storageBytes int64
	acquired     *bootdisk.Acquired
}

func NewOxideCloudProvider(
	kubeClient client.Client,
	ox *oxide.Client,
	project string,
	clusterUID types.UID,
	instanceProvider *instance.Provider,
	bootDiskPool *bootdisk.Provider,
	instanceTypeProvider *instancetype.Provider,
	instanceTypeStore *nodeoverlay.InstanceTypeStore,
	availabilityManager *availability.Manager,
	storageRefusals *storagerefusal.Tracker,
	bootstrappers *bootstrap.Bootstrappers,
	repairPolicies []cloudprovider.RepairPolicy,
	drifters []providers.Drifter,
) *OxideCloudProvider {
	return &OxideCloudProvider{
		kubeClient: kubeClient,
		ox:         ox,
		project:    project,
		clusterUID: clusterUID,

		instanceProvider:     instanceProvider,
		bootDiskPool:         bootDiskPool,
		instanceTypeProvider: instanceTypeProvider,
		instanceTypeStore:    instanceTypeStore,
		availability:         availabilityManager,
		storageRefusals:      storageRefusals,
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

	// Fail fast so the launch reconciler replaces the NodeClaim rather than
	// retrying against a nodeclass that can't resolve its images, subnets, or storage.
	if !nodeClass.StatusConditions().Root().IsTrue() {
		return nil, cloudprovider.NewNodeClassNotReadyError(fmt.Errorf(
			"resolving nodeclass readiness, %s",
			nodeClass.StatusConditions().Root().Message,
		))
	}

	candidate, probe, err := c.selectLaunch(nodeClaim, nodeClass)
	if err != nil {
		return nil, err
	}
	defer probe.Release()

	plan, err := c.planLaunch(ctx, nodeClaim, nodeClass, candidate)
	if err != nil {
		return nil, err
	}
	boot := c.bootDisk(ctx, nodeClass, nodeClaim, plan.storage)
	bootConsumed := false
	defer c.releaseUnstartedBootDisk(ctx, boot.acquired, &bootConsumed)

	result, err := c.executeLaunch(ctx, nodeClass, plan, boot, probe)
	if err != nil {
		return nil, err
	}
	bootConsumed = !result.Adopted || boot.acquired == nil ||
		result.Instance.BootDiskId == boot.acquired.DiskID

	if !result.Adopted {
		probe.RecordSuccess(ctx)
		c.storageRefusals.Clear(ctx)
	}
	return c.recordLaunch(ctx, nodeClaim, nodeClass, plan, boot, result)
}

// launchCandidate is one shape under one pin that a NodeClaim allows.
type launchCandidate struct {
	specs    instancetype.Specs
	platform availability.Platform
}

// cpuPlatformsFor is the pins a NodeClaim allows, in launch order. A NodeClaim
// that says nothing about the platform gets the default pin, which is what an
// unpinned guest would see anyway but leaves the node labelled.
func cpuPlatformsFor(nodeClaim *karpcorev1.NodeClaim) ([]availability.Platform, error) {
	requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(
		nodeClaim.Spec.Requirements...,
	)
	if !requirements.Has(v1alpha1.LabelInstanceCPUPlatform) {
		return []availability.Platform{availability.Platform(v1alpha1.CPUPlatformDefault)}, nil
	}
	requirement := requirements.Get(v1alpha1.LabelInstanceCPUPlatform)
	var platforms []availability.Platform
	for _, platform := range v1alpha1.CPUPlatforms {
		if requirement.Has(platform) {
			platforms = append(platforms, availability.Platform(platform))
		}
	}
	if len(platforms) == 0 {
		return nil, fmt.Errorf(
			"the NodeClaim requires a CPU platform the provider does not offer: %s %s",
			requirement.Operator(),
			requirement.Values(),
		)
	}
	return platforms, nil
}

func selectSubnet(subnets []v1alpha1.OxideSubnet) (v1alpha1.OxideSubnet, error) {
	if len(subnets) == 0 {
		return v1alpha1.OxideSubnet{}, cloudprovider.NewNodeClassNotReadyError(
			errors.New("nodeclass has no resolved subnets"),
		)
	}
	return subnets[rand.IntN(len(subnets))], nil
}

func (c *OxideCloudProvider) bootDisk(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	nodeClaim *karpcorev1.NodeClaim,
	plan storage.Plan,
) launchBootDisk {
	launch := launchBootDisk{
		attachment: plan.BootAttachment(
			c.ownership(nodeClaim)(ownership.KindBootDisk, "Boot disk"),
		),
		name:         plan.Boot.Name,
		storageBytes: plan.DistributedBytes(),
	}
	if c.bootDiskPool == nil {
		return launch
	}
	acquired, err := c.bootDiskPool.Acquire(ctx, nodeClass, nodeClaim.Name, nodeClaim.UID)
	if err != nil {
		metrics.BootDiskPoolAcquisitions.WithLabelValues("error").Inc()
		log.FromContext(ctx).V(1).Info("boot disk pool unavailable", "reason", err.Error())
		return launch
	}
	if acquired == nil {
		metrics.BootDiskPoolAcquisitions.WithLabelValues("miss").Inc()
		return launch
	}
	metrics.BootDiskPoolAcquisitions.WithLabelValues("hit").Inc()
	launch.attachment = oxide.InstanceDiskAttachment{
		Value: &oxide.InstanceDiskAttachmentAttach{Name: oxide.Name(acquired.DiskName)},
	}
	launch.name = acquired.DiskName
	launch.storageBytes -= plan.Boot.SizeBytes
	launch.acquired = acquired
	return launch
}

// releaseUnstartedBootDisk hands a pooled disk back when the launch it was
// acquired for never started. Aborting on quota or capacity is routine, so
// retiring here would burn a warm disk on every refusal.
func (c *OxideCloudProvider) releaseUnstartedBootDisk(
	ctx context.Context,
	acquired *bootdisk.Acquired,
	started *bool,
) {
	if acquired == nil || *started {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		bootDiskReleaseTimeout,
	)
	defer cancel()
	err := c.bootDiskPool.Unclaim(cleanupCtx, acquired.RecordName, acquired.ClaimUID)
	if err != nil {
		log.FromContext(ctx).V(1).Info(
			"unable to release a claimed boot disk",
			"disk", acquired.DiskName,
			"reason", err.Error(),
		)
	}
}

func (c *OxideCloudProvider) diagnoseLaunchFailure(
	ctx context.Context,
	err error,
	nodeClass *v1alpha1.OxideNodeClass,
	request utilization.Request,
	probe *availability.Probe,
) error {

	if stuck, ok := errors.AsType[*instance.StuckLaunchResourceError](err); ok {
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

		c.storageRefusals.Record(ctx, reason, diagnosis.Error())
		if reason == availability.ReasonQuota {

			c.instanceTypeProvider.RefreshQuota(ctx)
		}
		return cloudprovider.NewInsufficientCapacityError(diagnosis)
	}
	if errors.Is(err, instance.ErrInsufficientCapacity) {

		reason, diagnosis := c.diagnoseRefusal(
			utilization.Request{CPUs: request.CPUs, Memory: request.Memory},
			err,
		)

		placement := placementFor(nodeClass.Status.AffinityGroups)
		if reason == availability.ReasonCapacity && placement != "" {
			probe.RecordRefusal(ctx, availability.Refusal{
				Scope:   availability.RefusalScopePlacement,
				Reason:  reason,
				Message: diagnosis.Error(),
			})
			return cloudprovider.NewInsufficientCapacityError(diagnosis)
		}

		probe.RecordRefusal(ctx, availability.Refusal{
			Scope:  availability.RefusalScopeGlobal,
			Reason: reason,
		})
		if reason == availability.ReasonQuota {

			c.instanceTypeProvider.RefreshQuota(ctx)
		}
		return cloudprovider.NewInsufficientCapacityError(diagnosis)
	}
	return err
}

func (c *OxideCloudProvider) ownership(
	nodeClaim *karpcorev1.NodeClaim,
) func(ownership.Kind, string) string {
	owner := c.owner(nodeClaim)
	return func(kind ownership.Kind, human string) string {
		marker := owner
		marker.Kind = kind
		return marker.Description(human)
	}
}

func (c *OxideCloudProvider) owner(nodeClaim *karpcorev1.NodeClaim) ownership.Marker {
	return ownership.Marker{
		ClusterUID:    c.clusterUID,
		NodeClaimUID:  nodeClaim.UID,
		NodeClaimName: nodeClaim.Name,
		Kind:          ownership.KindInstance,
	}
}

// externalIPSets returns every combination of the entries' pools, the first choice of each
// entry first, so a launch can fall back as pools run out of addresses.
func externalIPSets(ephemeralIPs []v1alpha1.OxideEphemeralIP) [][]oxide.ExternalIpCreate {
	sets := [][]oxide.ExternalIpCreate{{}}
	for _, ephemeralIP := range ephemeralIPs {
		extended := make([][]oxide.ExternalIpCreate, 0, len(sets)*(1+len(ephemeralIP.Fallbacks)))
		for _, set := range sets {
			for _, pool := range poolChoices(ephemeralIP) {
				extended = append(extended, append(slices.Clone(set), pool))
			}
		}
		sets = extended
	}
	return sets
}

func poolChoices(ephemeralIP v1alpha1.OxideEphemeralIP) []oxide.ExternalIpCreate {
	explicit := func(name string) oxide.ExternalIpCreate {
		return oxide.ExternalIpCreate{Value: &oxide.ExternalIpCreateEphemeral{
			PoolSelector: oxide.PoolSelector{
				Value: &oxide.PoolSelectorExplicit{Pool: oxide.NameOrId(name)},
			},
		}}
	}
	if ephemeralIP.PoolName == "" {
		return []oxide.ExternalIpCreate{{Value: &oxide.ExternalIpCreateEphemeral{
			PoolSelector: oxide.PoolSelector{
				Value: &oxide.PoolSelectorAuto{IpVersion: oxide.IpVersion(ephemeralIP.IPVersion)},
			},
		}}}
	}
	choices := []oxide.ExternalIpCreate{explicit(ephemeralIP.PoolName)}
	for _, pool := range ephemeralIP.Fallbacks {
		choices = append(choices, explicit(pool.Name))
	}
	return choices
}

func (c *OxideCloudProvider) ephemeralIPRecords(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	instanceName string,
) ([]v1alpha1.EphemeralIPRecord, error) {
	if len(nodeClass.Status.EphemeralIPs) == 0 {
		return nil, nil
	}
	return c.readEphemeralIPRecords(ctx, instanceName)
}

func (c *OxideCloudProvider) readEphemeralIPRecords(
	ctx context.Context,
	instanceName string,
) ([]v1alpha1.EphemeralIPRecord, error) {
	listed, err := c.ox.InstanceExternalIpList(ctx, oxide.InstanceExternalIpListParams{
		Project:  projectscope.Scope(c.project, instanceName),
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
	catalog instancetype.Options,
) (*cloudprovider.InstanceType, error) {
	instanceType, ok := c.instanceTypeProvider.Get(instanceTypeName, catalog)
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

	if err := c.instanceProvider.Delete(
		ctx, instanceID, teardownFor(nodeClaim, name, c.owner(nodeClaim)),
	); err != nil {
		if errors.Is(err, instance.ErrNotFound) {
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return err
	}

	return cloudprovider.NewNodeClaimNotFoundError(instance.ErrNotFound)
}

func teardownFor(
	nodeClaim *karpcorev1.NodeClaim,
	instanceName string,
	owner ownership.Marker,
) instance.Teardown {
	if ids, found := nodeClaim.Annotations[v1alpha1.AnnotationDiskIDs]; found {
		return instance.Teardown{Owner: owner,
			BootDiskName: nodeClaim.Annotations[v1alpha1.AnnotationBootDisk],
			DiskIDs:      v1alpha1.ParseIDs(ids).UnsortedList()}
	}

	if prefix := nodeClaim.Annotations[v1alpha1.AnnotationDiskPrefix]; prefix != "" {
		return instance.Teardown{
			Owner:        owner,
			BootDiskName: nodeClaim.Annotations[v1alpha1.AnnotationBootDisk],
			DiskPrefix:   prefix,
		}
	}
	return instance.Teardown{
		Owner:        owner,
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
	marker, verifyErr := ownership.VerifyClusterResource(
		oxideInstance.Description, c.clusterUID, ownership.KindInstance,
	)
	if verifyErr != nil {
		c.rejectOwnership(
			ctx,
			"get",
			"instance",
			oxideInstance.Id,
			string(oxideInstance.Name),
			verifyErr,
		)
		return nil, cloudprovider.NewNodeClaimNotFoundError(verifyErr)
	}
	nodeClaim, err := c.InstanceToNodeClaim(oxideInstance, nil, &karpcorev1.NodeClaim{
		Name: marker.NodeClaimName,
	})
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
			Name: strings.TrimPrefix(oxideInstance.Hostname, InstanceNamePrefix),
		}
	}
	nodeClaim.Status.ProviderID = fmt.Sprintf("oxide://%s", oxideInstance.Id)
	labels := map[string]string{}
	catalog, err := instancetype.OptionsFor(nodeClass)
	if err != nil {
		return nil, err
	}
	if it, ok := c.instanceTypeProvider.Get(
		instancetype.SpecsToInstanceTypeName(
			int(oxideInstance.Ncpus),
			int(oxideInstance.Memory)/GiB,
		),
		catalog,
	); ok {
		for key, req := range it.Requirements {
			if req.Operator() != corev1.NodeSelectorOpIn || req.Len() != 1 {
				continue
			}
			labels[key] = req.Any()
		}
		// The platform is the one dimension offerings differ in, and only the
		// instance says which one it launched under, if any.
		for key, req := range it.Offerings.Cheapest().Requirements {
			if req.Operator() != corev1.NodeSelectorOpIn || req.Len() != 1 {
				continue
			}
			if key == v1alpha1.LabelInstanceCPUPlatform {
				continue
			}
			labels[key] = req.Any()
		}
		nodeClaim.Status.Allocatable = it.Allocatable()
		nodeClaim.Status.Capacity = it.Capacity
	}
	if oxideInstance.CpuPlatform != "" {
		labels[v1alpha1.LabelInstanceCPUPlatform] = string(oxideInstance.CpuPlatform)
	}
	labels = lo.Assign(labels, nodeClaim.Labels)
	nodeClaim.Labels = labels
	nodeClaim.Annotations = lo.Assign(
		nodeClaim.Annotations,
		c.instanceTypeProvider.TopologyAnnotations(),
	)
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
		marker, verifyErr := ownership.VerifyClusterResource(
			oxideInstance.Description, c.clusterUID, ownership.KindInstance,
		)
		if verifyErr != nil {
			c.rejectOwnership(
				ctx, "list", "instance", oxideInstance.Id, string(oxideInstance.Name), verifyErr,
			)
			continue
		}
		nodeClaim, err := c.InstanceToNodeClaim(&oxideInstance, nil, &karpcorev1.NodeClaim{
			Name: marker.NodeClaimName,
		})
		if err != nil {
			return nil, err
		}
		nodeClaims = append(nodeClaims, nodeClaim)
	}
	return nodeClaims, nil
}

func (c *OxideCloudProvider) rejectOwnership(
	ctx context.Context,
	operation string,
	resourceKind string,
	id string,
	name string,
	err error,
) {
	conflict, ok := ownership.Conflict(err)
	if !ok {
		return
	}
	metrics.OwnershipRejections.WithLabelValues(
		operation, resourceKind, string(conflict.Reason),
	).Inc()
	log.FromContext(ctx).Info(
		"rejected an Oxide resource whose ownership could not be verified",
		"operation", operation,
		"resource-kind", resourceKind,
		"resource-id", id,
		"resource", name,
		"reason", conflict.Reason,
		"expected-cluster-uid", conflict.Expected.ClusterUID,
		"observed-cluster-uid", conflict.Observed.ClusterUID,
		"expected-nodeclaim", conflict.Expected.NodeClaimName,
		"observed-nodeclaim", conflict.Observed.NodeClaimName,
		"expected-nodeclaim-uid", conflict.Expected.NodeClaimUID,
		"observed-nodeclaim-uid", conflict.Observed.NodeClaimUID,
		"expected-kind", conflict.Expected.Kind,
		"observed-kind", conflict.Observed.Kind,
	)
}

// GetInstanceTypes returns the catalog with ephemeral storage and kubelet overhead
// determined by the NodePool's NodeClass.
func (c *OxideCloudProvider) GetInstanceTypes(
	ctx context.Context,
	nodePool *karpcorev1.NodePool,
) ([]*cloudprovider.InstanceType, error) {
	nodeClass, err := c.nodeClassFor(ctx, nodePool)
	if err != nil {
		return nil, err
	}
	catalog, err := instancetype.OptionsFor(nodeClass)
	if err != nil {
		return nil, err
	}
	return c.instanceTypeProvider.InstanceTypes(catalog), nil
}

func (c *OxideCloudProvider) nodeClassFor(
	ctx context.Context,
	nodePool *karpcorev1.NodePool,
) (*v1alpha1.OxideNodeClass, error) {
	if nodePool == nil || nodePool.Spec.Template.Spec.NodeClassRef == nil {
		return nil, nil
	}
	nodeClass := &v1alpha1.OxideNodeClass{}
	if err := c.kubeClient.Get(
		ctx,
		client.ObjectKey{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name},
		nodeClass,
	); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, cloudprovider.NewNodeClassNotReadyError(err)
		}
		return nil, fmt.Errorf("unable to retrieve the OxideNodeClass, %w", err)
	}
	return nodeClass, nil
}

func (c *OxideCloudProvider) diagnoseRefusal(
	request utilization.Request,
	cause error,
) (availability.Reason, error) {
	quota, err := c.instanceTypeProvider.Quota()
	if err != nil {
		return availability.ReasonCapacity, cause
	}
	admits, resource := quota.Admits(request)
	if admits {
		return availability.ReasonCapacity, cause
	}
	return availability.ReasonQuota, fmt.Errorf(
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

func placementFor(groups []v1alpha1.OxideAffinityGroup) availability.Placement {
	constraints := lo.FilterMap(
		groups,
		func(group v1alpha1.OxideAffinityGroup, _ int) (availability.Constraint, bool) {
			return availability.Constraint{Kind: group.Type, ID: group.ID}, group.Policy == "fail"
		},
	)
	return availability.NewPlacement(constraints...)
}

// Reboot restarts the instance in place. The operation ID is unused: Oxide has
// no idempotency key, and a repeat reboot of a running instance is harmless.
func (c *OxideCloudProvider) Reboot(
	ctx context.Context,
	nodeClaim *karpcorev1.NodeClaim,
	_ string,
) error {
	instanceID, err := InstanceIDFromProviderID(nodeClaim.Status.ProviderID)
	if err != nil {
		return cloudprovider.NewNodeRebootFailedError(
			fmt.Errorf("invalid provider id, must use the oxide ccm: %w", err))
	}
	if err := c.instanceProvider.Reboot(ctx, instanceID); err != nil {
		switch {
		case errors.Is(err, instance.ErrNotFound):
			return cloudprovider.NewNodeClaimNotFoundError(err)
		case errors.Is(err, instance.ErrRebootRejected):
			return cloudprovider.NewNodeRebootFailedError(err)
		}
		return err
	}
	return nil
}

// RepairPolicy is for CloudProviders to define a set Unhealthy condition for Karpenter to monitor on the node.
func (c *OxideCloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return c.repairPolicies
}

// GetSupportedNodeClasses returns CloudProvider NodeClass that implements status.Object NOTE.
func (c *OxideCloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.OxideNodeClass{}}
}
