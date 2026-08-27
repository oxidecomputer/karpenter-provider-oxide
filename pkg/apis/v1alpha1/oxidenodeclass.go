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
	// UserData to be applied to the provisioned nodes.
	// +optional
	UserData string   `json:"userData,omitempty"`
	Env      []EnvVar `json:"env,omitempty"`
	// Server is the endpoint nodes use to join the cluster.
	// +optional
	Server string `json:"server,omitempty"`
	// KubeletArgs are extra kubelet arguments in "key=value" form applied to launched nodes.
	// +optional
	KubeletArgs []string `json:"kubeletArgs,omitempty"`
	// Bootstrap selects and configures the mechanism nodes use to join the cluster.
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
}

const maxAttachedDisks = 8

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

	// Pool keeps unused boot disks ready.
	// +optional
	Pool *BootDiskPool `json:"pool,omitempty" hash:"ignore"`
}

const (
	DefaultBootDiskPoolMinReady   = int32(2)
	DefaultBootDiskPoolPercentage = int32(10)
	DefaultBootDiskPoolMaxReady   = int32(20)
)

// BootDiskPool controls unused boot disks.
// +kubebuilder:validation:XValidation:rule="!has(self.minReady) || !has(self.maxReady) || self.minReady <= self.maxReady",message="minReady must not exceed maxReady"
type BootDiskPool struct {
	// Enabled defaults to true.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// MinReady is the smallest ready pool.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=2
	// +optional
	MinReady *int32 `json:"minReady,omitempty"`
	// Percentage is the ready disks per 100 nodes.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	// +optional
	Percentage *int32 `json:"percentage,omitempty"`
	// MaxReady is the largest ready pool.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=20
	// +optional
	MaxReady *int32 `json:"maxReady,omitempty"`
}

func (in BootDisk) PoolEnabled() bool {
	return in.Pool == nil || in.Pool.Enabled == nil || *in.Pool.Enabled
}

func (in BootDisk) PoolMinReady() int32 {
	if in.Pool != nil && in.Pool.MinReady != nil {
		return *in.Pool.MinReady
	}
	return DefaultBootDiskPoolMinReady
}

func (in BootDisk) PoolPercentage() int32 {
	if in.Pool != nil && in.Pool.Percentage != nil {
		return *in.Pool.Percentage
	}
	return DefaultBootDiskPoolPercentage
}

func (in BootDisk) PoolMaxReady() int32 {
	if in.Pool != nil && in.Pool.MaxReady != nil {
		return *in.Pool.MaxReady
	}
	return DefaultBootDiskPoolMaxReady
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
	Name      string     `json:"name,omitempty"`
	Value     string     `json:"value,omitempty"`
	ValueFrom *ValueFrom `json:"valueFrom,omitempty"`
}

type ValueFrom struct {
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
