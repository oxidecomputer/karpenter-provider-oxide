package v1alpha1

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OxideNodeClassSpec is the top level specification for the Oxide Karpenter Provider.
type OxideNodeClassSpec struct {
	// UserData is a bash fragment that runs before the provider-generated node bootstrap steps.
	// +optional
	UserData string `json:"userData,omitempty"`
	// Env contains environment variables exported before userData and the provider-generated
	// node bootstrap steps. Values may be specified directly or read from a Secret.
	// +optional
	Env []EnvVar `json:"env,omitempty"`
	// Server is the endpoint nodes use to join the cluster.
	// +optional
	Server string `json:"server,omitempty"`
	// Kubelet configures the kubelet on launched nodes.
	// +optional
	Kubelet *KubeletConfiguration `json:"kubelet,omitempty"`
	// Bootstrap selects and configures the mechanism nodes use to join the cluster.
	// Leave it unset to have the provider detected from the cluster it runs in.
	// +optional
	Bootstrap *Bootstrap `json:"bootstrap,omitempty"`
	// ImageSelector is used to select an image to launch nodes with.
	ImageSelector []ImageSelector `json:"imageSelector,omitempty" hash:"ignore"`
	// Storage is the disks every instance launched from this nodeclass is created with.
	// +kubebuilder:validation:Required
	Storage Storage `json:"storage" hash:"ignore"`
	// SubnetSelector is used to select subnets to launch nodes into.
	// +kubebuilder:validation:MinItems=1
	SubnetSelector []SubnetSelector `json:"subnetSelector" hash:"ignore"`
	// SSHKeySelector selects SSH keys to copy to the node.
	// +optional
	SSHKeySelector []SSHKeySelector `json:"sshKeySelector,omitempty" hash:"ignore"`
	// EphemeralIPs are the external addresses attached to every launched instance.
	// +kubebuilder:validation:MaxItems=2
	// +optional
	EphemeralIPs []EphemeralIP `json:"ephemeralIPs,omitempty" hash:"ignore"`
	// Affinity controls which Oxide affinity and anti-affinity groups launched instances join.
	// +optional
	Affinity *Affinity `json:"affinity,omitempty"`
	// JumboFrames opts launched instances in to jumbo frames (8500 byte MTU) on their primary
	// network interface. Requires the fleet-wide jumbo-frames opt-in to be enabled by an operator.
	// +optional
	JumboFrames *bool `json:"jumboFrames,omitempty"`
}

// KubeletConfiguration is the subset of kubelet configuration the provider lets a
// nodeclass set, named after the upstream kubelet types. Fields that change what a
// node can hold (reservations, eviction thresholds, pod limits) are modeled into
// instance type capacity and overhead in lockstep with the flags rendered to
// kubelet, so the scheduler plans against what the node reports. There is no
// passthrough for other flags; add a typed field instead.
// +kubebuilder:validation:XValidation:rule="!has(self.imageGCHighThresholdPercent) || !has(self.imageGCLowThresholdPercent) || self.imageGCHighThresholdPercent > self.imageGCLowThresholdPercent",message="imageGCHighThresholdPercent must be greater than imageGCLowThresholdPercent"
// +kubebuilder:validation:XValidation:rule="!has(self.evictionSoft) || self.evictionSoft.all(e, has(self.evictionSoftGracePeriod) && e in self.evictionSoftGracePeriod)",message="every evictionSoft signal needs a matching evictionSoftGracePeriod, or kubelet refuses to start"
// +kubebuilder:validation:XValidation:rule="!has(self.evictionSoftGracePeriod) || self.evictionSoftGracePeriod.all(e, has(self.evictionSoft) && e in self.evictionSoft)",message="every evictionSoftGracePeriod signal needs a matching evictionSoft"
type KubeletConfiguration struct {
	// ClusterDNS is the list of cluster DNS server addresses.
	// +optional
	ClusterDNS []string `json:"clusterDNS,omitempty"`
	// MaxPods caps the pods a node runs. The provider advertises 110 by default.
	// +kubebuilder:validation:Minimum:=0
	// +optional
	MaxPods *int32 `json:"maxPods,omitempty"`
	// PodsPerCore caps the pods a node runs at this many per vCPU. The lower of
	// it and maxPods wins, as in kubelet.
	// +kubebuilder:validation:Minimum:=0
	// +optional
	PodsPerCore *int32 `json:"podsPerCore,omitempty"`
	// SystemReserved is held back for OS daemons and the kernel. The provider reserves nothing here by default.
	// +kubebuilder:validation:XValidation:rule="self.all(x, x in ['cpu','memory','ephemeral-storage','pid'])",message="valid keys for systemReserved are cpu, memory, ephemeral-storage and pid"
	// +kubebuilder:validation:XValidation:rule="self.all(x, isQuantity(self[x]) && sign(quantity(self[x])) >= 0)",message="systemReserved values must be non-negative resource quantities"
	// +optional
	SystemReserved map[string]string `json:"systemReserved,omitempty"`
	// KubeReserved is held back for Kubernetes system components. Each key set here
	// replaces the provider's own per-shape reservation for that resource.
	// +kubebuilder:validation:XValidation:rule="self.all(x, x in ['cpu','memory','ephemeral-storage','pid'])",message="valid keys for kubeReserved are cpu, memory, ephemeral-storage and pid"
	// +kubebuilder:validation:XValidation:rule="self.all(x, isQuantity(self[x]) && sign(quantity(self[x])) >= 0)",message="kubeReserved values must be non-negative resource quantities"
	// +optional
	KubeReserved map[string]string `json:"kubeReserved,omitempty"`
	// EvictionHard maps eviction signals to the hard thresholds, absolute or a percentage of
	// capacity, at which kubelet evicts. memory.available and nodefs.available replace the
	// provider's own thresholds and are modeled into overhead; the rest only reach kubelet.
	// +kubebuilder:validation:XValidation:rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])",message="valid keys for evictionHard are memory.available, nodefs.available, nodefs.inodesFree, imagefs.available, imagefs.inodesFree and pid.available"
	// +kubebuilder:validation:XValidation:rule="self.all(x, self[x].matches('^[0-9]+([.][0-9]+)?%$') || (isQuantity(self[x]) && sign(quantity(self[x])) >= 0))",message="evictionHard values must be non-negative resource quantities or percentages"
	// +optional
	EvictionHard map[string]string `json:"evictionHard,omitempty"`
	// EvictionSoft maps eviction signals to the thresholds at which kubelet evicts once
	// the signal's grace period has passed. Soft thresholds do not change allocatable.
	// +kubebuilder:validation:XValidation:rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])",message="valid keys for evictionSoft are memory.available, nodefs.available, nodefs.inodesFree, imagefs.available, imagefs.inodesFree and pid.available"
	// +kubebuilder:validation:XValidation:rule="self.all(x, self[x].matches('^[0-9]+([.][0-9]+)?%$') || (isQuantity(self[x]) && sign(quantity(self[x])) >= 0))",message="evictionSoft values must be non-negative resource quantities or percentages"
	// +optional
	EvictionSoft map[string]string `json:"evictionSoft,omitempty"`
	// EvictionSoftGracePeriod maps eviction signals to how long a soft threshold must hold before kubelet evicts.
	// +kubebuilder:validation:XValidation:rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])",message="valid keys for evictionSoftGracePeriod are memory.available, nodefs.available, nodefs.inodesFree, imagefs.available, imagefs.inodesFree and pid.available"
	// +optional
	EvictionSoftGracePeriod map[string]metav1.Duration `json:"evictionSoftGracePeriod,omitempty"`
	// EvictionMaxPodGracePeriod is the longest grace period, in seconds, kubelet
	// gives a pod it evicts for a soft threshold.
	// +kubebuilder:validation:Minimum:=0
	// +optional
	EvictionMaxPodGracePeriod *int32 `json:"evictionMaxPodGracePeriod,omitempty"`
	// ImageGCHighThresholdPercent is the disk usage percentage above which image
	// garbage collection always runs.
	// +kubebuilder:validation:Minimum:=0
	// +kubebuilder:validation:Maximum:=100
	// +optional
	ImageGCHighThresholdPercent *int32 `json:"imageGCHighThresholdPercent,omitempty"`
	// ImageGCLowThresholdPercent is the disk usage percentage below which image
	// garbage collection never runs.
	// +kubebuilder:validation:Minimum:=0
	// +kubebuilder:validation:Maximum:=100
	// +optional
	ImageGCLowThresholdPercent *int32 `json:"imageGCLowThresholdPercent,omitempty"`
	// CPUCFSQuota enables CPU CFS quota enforcement for containers that specify CPU limits.
	// +optional
	CPUCFSQuota *bool `json:"cpuCFSQuota,omitempty"`
}

const (
	maxAttachedDisks = 8

	// These match Nexus MIN_DISK_SIZE_BYTES and MAX_DISK_SIZE_BYTES in
	// omicron/nexus/src/app/mod.rs.
	DiskSizeGranularityBytes    int64 = 1 << 30
	MinDiskSizeBytes            int64 = 1 << 30
	MaxDistributedDiskSizeBytes int64 = 1023 << 30
)

// DiskSizeError reports a disk quantity or source-size contract Nexus would reject.
type DiskSizeError struct {
	Disk       string
	Requested  string
	Constraint string
}

func (e *DiskSizeError) Error() string {
	return fmt.Sprintf("%s size %s is invalid: %s", e.Disk, e.Requested, e.Constraint)
}

// Filesystem values a diskSet's disks can be formatted with.
const (
	FilesystemExt4 = "ext4"
	FilesystemXFS  = "xfs"
	FilesystemNone = "None"
)

// Disk set types, which is the Oxide disk backend the set's disks are created with.
const (
	DiskSetTypeDistributed = "Distributed"
	DiskSetTypeLocal       = "Local"
)

// Disk set layouts, which is how the set's disks are presented to the guest.
const (
	DiskSetLayoutIndividual = "Individual"
)

// Storage is the whole disk layout of a launched instance.
// +kubebuilder:validation:XValidation:rule="!has(self.diskSets) || self.diskSets.map(s, s.count).sum() <= 7",message="an instance can attach at most 8 disks and the boot disk is one of them, so the diskSets' counts must sum to 7 or fewer"
// +kubebuilder:validation:XValidation:rule="!has(self.diskSets) || self.diskSets.all(x, self.diskSets.exists_one(y, y.name == x.name))",message="each diskSet must have a distinct name"
// +kubebuilder:validation:XValidation:rule="!has(self.diskSets) || self.diskSets.all(x, self.diskSets.exists_one(y, (has(y.mountPath) ? y.mountPath : y.mountDir) == (has(x.mountPath) ? x.mountPath : x.mountDir)))",message="each diskSet must have a distinct mountPath or mountDir"
type Storage struct {
	// Boot is the disk the instance boots from.
	// +kubebuilder:validation:Required
	Boot BootDisk `json:"boot"`

	// DiskSets are additional disks attached to every instance, in sets of identically configured disks.
	// +kubebuilder:validation:MaxItems=7
	// +optional
	DiskSets []DiskSet `json:"diskSets,omitempty"`
}

// BootDisk is the disk the instance boots from.
type BootDisk struct {
	// Size is the boot disk's capacity.
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`
}

// ValidateSize checks the static Nexus size contract for a boot disk.
func (in BootDisk) ValidateSize() error {
	return validateDiskSize("boot disk", in.Size, true)
}

// ValidateSourceSize checks that the boot disk can hold its selected image.
func (in BootDisk) ValidateSourceSize(sourceBytes int64) error {
	return validateSourceSize("boot disk", in.Size, sourceBytes, false)
}

// DiskSet is a group of identically configured disks attached to every launched instance.
// +kubebuilder:validation:XValidation:rule="[has(self.mountPath), has(self.mountDir)].exists_one(x, x)",message="exactly one of mountPath or mountDir must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.mountPath) || self.count == 1",message="mountPath is a single mount point, so it needs count 1; a set of several disks wants mountDir, which each disk is mounted beneath"
// +kubebuilder:validation:XValidation:rule="!has(self.mountPath) || !has(self.filesystem) || self.filesystem != 'None'",message="a raw block diskSet is not mounted, so it needs mountDir, the directory its block devices are linked into, rather than mountPath"
// +kubebuilder:validation:XValidation:rule="self.type == 'Distributed' || (!has(self.imageSelector) && !has(self.snapshotSelector))",message="a Local diskSet cannot be created from an image or a snapshot: the Oxide API has no disk source for a local disk, and local disks cannot be created from distributed disk images"
// +kubebuilder:validation:XValidation:rule="!(has(self.imageSelector) && has(self.snapshotSelector))",message="a diskSet may select an image or a snapshot, not both"
// +kubebuilder:validation:XValidation:rule="!has(self.readOnly) || has(self.imageSelector) || has(self.snapshotSelector)",message="readOnly only means something for a diskSet created from an image or a snapshot"
// +kubebuilder:validation:XValidation:rule="!has(self.filesystem) || (!has(self.imageSelector) && !has(self.snapshotSelector))",message="a diskSet created from an image or a snapshot already carries a filesystem, so filesystem must not be set: it would be formatted over"
type DiskSet struct {
	// Name identifies the set.
	// +kubebuilder:validation:Pattern=`^[a-z]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=20
	Name string `json:"name"`

	// Type selects Distributed or Local disks.
	// +kubebuilder:validation:Enum=Distributed;Local
	// +kubebuilder:default=Distributed
	Type string `json:"type"`

	// Count is how many disks the set has.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=7
	// +kubebuilder:default=1
	// +optional
	Count int32 `json:"count,omitempty"`

	// Size is the capacity of each disk in the set, not of the set as a whole.
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`

	// Layout is how the set's disks are presented to the guest.
	// +kubebuilder:validation:Enum=Individual
	// +kubebuilder:default=Individual
	// +optional
	Layout string `json:"layout,omitempty"`

	// Filesystem is what the guest formats each disk with.
	// +kubebuilder:validation:Enum=ext4;xfs;None
	// +optional
	Filesystem string `json:"filesystem,omitempty"`

	// MountPath is the single mount point for a set of one disk.
	// +kubebuilder:validation:Pattern=`^/[^/]+(/[^/]+)*$`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// MountDir is the parent directory the set's disks are placed beneath.
	// +kubebuilder:validation:Pattern=`^/[^/]+(/[^/]+)*$`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	MountDir string `json:"mountDir,omitempty"`

	// ImageSelector selects the image each disk in the set is created from.
	// +optional
	ImageSelector []ImageSelector `json:"imageSelector,omitempty"`

	// SnapshotSelector selects the snapshot each disk in the set is created from.
	// +optional
	SnapshotSelector []SnapshotSelector `json:"snapshotSelector,omitempty"`

	// ReadOnly attaches sourced disks read-only.
	// +optional
	ReadOnly *bool `json:"readOnly,omitempty"`
}

// ValidateSize checks the static Nexus size contract for each disk in the set.
func (in DiskSet) ValidateSize() error {
	return validateDiskSize(
		fmt.Sprintf("diskSet %q", in.Name),
		in.Size,
		in.EffectiveType() == DiskSetTypeDistributed,
	)
}

// ValidateSourceSize checks the size contract for the set's selected source.
func (in DiskSet) ValidateSourceSize(sourceBytes int64) error {
	return validateSourceSize(
		fmt.Sprintf("diskSet %q", in.Name),
		in.Size,
		sourceBytes,
		in.ReadOnlyEnabled(),
	)
}

func validateDiskSize(
	disk string,
	size resource.Quantity,
	distributed bool,
) error {
	bytes, exact := size.AsInt64()
	switch {
	case !exact:
		return &DiskSizeError{
			Disk: disk, Requested: size.String(),
			Constraint: "must fit exactly in a signed 64-bit Oxide byte count",
		}
	case bytes < MinDiskSizeBytes:
		return &DiskSizeError{
			Disk: disk, Requested: size.String(),
			Constraint: fmt.Sprintf("must be at least %d bytes (1 GiB)", MinDiskSizeBytes),
		}
	case bytes%DiskSizeGranularityBytes != 0:
		return &DiskSizeError{
			Disk: disk, Requested: size.String(),
			Constraint: fmt.Sprintf(
				"must be a multiple of %d bytes (1 GiB)", DiskSizeGranularityBytes,
			),
		}
	case distributed && bytes > MaxDistributedDiskSizeBytes:
		return &DiskSizeError{
			Disk: disk, Requested: size.String(),
			Constraint: fmt.Sprintf(
				"must not exceed %d bytes (1023 GiB) for distributed storage",
				MaxDistributedDiskSizeBytes,
			),
		}
	default:
		return nil
	}
}

func validateSourceSize(
	disk string,
	size resource.Quantity,
	sourceBytes int64,
	readOnly bool,
) error {
	bytes, exact := size.AsInt64()
	if !exact {
		return &DiskSizeError{
			Disk: disk, Requested: size.String(),
			Constraint: "must fit exactly in a signed 64-bit Oxide byte count",
		}
	}
	if sourceBytes <= 0 {
		return &DiskSizeError{
			Disk: disk, Requested: size.String(),
			Constraint: "cannot be validated because its source size is unknown or zero",
		}
	}
	if readOnly && bytes != sourceBytes {
		return &DiskSizeError{
			Disk: disk, Requested: size.String(),
			Constraint: fmt.Sprintf(
				"must equal its read-only source size of %d bytes", sourceBytes,
			),
		}
	}
	if !readOnly && bytes < sourceBytes {
		return &DiskSizeError{
			Disk: disk, Requested: size.String(),
			Constraint: fmt.Sprintf("must be at least its source size of %d bytes", sourceBytes),
		}
	}
	return nil
}

// SnapshotSelector selects a snapshot by ID or by name.
type SnapshotSelector struct {
	// ID is the exact snapshot UUID.
	// +optional
	ID string `json:"id,omitempty"`
	// Name is the name of a snapshot.
	// +optional
	Name string `json:"name,omitempty"`
	// Project where the snapshot is located.
	// +optional
	Project string `json:"project,omitempty"`
}

// Affinity controls which Oxide affinity and anti-affinity groups launched instances join.
type Affinity struct {
	// AffinityGroupSelector selects existing affinity groups that every launched instance joins.
	// +optional
	AffinityGroupSelector []AffinityGroupSelector `json:"affinityGroupSelector,omitempty" hash:"ignore"`

	// AntiAffinityGroupSelector selects existing anti-affinity groups that every launched instance joins.
	// +optional
	AntiAffinityGroupSelector []AffinityGroupSelector `json:"antiAffinityGroupSelector,omitempty" hash:"ignore"`

	// Managed asks Karpenter to own one group for this nodeclass that every launched instance joins.
	// +optional
	Managed *ManagedAffinityGroup `json:"managed,omitempty" hash:"ignore"`
}

// ManagedAffinityGroup is the single group Karpenter owns end to end for a nodeclass.
type ManagedAffinityGroup struct {
	// Type selects affinity or anti-affinity placement.
	// +kubebuilder:validation:Enum=Affinity;AntiAffinity
	Type string `json:"type"`
	// Policy is what the rack does when the request cannot be satisfied.
	// +kubebuilder:validation:Enum=allow;fail
	// +kubebuilder:default=allow
	// +optional
	Policy string `json:"policy,omitempty"`
}

// AffinityGroupSelector selects affinity or anti-affinity groups by ID or by name.
type AffinityGroupSelector struct {
	// ID is the exact group UUID.
	// +optional
	ID string `json:"id,omitempty"`
	// Name is the name of a group.
	// +optional
	Name string `json:"name,omitempty"`
}

// Bootstrap kinds, one per supported bootstrap provider.
const (
	BootstrapKindRKE2    = "rke2"
	BootstrapKindKubeadm = "kubeadm"
)

// Bootstrap holds provider-specific bootstrap configuration.
// +kubebuilder:validation:XValidation:rule="[has(self.rke2), has(self.kubeadm)].exists_one(x, x)",message="exactly one bootstrap provider must be set"
type Bootstrap struct {
	// +optional
	RKE2 *RKE2Bootstrap `json:"rke2,omitempty"`
	// +optional
	Kubeadm *KubeadmBootstrap `json:"kubeadm,omitempty"`
}

// RKE2Config returns the RKE2 configuration, zero-valued when the nodeclass left it out.
func (in *Bootstrap) RKE2Config() RKE2Bootstrap {
	if in == nil || in.RKE2 == nil {
		return RKE2Bootstrap{}
	}
	return *in.RKE2
}

// KubeadmConfig returns the kubeadm configuration, zero-valued when the nodeclass left it out.
// The provider supplies its own defaults for the zero fields, since CRD defaulting only
// reaches a kubeadm block that was actually written.
func (in *Bootstrap) KubeadmConfig() KubeadmBootstrap {
	if in == nil || in.Kubeadm == nil {
		return KubeadmBootstrap{}
	}
	return *in.Kubeadm
}

// Kind returns the bootstrap provider configured on the nodeclass, or an empty string if none is.
func (in *Bootstrap) Kind() string {
	if in == nil {
		return ""
	}
	switch {
	case in.RKE2 != nil:
		return BootstrapKindRKE2
	case in.Kubeadm != nil:
		return BootstrapKindKubeadm
	default:
		return ""
	}
}

type EnvVar struct {
	// Name is the shell environment variable name.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_][a-zA-Z0-9_]*$`
	Name string `json:"name"`
	// Value is the literal environment variable value.
	// +optional
	Value string `json:"value,omitempty"`
	// ValueFrom selects the environment variable value from a Kubernetes object.
	// +optional
	ValueFrom *ValueFrom `json:"valueFrom,omitempty"`
}

type ValueFrom struct {
	// SecretKeyRef selects a key of a Secret.
	SecretKeyRef SecretRef `json:"secretKeyRef,omitempty"`
}

type RKE2Bootstrap struct {
	// TokenRefSecret references the Secret holding the long-lived RKE2 agent join token.
	// +optional
	TokenRefSecret SecretRef `json:"tokenRef,omitzero"`
}

// KubeadmBootstrap configures joining nodes with kubeadm.
type KubeadmBootstrap struct {
	// TokenTTL is how long the per-node bootstrap token remains valid.
	// +kubebuilder:default="15m"
	// +optional
	TokenTTL *metav1.Duration `json:"tokenTTL,omitempty"`
	// ConfigVersion is the kubeadm configuration API version used to render the JoinConfiguration.
	// +kubebuilder:validation:Enum=kubeadm.k8s.io/v1beta3;kubeadm.k8s.io/v1beta4
	// +kubebuilder:default="kubeadm.k8s.io/v1beta4"
	// +optional
	ConfigVersion string `json:"configVersion,omitempty"`
}

type SecretRef struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Key       string `json:"key,omitempty"`
}

type ImageSelector struct {
	// ID is the exact image UUID.
	// +optional
	ID string `json:"id,omitempty"`
	// Name is the name of an image.
	// +optional
	Name string `json:"name,omitempty"`
	// Project where the image is located.
	// +optional
	Project string `json:"project,omitempty"`
	// OS is the image operating system.
	// +optional
	OS string `json:"os,omitempty"`
	// Version is the version of the image or OS.
	// +optional
	Version string `json:"version,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.id) || has(self.vpcID) || has(self.vpcName)",message="a subnet selector needs a subnet id, VPC id, or VPC name"
// +kubebuilder:validation:XValidation:rule="!has(self.name) || has(self.vpcID) || has(self.vpcName)",message="a subnet name requires a VPC id or VPC name"
// +kubebuilder:validation:XValidation:rule="!(has(self.vpcID) && has(self.vpcName))",message="a subnet selector cannot set both vpcID and vpcName"
type SubnetSelector struct {
	// ID is the exact subnet ID.
	// +optional
	ID string `json:"id,omitempty"`
	// VPCID is the exact VPC ID.
	// +optional
	VPCID string `json:"vpcID,omitempty"`
	// VPCName is the exact VPC name.
	// +optional
	VPCName string `json:"vpcName,omitempty"`
	// Name is the exact subnet name.
	// +optional
	Name string `json:"name,omitempty"`
	// Project where the subnet is located.
	// +optional
	Project string `json:"project,omitempty"`
}

// EphemeralIP is one external address attached to an instance at launch.
// +kubebuilder:validation:XValidation:rule="has(self.ipVersion) || has(self.pool)",message="an ephemeral IP needs an ipVersion, to take the silo's default pool for that version, or a pool to allocate from"
type EphemeralIP struct {
	// IPVersion is the address family this entry allocates.
	// +kubebuilder:validation:Enum=v4;v6
	// +optional
	IPVersion string `json:"ipVersion,omitempty"`
	// Pool is the IP pool to allocate from.
	// +optional
	Pool *IPPoolRef `json:"pool,omitempty"`
}

// IPPoolRef identifies exactly one IP pool.
// +kubebuilder:validation:XValidation:rule="[has(self.id), has(self.name)].exists_one(x, x)",message="exactly one of id or name must be set"
type IPPoolRef struct {
	// ID is the exact IP pool UUID.
	// +optional
	ID string `json:"id,omitempty"`
	// Name is the exact name of an IP pool.
	// +optional
	Name string `json:"name,omitempty"`
}

type SSHKeySelector struct {
	// ID is the exact SSH key ID.
	// +optional
	ID string `json:"id,omitempty"`
	// Name is the exact SSH key name.
	// +optional
	Name string `json:"name,omitempty"`
}

// OxideNodeClass is an Oxide Karpenter node class.
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
// +kubebuilder:printcolumn:name="Role",type="string",JSONPath=".spec.role",priority=1,description=""
// +kubebuilder:resource:path=oxidenodeclasses,scope=Cluster,categories=karpenter,shortName={oxidenc,oxidencs,oxnc,oxncs,oxnodeclass,oxnodeclasses}
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
type OxideNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OxideNodeClassSpec   `json:"spec,omitempty"`
	Status OxideNodeClassStatus `json:"status,omitempty"`
}

// OxideNodeClassHashVersion tracks launch-affecting schema changes.
const OxideNodeClassHashVersion = "v1"

// Hash is the fingerprint of everything about a nodeclass that is fixed into an instance at launch.
func (in *OxideNodeClass) Hash() string {
	return fmt.Sprint(lo.Must(hashstructure.Hash([]any{
		in.Spec,

		in.Spec.Storage.storageFingerprint(),
	}, hashstructure.FormatV2, &hashstructure.HashOptions{
		SlicesAsSets:    true,
		IgnoreZeroValue: true,
		ZeroNil:         true,
	})))
}

func (in Storage) storageFingerprint() string {
	entries := make([]string, 0, len(in.DiskSets)+1)
	entries = append(entries, fmt.Sprintf("boot:size=%d", in.Boot.Size.Value()))
	for _, set := range in.DiskSets {
		entries = append(entries, fmt.Sprintf(
			"set/%s:type=%s,count=%d,size=%d,layout=%s,fs=%s,mountPath=%s,mountDir=%s,readOnly=%t",
			set.Name,
			set.EffectiveType(),
			set.EffectiveCount(),
			set.Size.Value(),
			set.EffectiveLayout(),
			set.EffectiveFilesystem(),
			set.MountPath,
			set.MountDir,
			set.ReadOnly != nil && *set.ReadOnly,
		))
	}
	slices.Sort(entries)
	return strings.Join(entries, ";")
}

// OxideNodeClassList contains OxideNodeClasses.
// +kubebuilder:object:root=true
type OxideNodeClassList struct {
	metav1.TypeMeta `                 json:",inline"`
	metav1.ListMeta `                 json:"metadata,omitempty"`
	Items           []OxideNodeClass `json:"items"`
}

// NeedsSourceResolution reports whether any diskSet carries an image or snapshot selector.
func (in Storage) NeedsSourceResolution() bool {
	for _, set := range in.DiskSets {
		if len(set.ImageSelector) > 0 || len(set.SnapshotSelector) > 0 {
			return true
		}
	}
	return false
}

// Disks is how many disks a launch from this nodeclass creates, the boot disk included.
func (in Storage) Disks() int {
	total := 1
	for _, set := range in.DiskSets {
		total += int(set.EffectiveCount())
	}
	return total
}

// BootSizeBytes is the boot disk size a launch from this nodeclass gets,
// falling back to the spec while the status has yet to resolve.
func (in *OxideNodeClass) BootSizeBytes() int64 {
	if in == nil {
		return 0
	}
	if in.Status.Storage != nil && in.Status.Storage.BootSizeBytes > 0 {
		return in.Status.Storage.BootSizeBytes
	}
	return in.Spec.Storage.Boot.Size.Value()
}

// MaxDisks is the Oxide API's limit on disks attached to one instance.
func MaxDisks() int { return maxAttachedDisks }

// EffectiveCount is the set's disk count, treating an unset count as one.
func (in DiskSet) EffectiveCount() int32 {
	if in.Count < 1 {
		return 1
	}
	return in.Count
}

// EffectiveLayout returns Individual when the layout is unset.
func (in DiskSet) EffectiveLayout() string {
	if in.Layout == "" {
		return DiskSetLayoutIndividual
	}
	return in.Layout
}

// EffectiveType returns Distributed when the type is unset.
func (in DiskSet) EffectiveType() string {
	if in.Type == "" {
		return DiskSetTypeDistributed
	}
	return in.Type
}

// Sourced reports whether the set's disks are created from an image or a snapshot.
func (in DiskSet) Sourced() bool {
	return len(in.ImageSelector) > 0 || len(in.SnapshotSelector) > 0
}

// ReadOnlyEnabled reports whether sourced disks are attached read-only.
func (in DiskSet) ReadOnlyEnabled() bool {
	return in.ReadOnly != nil && *in.ReadOnly
}

// EffectiveFilesystem is what the guest should format each of the set's disks with.
func (in DiskSet) EffectiveFilesystem() string {
	if in.Sourced() {
		return ""
	}
	if in.Filesystem == "" {
		return FilesystemExt4
	}
	return in.Filesystem
}

// RawBlock reports whether the set contains raw block devices.
func (in DiskSet) RawBlock() bool {
	return in.EffectiveFilesystem() == FilesystemNone
}
