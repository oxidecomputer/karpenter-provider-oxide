package cloudprovider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	"github.com/google/uuid"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instance"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const (
	GiB                = 1 << 30
	InstanceNamePrefix = "karpenter-"
)

type OxideCloudProvider struct {
	kubeClient client.Client
	ox         *oxide.Client
	project    string

	instanceProvider     *instance.Provider
	instanceTypeProvider *instancetype.Provider
	bootstrappers        bootstrap.Bootstrappers
	repairPolicies       []cloudprovider.RepairPolicy
}

func NewOxideCloudProvider(
	kubeClient client.Client,
	ox *oxide.Client,
	project string,
	instanceTypeProvider *instancetype.Provider,
	bootstrappers bootstrap.Bootstrappers,
	repairPolicies []cloudprovider.RepairPolicy,
) *OxideCloudProvider {
	return &OxideCloudProvider{
		kubeClient: kubeClient,
		ox:         ox,
		project:    project,
		// Every teardown reports what it gave back, including the ones that happen inside a failed
		// launch. A shape the rack refused an hour ago may well fit in the room a terminating node
		// just left, and waiting out the rest of its backoff to find that out is exactly the delay
		// this is here to avoid.
		instanceProvider: instance.NewProvider(
			ox,
			project,
			func(ctx context.Context, released instance.Released) {
				instanceTypeProvider.RecordCapacityReleased(ctx, instancetype.Specs{
					Ncpus: released.Ncpus,
					// Truncating is the conservative direction: it can only understate what came
					// back, which reopens fewer shapes rather than more.
					MemInGiB: int(released.Memory / GiB),
				})
			},
		),
		instanceTypeProvider: instanceTypeProvider,
		bootstrappers:        bootstrappers,
		repairPolicies:       repairPolicies,
	}
}

// Name returns the CloudProvider implementation name.
func (c *OxideCloudProvider) Name() string {
	return "oxide"
}

// Create launches a NodeClaim with the given resource requests and requirements and returns a hydrated
// NodeClaim back with resolved NodeClaim labels for the launched NodeClaim
func (c *OxideCloudProvider) Create(
	ctx context.Context,
	nodeClaim *karpcorev1.NodeClaim,
) (*karpcorev1.NodeClaim, error) {

	// Get the Oxide Node Class from the NodeClaim
	nodeClass := &v1alpha1.OxideNodeClass{}
	if err := c.kubeClient.Get(
		ctx,
		client.ObjectKey{Name: nodeClaim.Spec.NodeClassRef.Name},
		nodeClass,
	); err != nil {
		return nil, fmt.Errorf("unable to retrieve the OxideNodeClass, %w", err)
	}

	// Taints are per-NodeClaim rather than per-nodeclass, so they are resolved here and handed to the
	// bootstrapper to apply in whatever form it registers nodes with.
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

	// Shapes that cannot be launched are dropped here as well as from the offerings the scheduler
	// sees. The NodeClaim carries the requirements it was built with, so one that was already in
	// flight when a shape became unavailable can still ask for it.
	//
	// The reason is kept so the error below can say which limit was hit. Quota wins when both apply,
	// because it is the one an operator can do something about.
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

	// Smallest first, so the launch takes the least of the rack that satisfies the NodeClaim.
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

	// A shape whose backoff has just elapsed is offered again, but only one launch is allowed to test
	// it. Losing that race is not a capacity failure, so this walks on to the next shape rather than
	// turning a local interlock into a refusal the rack never issued.
	instanceType, held := instancetype.Specs{}, false
	for _, specs := range ordered {
		if c.instanceTypeProvider.TryAcquire(specs) {
			instanceType, held = specs, true
			break
		}
	}
	if !held {
		// Retryable on purpose. The probe ahead of this one settles the question within a launch or
		// two, and whichever way it goes the next attempt has a better answer than this one could.
		return nil, fmt.Errorf(
			"all %d instance types the NodeClaim can use are being probed by another launch",
			len(ordered),
		)
	}
	// Nothing below reports an outcome for a launch abandoned before the rack sees it, so the
	// reservation is handed back here. RecordICE and RecordSuccess release it themselves; this is
	// idempotent alongside them.
	defer c.instanceTypeProvider.ReleaseProbe(instanceType)
	// Checked before anything is created, and before a bootstrapper mints a join credential the
	// launch would waste. This is the only place the boot disk is known, so it is the only place the
	// storage quota can be checked at all: disk size comes from the OxideNodeClass, not the shape, so
	// it cannot be folded into an instance type's offerings the way CPU and memory are.
	request := utilization.Request{
		CPUs:    int64(instanceType.Ncpus),
		Memory:  uint64(instanceType.MemInGiB) * GiB,
		Storage: uint64(nodeClass.Spec.BootDiskSize.Value()),
	}
	if quota, err := c.instanceTypeProvider.Quota(); err == nil {
		if admits, resource := quota.Admits(request); !admits {
			// Only the shape's own dimensions are worth marking. A storage quota refuses every shape
			// equally, so taking this one and everything larger out of consideration would say something
			// untrue about the shape and would not help the next attempt find a smaller one that fits.
			if resource != utilization.ResourceStorage {
				c.instanceTypeProvider.RecordICE(ctx, instanceType, instancetype.ReasonQuota)
			}
			return nil, cloudprovider.NewInsufficientCapacityError(
				fmt.Errorf("%s", quota.Explain(request, resource)),
			)
		}
	}

	name := InstanceNamePrefix + nodeClaim.Name
	subnet := nodeClass.Status.Subnets[rand.IntN(len(nodeClass.Status.Subnets))]

	// Generated last, immediately before the instance is created. Some bootstrappers mint a
	// short-lived join credential here, so this must not run for a launch that is going to be
	// abandoned for an unrelated reason.
	userData, err := bootstrapper.UserData(ctx, nodeClass, bootstrap.Request{
		NodeClaim:  nodeClaim,
		NodeTaints: nodeTaints,
		Hostname:   name,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to generate %s user-data: %w", bootstrapper.Name(), err)
	}

	// Create resolves the whole launch: it adopts or clears an instance left behind by an earlier
	// attempt, and does not return until the instance is actually running.
	launched, err := c.instanceProvider.Create(ctx, oxide.InstanceCreateParams{
		Project: oxide.NameOrId(c.project),
		Body: &oxide.InstanceCreate{
			Name:        oxide.Name(name),
			Hostname:    oxide.Hostname(name),
			Description: "Karpenter provisioned node by NodePool",
			Memory:      oxide.ByteCount(instanceType.MemInGiB * GiB),
			Ncpus:       oxide.InstanceCpuCount(instanceType.Ncpus),
			UserData:    base64.StdEncoding.EncodeToString([]byte(userData)),
			BootDisk: oxide.InstanceDiskAttachment{
				Value: &oxide.InstanceDiskAttachmentCreate{
					Name:        oxide.Name(name),
					Description: "Boot disk",
					Size:        oxide.ByteCount(nodeClass.Spec.BootDiskSize.Value()),
					DiskBackend: oxide.DiskBackend{
						Value: &oxide.DiskBackendDistributed{
							DiskSource: oxide.DiskSource{
								Value: &oxide.DiskSourceImage{
									// Images is empty when no selector matched. Launch with an empty
									// image and let the Oxide API reject it, which is what happened
									// when an unmatched selector still left a zero-valued entry here.
									ImageId: lo.FirstOrEmpty(nodeClass.Status.Images).ID,
								},
							},
						},
					},
				},
			},
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
			ExternalIps: lo.Map(
				nodeClass.Status.IPPools,
				func(ipPool v1alpha1.OxideIPPool, _ int) oxide.ExternalIpCreate {
					return oxide.ExternalIpCreate{
						Value: &oxide.ExternalIpCreateEphemeral{
							PoolSelector: oxide.PoolSelector{
								Value: &oxide.PoolSelectorExplicit{
									Pool: oxide.NameOrId(ipPool.Name),
								},
							},
						},
					}
				},
			),
			SshPublicKeys: lo.Map(
				nodeClass.Status.SSHKeys,
				func(sshKey v1alpha1.OxideSSHKey, _ int) oxide.NameOrId {
					return oxide.NameOrId(sshKey.Name)
				},
			),
		},
	})
	if err != nil {
		// Karpenter deletes the NodeClaim immediately on an insufficient capacity error and
		// reschedules its pods. Without this the NodeClaim sits around holding a launch that the
		// rack cannot satisfy until the 15 minute registration timeout expires.
		if errors.Is(err, instance.ErrInsufficientCapacity) {
			// The API reports a quota that has no room and a rack that has no room identically, so the
			// two are told apart here by asking whether the quota would have allowed this launch. The
			// quota was checked above, but a concurrent launch or a lowered quota can have consumed the
			// headroom since, which is exactly the case worth naming rather than blaming the rack for.
			reason, diagnosis := c.diagnoseRefusal(request, err)
			// Rescheduling only helps if it lands somewhere else, so this shape and everything
			// larger comes out of consideration while the limit holds.
			c.instanceTypeProvider.RecordICE(ctx, instanceType, reason)
			if reason == instancetype.ReasonQuota {
				// Direct evidence that the snapshot overstated the headroom. Re-reading now means the rest
				// of an in-flight burst is judged against the truth instead of failing the same way.
				c.instanceTypeProvider.RefreshQuota(ctx)
			}
			return nil, cloudprovider.NewInsufficientCapacityError(diagnosis)
		}
		return nil, err
	}

	// The rack placed this shape, which disproves every recorded claim that it could not be placed,
	// including the ones recorded against smaller shapes. Closing the breaker here is what stops a
	// shape that recovered from carrying its refusal history into the next incident and backing off
	// from the second refusal as though it were the fifth.
	c.instanceTypeProvider.RecordSuccess(ctx, instanceType)

	resolvedNodeClaim, err := c.InstanceToNodeClaim(launched, nodeClass, nodeClaim.DeepCopy())
	if err != nil {
		return nil, err
	}

	return resolvedNodeClaim, nil
}

// Delete removes a NodeClaim from the cloudprovider by its provider id. Delete should return
// NodeClaimNotFoundError if the cloudProvider instance is already terminated and nil if deletion was triggered.
// Karpenter will keep retrying until Delete returns a NodeClaimNotFound error.
func (c *OxideCloudProvider) Delete(ctx context.Context, nodeClaim *karpcorev1.NodeClaim) error {
	instanceID, err := InstanceIDFromProviderID(nodeClaim.Status.ProviderID)
	if err != nil {
		return fmt.Errorf("invalid provider id, must use the oxide ccm: %w", err)
	}

	// Matches the instance and boot disk names assigned in Create. It is the fallback used to find
	// the boot disk when the instance is already gone and can no longer be asked for its disk ID.
	bootDiskName := InstanceNamePrefix + nodeClaim.Name

	if err := c.instanceProvider.Delete(ctx, instanceID, bootDiskName); err != nil {
		if errors.Is(err, instance.ErrNotFound) {
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return err
	}

	// The instance and its disks are gone by the time Delete returns without an error, so report the
	// terminal state directly rather than making Karpenter retry to discover it.
	return cloudprovider.NewNodeClaimNotFoundError(instance.ErrNotFound)
}

// Get retrieves a NodeClaim from the cloudprovider by its provider id
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
		// Karpenter's termination flow only removes finalizers once the cloudprovider reports the
		// instance as gone, so a 404 must surface as a NodeClaimNotFoundError rather than a generic
		// error that gets retried forever.
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

// List retrieves all NodeClaims from the cloudprovider
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
// Availability of types or zone may vary by nodepool or over time.  Regardless of
// availability, the GetInstanceTypes method should always return all instance types,
// even those with no offerings available.
func (c *OxideCloudProvider) GetInstanceTypes(
	context.Context,
	*karpcorev1.NodePool,
) ([]*cloudprovider.InstanceType, error) {
	return c.instanceTypeProvider.InstanceTypes(), nil
}

// IsDrifted returns whether a NodeClaim has drifted from the provisioning requirements
// it is tied to.
func (c *OxideCloudProvider) IsDrifted(
	context.Context,
	*karpcorev1.NodeClaim,
) (cloudprovider.DriftReason, error) {
	return "", nil
}

// noCandidatesError explains that every instance type the NodeClaim could have used is currently
// unavailable, in terms of whichever limit ruled them out.
//
// This is the message that surfaces on the NodeClaim and in the events an operator reads when nothing
// is scaling up, so it is worth the effort of being specific: "the silo's cpus quota has no room"
// points at an administrator, whereas "the rack has no room" points at waiting or at smaller pods.
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
	// Reported against the smallest candidate, since that is the one whose failure to fit actually
	// establishes that none of them do.
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

// diagnoseRefusal decides whether a refused launch was the silo's quota or the rack's capacity, and
// returns the reason to record along with the error to report.
//
// The API does not distinguish them, so this asks the question the API answers implicitly: if the
// quota had no room for this request, the quota is the binding constraint and the rack never got a
// say. An unknown quota falls back to capacity, which is both the more common cause and the
// conservative guess, since the breaker retries it on its own.
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

// smallestSpecs returns the smallest shape among instance type names, by CPU and then memory.
// Unparseable names are skipped rather than failing the caller, which is only assembling an error
// message.
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

// RepairPolicy is for CloudProviders to define a set Unhealthy condition for Karpenter
// to monitor on the node.
//
// This is empty unless sled inventory is enabled, because nothing else sets the condition it would
// name: there is no way to observe that a node's hardware is being retired without the fleet-scoped
// system API. Returning it empty is also what keeps core from registering its health controller at
// all, so an installation without inventory has no node repair machinery running for this provider.
func (c *OxideCloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return c.repairPolicies
}

// GetSupportedNodeClasses returns CloudProvider NodeClass that implements status.Object
// NOTE: It returns a list where the first element should be the default NodeClass
func (c *OxideCloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.OxideNodeClass{}}
}
