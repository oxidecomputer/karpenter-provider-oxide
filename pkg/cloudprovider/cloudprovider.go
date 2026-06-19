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
	"github.com/oxidecomputer/karpenter/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter/pkg/instancetype"
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

	instanceTypeProvider *instancetype.Provider
	bootstrappers        bootstrap.Bootstrappers
}

func NewOxideCloudProvider(kubeClient client.Client, ox *oxide.Client, project string, instanceTypeProvider *instancetype.Provider, bootstrappers bootstrap.Bootstrappers) *OxideCloudProvider {
	return &OxideCloudProvider{
		kubeClient:           kubeClient,
		ox:                   ox,
		project:              project,
		instanceTypeProvider: instanceTypeProvider,
		bootstrappers:        bootstrappers,
	}
}

// Name returns the CloudProvider implementation name.
func (c *OxideCloudProvider) Name() string {
	return "oxide"
}

// Create launches a NodeClaim with the given resource requests and requirements and returns a hydrated
// NodeClaim back with resolved NodeClaim labels for the launched NodeClaim
func (c *OxideCloudProvider) Create(ctx context.Context, nodeClaim *karpcorev1.NodeClaim) (*karpcorev1.NodeClaim, error) {

	// Get the Oxide Node Class from the NodeClaim
	nodeClass := &v1alpha1.OxideNodeClass{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: nodeClaim.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		return nil, fmt.Errorf("unable to retrieve the OxideNodeClass, %w", err)
	}

	// Taints are per-NodeClaim rather than per-nodeclass, so they are resolved here and handed to the
	// bootstrapper to apply in whatever form it registers nodes with.
	nodeTaints := append(append(nodeClaim.Spec.Taints, nodeClaim.Spec.StartupTaints...), corev1.Taint{
		Key:    karpcorev1.UnregisteredTaintKey,
		Effect: corev1.TaintEffectNoExecute,
	})
	nodeTaints = lo.UniqBy(nodeTaints, func(t corev1.Taint) string { return t.Key })

	bootstrapper, err := c.bootstrappers.For(nodeClass)
	if err != nil {
		return nil, err
	}

	var minInstanceType string

	for _, req := range nodeClaim.Spec.Requirements {
		if req.Key != corev1.LabelInstanceTypeStable {
			continue
		}
		minInstanceType = slices.MinFunc(req.Values, func(a, b string) int {
			aSpecs, err := instancetype.InstanceTypeToSpecs(a)
			if err != nil {
				return 0
			}
			bSpecs, err := instancetype.InstanceTypeToSpecs(b)
			if err != nil {
				return 0
			}
			cpuDelta := aSpecs.Ncpus - bSpecs.Ncpus
			if cpuDelta == 0 {
				return aSpecs.MemInGiB - bSpecs.MemInGiB
			}
			return cpuDelta
		})
	}

	if minInstanceType == "" {
		return nil, fmt.Errorf("no instance types match NodeClaim requirements")
	}

	instanceType, err := instancetype.InstanceTypeToSpecs(minInstanceType)
	if err != nil {
		return nil, fmt.Errorf("invalid instance type: %w", err)
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

	instance, err := c.ox.InstanceCreate(ctx, oxide.InstanceCreateParams{
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
									ImageId: nodeClass.Status.Images[0].ID,
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
			ExternalIps: lo.Map(nodeClass.Status.IPPools, func(ipPool v1alpha1.OxideIPPool, _ int) oxide.ExternalIpCreate {
				return oxide.ExternalIpCreate{
					Value: &oxide.ExternalIpCreateEphemeral{
						PoolSelector: oxide.PoolSelector{
							Value: &oxide.PoolSelectorExplicit{
								Pool: oxide.NameOrId(ipPool.Name),
							},
						},
					},
				}
			}),
			SshPublicKeys: lo.Map(nodeClass.Status.SSHKeys, func(sshKey v1alpha1.OxideSSHKey, _ int) oxide.NameOrId {
				return oxide.NameOrId(sshKey.Name)
			}),
		},
	})

	if err != nil {
		// TODO: we should check if it's stopped and then start it if necessary, otherwise do nothing
		if errors.Is(err, oxide.ErrObjectAlreadyExists) {
			var startErr error
			_, startErr = c.ox.InstanceStart(ctx, oxide.InstanceStartParams{
				Project:  oxide.NameOrId(c.project),
				Instance: oxide.NameOrId(name),
			})
			return nil, fmt.Errorf("unable to create instance, %w, unable to start instance, %w", err, startErr)
		}
		return nil, err
	}

	resolvedNodeClaim, err := c.InstanceToNodeClaim(instance, nodeClass, nodeClaim.DeepCopy())
	if err != nil {
		return nil, err
	}

	return resolvedNodeClaim, nil
}

// Delete removes a NodeClaim from the cloudprovider by its provider id. Delete should return
// NodeClaimNotFoundError if the cloudProvider instance is already terminated and nil if deletion was triggered.
// Karpenter will keep retrying until Delete returns a NodeClaimNotFound error.
func (c *OxideCloudProvider) Delete(ctx context.Context, nodeClaim *karpcorev1.NodeClaim) error {
	if !strings.HasPrefix(nodeClaim.Status.ProviderID, "oxide://") {
		return fmt.Errorf("invalid provider id, must use the oxide ccm")
	}
	instanceID := strings.TrimPrefix(nodeClaim.Status.ProviderID, "oxide://")
	_, err := c.ox.InstanceStop(ctx, oxide.InstanceStopParams{
		Instance: oxide.NameOrId(instanceID),
	})
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return fmt.Errorf("unable to stop the instance, %w", err)
	}

	err = c.ox.InstanceDelete(ctx, oxide.InstanceDeleteParams{
		Instance: oxide.NameOrId(instanceID),
	})
	if err != nil {
		return fmt.Errorf("unable to delete the instance, %w", err)
	}

	err = c.ox.DiskDelete(ctx, oxide.DiskDeleteParams{
		Project: oxide.NameOrId(c.project),
		Disk:    oxide.NameOrId(nodeClaim.Name),
	})
	if err != nil {
		return fmt.Errorf("unable to delete boot disk, %w", err)
	}
	return nil
}

// Get retrieves a NodeClaim from the cloudprovider by its provider id
func (c *OxideCloudProvider) Get(ctx context.Context, providerID string) (*karpcorev1.NodeClaim, error) {
	instanceID, err := InstanceIDFromProviderID(providerID)
	if err != nil {
		return nil, err
	}
	instance, err := c.ox.InstanceView(ctx, oxide.InstanceViewParams{Instance: oxide.NameOrId(instanceID)})
	if err != nil {
		return nil, fmt.Errorf("unable to get instance with instance ID %s, %w", instanceID, err)
	}
	nodeClaim, err := c.InstanceToNodeClaim(instance, nil, nil)
	if err != nil {
		return nil, err
	}
	return nodeClaim, nil
}

func (c *OxideCloudProvider) InstanceToNodeClaim(instance *oxide.Instance, nodeClass *v1alpha1.OxideNodeClass, nodeClaim *karpcorev1.NodeClaim) (*karpcorev1.NodeClaim, error) {
	if nodeClaim == nil {
		nodeClaim = &karpcorev1.NodeClaim{
			ObjectMeta: v1.ObjectMeta{
				Name: strings.TrimPrefix(instance.Hostname, InstanceNamePrefix),
			},
		}
	}
	nodeClaim.Status.ProviderID = fmt.Sprintf("oxide://%s", instance.Id)
	labels := map[string]string{}
	if it, ok := c.instanceTypeProvider.Get(instancetype.SpecsToInstanceTypeName(int(instance.Ncpus), int(instance.Memory)/GiB)); ok {
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
		nodeClaim.Status.ImageID = nodeClass.Status.Images[0].ID
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
	instances, err := c.ox.InstanceListAllPages(ctx, oxide.InstanceListParams{Project: oxide.NameOrId(c.project)})
	if err != nil {
		return nil, err
	}
	var nodeClaims []*karpcorev1.NodeClaim
	for _, instance := range instances {
		if !strings.HasPrefix(string(instance.Name), InstanceNamePrefix) {
			continue
		}
		nodeClaim, err := c.InstanceToNodeClaim(&instance, nil, nil)
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
func (c *OxideCloudProvider) GetInstanceTypes(context.Context, *karpcorev1.NodePool) ([]*cloudprovider.InstanceType, error) {
	return c.instanceTypeProvider.InstanceTypes(), nil
}

// IsDrifted returns whether a NodeClaim has drifted from the provisioning requirements
// it is tied to.
func (c *OxideCloudProvider) IsDrifted(context.Context, *karpcorev1.NodeClaim) (cloudprovider.DriftReason, error) {
	return "", nil
}

// RepairPolicy is for CloudProviders to define a set Unhealthy condition for Karpenter
// to monitor on the node.
func (c *OxideCloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return nil
}

// GetSupportedNodeClasses returns CloudProvider NodeClass that implements status.Object
// NOTE: It returns a list where the first element should be the default NodeClass
func (c *OxideCloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.OxideNodeClass{}}
}
