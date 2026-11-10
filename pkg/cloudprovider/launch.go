package cloudprovider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type launchPlan struct {
	candidate    launchCandidate
	storage      storage.Plan
	subnet       v1alpha1.OxideSubnet
	owner        ownership.Marker
	nodeClass    *v1alpha1.OxideNodeClass
	bootstrapper bootstrap.Bootstrapper
	bootstrap    bootstrap.Request
	instanceType *cloudprovider.InstanceType
}

func (c *OxideCloudProvider) planLaunch(ctx context.Context, claim *karpcorev1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass, candidate launchCandidate) (*launchPlan, error) {
	catalog, err := instancetype.OptionsFor(nodeClass)
	if err != nil {
		return nil, err
	}
	effective, err := c.effectiveInstanceType(
		ctx,
		claim,
		instancetype.SpecsToInstanceTypeName(
			candidate.specs.Ncpus,
			candidate.specs.MemInGiB,
		),
		catalog,
	)
	if err != nil {
		return nil, err
	}
	name := InstanceNamePrefix + claim.Name
	disks, err := storage.Build(name, uuid.NewString(), nodeClass)
	if err != nil {
		return nil, fmt.Errorf("unable to plan the disks for %s: %w", claim.Name, err)
	}
	subnet, err := selectSubnet(nodeClass.Status.Subnets)
	if err != nil {
		return nil, err
	}
	bootstrapper, err := c.bootstrappers.For(ctx, nodeClass)
	if err != nil {
		return nil, err
	}
	taints := append([]corev1.Taint{}, claim.Spec.Taints...)
	taints = append(taints, claim.Spec.StartupTaints...)
	taints = append(
		taints,
		corev1.Taint{Key: karpcorev1.UnregisteredTaintKey, Effect: corev1.TaintEffectNoExecute},
	)
	p := &launchPlan{candidate: candidate, storage: disks, subnet: subnet, owner: c.owner(claim),
		nodeClass: nodeClass, bootstrapper: bootstrapper, instanceType: effective}
	p.bootstrap = bootstrap.Request{
		NodeClaim:    claim,
		NodeTaints:   lo.UniqBy(taints, func(t corev1.Taint) string { return t.Key }),
		Hostname:     name,
		Storage:      &p.storage,
		InstanceType: candidate.specs,
		Kubelet:      catalog.Kubelet.Reserved,
	}
	return p, nil
}

func (c *OxideCloudProvider) executeLaunch(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	plan *launchPlan,
	boot launchBootDisk,
	probe *availability.Probe,
) (*instance.LaunchResult, error) {
	request := availability.Request{
		Shape:    plan.candidate.specs,
		Platform: plan.candidate.platform,
		Placement: placementFor(
			nodeClass.Status.AffinityGroups,
		),
		Storage: uint64(boot.storageBytes),
	}
	decision := c.instanceTypeProvider.Availability().Evaluate(request)
	if !decision.Available {
		if decision.Source == availability.SourceQuota &&
			decision.Resource != utilization.ResourceStorage {
			probe.RecordRefusal(
				ctx,
				availability.Refusal{Scope: decision.Scope, Reason: decision.Reason},
			)
		}
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("%s", decision.Message))
	}
	userData, err := plan.bootstrapper.UserData(ctx, nodeClass, plan.bootstrap)
	if err != nil {
		return nil, fmt.Errorf("unable to generate %s user-data: %w", plan.bootstrapper.Name(), err)
	}
	params, options := plan.instanceRequest(c.project, boot, userData)
	result, err := c.instanceProvider.Create(ctx, params, options)
	if err != nil {
		if result != nil && result.Adopted && errors.Is(err, instance.ErrInsufficientCapacity) {
			return nil, cloudprovider.NewInsufficientCapacityError(err)
		}
		return nil, c.diagnoseLaunchFailure(ctx, err, nodeClass, utilization.Request{
			CPUs: int64(
				request.Shape.Ncpus,
			),
			Memory:  uint64(request.Shape.MemInGiB) * GiB,
			Storage: request.Storage,
		}, probe)
	}
	return result, nil
}

func (p *launchPlan) instanceRequest(
	project string,
	boot launchBootDisk,
	userData string,
) (oxide.InstanceCreateParams, instance.Options) {
	name := p.storage.InstanceName
	describe := func(kind ownership.Kind, human string) string {
		marker := p.owner
		marker.Kind = kind
		return marker.Description(human)
	}
	externalIPSets := externalIPSets(p.nodeClass.Status.EphemeralIPs)
	return oxide.InstanceCreateParams{
		Project: oxide.NameOrId(project),
		Body: &oxide.InstanceCreate{
			Name:        oxide.Name(name),
			Hostname:    oxide.Hostname(name),
			Description: describe(ownership.KindInstance, "Karpenter provisioned node by NodePool"),
			Memory:      oxide.ByteCount(p.candidate.specs.MemInGiB * GiB),
			Ncpus:       oxide.InstanceCpuCount(p.candidate.specs.Ncpus),
			UserData:    base64.StdEncoding.EncodeToString([]byte(userData)),

			EnableJumboFrames: p.nodeClass.Spec.JumboFrames,
			CpuPlatform:       oxide.InstanceCpuPlatform(p.candidate.platform),

			AntiAffinityGroups: antiAffinityGroups(p.nodeClass.Status.AffinityGroups),

			BootDisk: boot.attachment,
			Disks: p.storage.DataAttachments(func(disk storage.Disk) string {
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
							SubnetName:  oxide.Name(p.subnet.Name),
							VpcName:     oxide.Name(p.subnet.VPCName),
						},
					},
				},
			},
			ExternalIps: externalIPSets[0],
			SshPublicKeys: lo.Map(
				p.nodeClass.Status.SSHKeys,
				func(sshKey v1alpha1.OxideSSHKey, _ int) oxide.NameOrId {
					return oxide.NameOrId(sshKey.Name)
				},
			),
		},
	}, instance.Options{
		Owner:               p.owner,
		AffinityGroups:      affinityGroups(p.nodeClass.Status.AffinityGroups),
		ExternalIPFallbacks: externalIPSets[1:],
	}
}
