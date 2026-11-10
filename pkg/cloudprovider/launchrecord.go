package cloudprovider

import (
	"context"
	"fmt"
	"maps"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

type launchRecord struct {
	annotations map[string]string
	imageID     string
	bootSize    int64
}

func (c *OxideCloudProvider) recordLaunch(ctx context.Context, claim *karpv1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass, plan *launchPlan, boot launchBootDisk,
	result *instance.LaunchResult) (*karpv1.NodeClaim, error) {
	var record launchRecord
	var err error
	if result.Adopted {
		record, err = c.recoverLaunch(ctx, claim, result.Instance)
	} else {
		record, err = c.plannedLaunchRecord(ctx, plan, boot)
	}
	if err != nil {
		return nil, err
	}
	resolvedClass := nodeClass.DeepCopy()
	if record.bootSize > 0 {
		resolvedClass.Spec.Storage.Boot.Size = *resource.NewQuantity(record.bootSize, resource.BinarySI)
		resolvedClass.Status.Storage = &v1alpha1.StorageStatus{BootSizeBytes: record.bootSize}
	}
	recordedClaim := claim.DeepCopy()
	if result.Adopted {
		recordedClaim.Annotations = record.annotations
		delete(recordedClaim.Annotations, v1alpha1.AnnotationDiskPrefix)
		delete(recordedClaim.Labels, corev1.LabelInstanceTypeStable)
		delete(recordedClaim.Labels, v1alpha1.LabelInstanceCPUPlatform)
	} else {
		recordedClaim.Annotations = lo.Assign(recordedClaim.Annotations, record.annotations)
		delete(recordedClaim.Annotations, v1alpha1.AnnotationDiskIDs)
	}
	resolved, err := c.InstanceToNodeClaim(result.Instance, resolvedClass, recordedClaim)
	if err != nil {
		return nil, err
	}
	resolved.Status.ImageID = record.imageID
	effective := plan.instanceType
	if result.Adopted {
		catalog, optionsErr := instancetype.OptionsFor(resolvedClass)
		if optionsErr != nil {
			return nil, optionsErr
		}
		effective, err = c.effectiveInstanceType(
			ctx,
			claim,
			instancetype.SpecsToInstanceTypeName(
				int(result.Instance.Ncpus),
				int(result.Instance.Memory)/GiB,
			),
			catalog,
		)
		if err != nil {
			return nil, err
		}
	}
	annotateHourlyPrice(resolved, effective)
	return resolved, nil
}

func (c *OxideCloudProvider) plannedLaunchRecord(ctx context.Context, plan *launchPlan,
	boot launchBootDisk) (launchRecord, error) {
	nodeClass := plan.nodeClass
	ips, err := c.ephemeralIPRecords(ctx, nodeClass, plan.storage.InstanceName)
	if err != nil {
		return launchRecord{}, err
	}
	annotations := map[string]string{
		v1alpha1.AnnotationOxideNodeClassHash:        nodeClass.Hash(),
		v1alpha1.AnnotationOxideNodeClassHashVersion: v1alpha1.OxideNodeClassHashVersion,
		v1alpha1.AnnotationSubnet:                    plan.subnet.ID,
		v1alpha1.AnnotationDiskPrefix:                plan.storage.Prefix,
		v1alpha1.AnnotationBootDisk:                  boot.name,
		v1alpha1.AnnotationSSHKeys: v1alpha1.EncodeIDs(
			v1alpha1.SSHKeyIDs(nodeClass.Status.SSHKeys),
		),
		v1alpha1.AnnotationEphemeralIPs: v1alpha1.EncodeEphemeralIPs(ips),
		v1alpha1.AnnotationDiskSetSources: v1alpha1.EncodeDiskSetSources(
			v1alpha1.DiskSetSources(nodeClass.Status.Storage),
		),
	}
	if len(nodeClass.Status.AffinityGroups) > 0 {
		annotations[v1alpha1.AnnotationAffinityGroups] = v1alpha1.EncodeAffinityGroups(
			v1alpha1.AffinityGroupRefsFor(nodeClass.Status.AffinityGroups))
	}
	return launchRecord{annotations: annotations, imageID: plan.storage.Boot.ImageID,
		bootSize: plan.storage.Boot.SizeBytes}, nil
}

func (c *OxideCloudProvider) recoverLaunch(ctx context.Context, claim *karpv1.NodeClaim,
	inst *oxide.Instance) (launchRecord, error) {
	record := launchRecord{
		annotations: maps.Clone(claim.Annotations),
		imageID:     claim.Status.ImageID,
	}
	if record.annotations == nil {
		record.annotations = map[string]string{}
	}
	disks, err := c.ox.InstanceDiskListAllPages(
		ctx,
		oxide.InstanceDiskListParams{Instance: oxide.NameOrId(inst.Id)},
	)
	if err != nil {
		return launchRecord{}, fmt.Errorf("reading adopted instance disks: %w", err)
	}
	var ownedIDs []string
	for _, disk := range disks {
		if disk.Id == inst.BootDiskId {
			record.imageID, record.bootSize = disk.ImageId, int64(disk.Size)
			record.annotations[v1alpha1.AnnotationBootDisk] = string(disk.Name)
		}
		marker, ok := ownership.Parse(disk.Description)
		if !ok || (marker.Kind != ownership.KindBootDisk && marker.Kind != ownership.KindDataDisk) {
			continue
		}
		expected := c.owner(claim)
		expected.Kind = marker.Kind
		if ownership.Verify(disk.Description, expected) == nil {
			ownedIDs = append(ownedIDs, disk.Id)
		}
	}
	record.annotations[v1alpha1.AnnotationDiskIDs] = v1alpha1.EncodeIDs(ownedIDs)
	if inst.BootDiskId != "" && record.bootSize == 0 {
		return launchRecord{}, fmt.Errorf(
			"adopted instance boot disk %s was not listed",
			inst.BootDiskId,
		)
	}
	interfaces, err := c.ox.InstanceNetworkInterfaceListAllPages(ctx,
		oxide.InstanceNetworkInterfaceListParams{Instance: oxide.NameOrId(inst.Id)})
	if err != nil {
		return launchRecord{}, fmt.Errorf("reading adopted instance interfaces: %w", err)
	}
	delete(record.annotations, v1alpha1.AnnotationSubnet)
	for _, nic := range interfaces {
		if len(interfaces) == 1 || (nic.Primary != nil && *nic.Primary) {
			record.annotations[v1alpha1.AnnotationSubnet] = nic.SubnetId
			break
		}
	}
	keys, err := c.ox.InstanceSshPublicKeyListAllPages(ctx,
		oxide.InstanceSshPublicKeyListParams{Instance: oxide.NameOrId(inst.Id)})
	if err != nil {
		return launchRecord{}, fmt.Errorf("reading adopted instance SSH keys: %w", err)
	}
	record.annotations[v1alpha1.AnnotationSSHKeys] = v1alpha1.EncodeIDs(
		lo.Map(keys, func(key oxide.SshKey, _ int) string { return key.Id }))
	ips, err := c.readEphemeralIPRecords(ctx, inst.Id)
	if err != nil {
		return launchRecord{}, err
	}
	record.annotations[v1alpha1.AnnotationEphemeralIPs] = v1alpha1.EncodeEphemeralIPs(ips)
	groups, err := c.readAffinityGroupRecords(ctx, inst.Id)
	if err != nil {
		return launchRecord{}, err
	}
	record.annotations[v1alpha1.AnnotationAffinityGroups] = v1alpha1.EncodeAffinityGroups(groups)
	return record, nil
}

func (c *OxideCloudProvider) readAffinityGroupRecords(
	ctx context.Context,
	instanceID string,
) ([]v1alpha1.AffinityGroupRef, error) {
	affinity, err := c.ox.ExperimentalInstanceAffinityGroupListAllPages(ctx,
		oxide.InstanceAffinityGroupListParams{Instance: oxide.NameOrId(instanceID)})
	if err != nil {
		return nil, fmt.Errorf("reading adopted instance affinity groups: %w", err)
	}
	antiAffinity, err := c.ox.InstanceAntiAffinityGroupListAllPages(ctx,
		oxide.InstanceAntiAffinityGroupListParams{Instance: oxide.NameOrId(instanceID)})
	if err != nil {
		return nil, fmt.Errorf("reading adopted instance anti-affinity groups: %w", err)
	}
	var groups []v1alpha1.AffinityGroupRef
	for _, group := range affinity {
		groups = append(
			groups,
			v1alpha1.AffinityGroupRef{Type: v1alpha1.AffinityGroupTypeAffinity, ID: group.Id},
		)
	}
	for _, group := range antiAffinity {
		groups = append(
			groups,
			v1alpha1.AffinityGroupRef{Type: v1alpha1.AffinityGroupTypeAntiAffinity, ID: group.Id},
		)
	}
	return groups, nil
}
