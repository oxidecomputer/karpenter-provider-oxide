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
// This will contain configuration necessary to launch instances on Oxide.
type OxideNodeClassSpec struct {
	// UserData to be applied to the provisioned nodes.
	// It must be in the appropriate format based on the machine image in use. Karpenter will merge certain fields into
	// this UserData to ensure nodes are being provisioned with the correct configuration.
	// +optional
	UserData string   `json:"userData,omitempty"`
	Env      []EnvVar `json:"env,omitempty"`
	// Server is the endpoint nodes use to join the cluster. When empty it is discovered from the
	// default/kubernetes EndpointSlice. Each bootstrap provider renders the discovered address in
	// the form it needs, so the port differs by provider: RKE2 joins on the supervisor port (9345)
	// while kubeadm joins on the kube-apiserver port (6443).
	// +optional
	Server string `json:"server,omitempty"`
	// KubeletArgs are extra kubelet arguments in "key=value" form applied to launched nodes,
	// in addition to the args Karpenter requires.
	// +optional
	KubeletArgs []string `json:"kubeletArgs,omitempty"`
	// Bootstrap selects and configures the mechanism nodes use to join the cluster.
	// Exactly one provider may be set.
	// +optional
	Bootstrap *Bootstrap `json:"bootstrap,omitempty"`
	// ImageSelector is used to select an image to launch nodes with.
	// If multiple images match then the most recently uploaded image is selected.
	// Selectors are OR'd together, but terms within a selector are AND'd together
	//
	// Excluded from the nodeclass hash for the reason every selector is: what a selector matches is a
	// function of the project as well as of the nodeclass, so drift is decided by comparing resolved
	// state rather than by hashing the terms. A selector rewritten to name the image it already
	// matched has changed nothing about the running nodes, and a new image uploaded into an unchanged
	// wildcard has. See IsDrifted.
	ImageSelector []ImageSelector `json:"imageSelector,omitempty" hash:"ignore"`
	// Storage is the disks every instance launched from this nodeclass is created with: the boot
	// disk, and any additional sets of data disks.
	//
	// A value rather than a pointer, and required: a nodeclass has always needed a boot disk size,
	// so making the whole stanza optional would buy nothing but a nil check in every reader.
	//
	// Everything here except the nested source selectors is fixed into an instance at launch: a disk
	// cannot be resized, re-sourced or re-mounted afterwards, so a change to any of it is
	// drift by definition.
	//
	// Excluded from the reflected part of the nodeclass hash all the same, and folded in as an
	// explicit fingerprint instead. Reflection cannot be used here because the sizes are
	// resource.Quantity, which keeps its value in unexported fields and its formatting in an exported
	// one, so a reflected hash would see how a size was written rather than what it is. Tagging the
	// individual sizes does not work either: hashstructure only honours hash:"ignore" on fields of the
	// struct it is handed, not on fields of structs nested inside it, so a tag down here would be
	// silently ineffective. See Hash and storageFingerprint.
	// +kubebuilder:validation:Required
	Storage Storage `json:"storage" hash:"ignore"`
	// SubnetSelector is used to select subnets to launch nodes into.
	// Multiple subnets can match. Node launches will use subnets with available IP space.
	//
	// Excluded from the nodeclass hash for the same reason ImageSelector is.
	SubnetSelector []SubnetSelector `json:"subnetSelector,omitempty" hash:"ignore"`
	// SSHKeySelector is used to select SSH keys that will be copied to the node
	//
	// Excluded from the nodeclass hash for the same reason ImageSelector is.
	// +optional
	SSHKeySelector []SSHKeySelector `json:"sshKeySelector,omitempty" hash:"ignore"`
	// IPPoolSelector is used to select IP Pools to allocate ephemeral IPs from
	//
	// Excluded from the nodeclass hash for the same reason ImageSelector is.
	// +optional
	IPPoolSelector []IPPoolSelector `json:"ipPoolSelector,omitempty" hash:"ignore"`
	// Affinity controls which Oxide affinity and anti-affinity groups launched instances join.
	// +optional
	Affinity *Affinity `json:"affinity,omitempty"`
}

// maxAttachedDisks is the Oxide API's limit on disks attached to one instance. The boot disk counts
// against it, which is why the diskSets budget is one less.
const maxAttachedDisks = 8

// Filesystem values a diskSet's disks can be formatted with. FilesystemNone leaves a disk as a raw
// block device: nothing is created on it and nothing is mounted.
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
//
// Individual is the only one. The field exists so that a layout that combines a set's disks into one
// volume can be added later without changing the meaning of a nodeclass that does not ask for it.
const (
	DiskSetLayoutIndividual = "Individual"
)

// Storage is the whole disk layout of a launched instance.
//
// The 7-disk budget on diskSets is the Oxide attachment limit of 8 minus the boot disk. It is
// checked here and again when a launch plans its disks: CEL refuses the edit, and the launch check
// exists because CEL cannot be relied on to have run -- a nodeclass stored before this rule shipped
// is not re-validated by the apiserver until it is next written.
//
// The MaxItems on diskSets is load-bearing rather than cosmetic. The apiserver's CEL cost estimator
// bounds all(), exists_one() and map() by the declared maximum list length, and the uniqueness rules
// below are O(n^2); without a bound their estimated cost exceeds the per-rule budget and the CRD
// fails to install.
//
// +kubebuilder:validation:XValidation:rule="!has(self.diskSets) || self.diskSets.map(s, s.count).sum() <= 7",message="an instance can attach at most 8 disks and the boot disk is one of them, so the diskSets' counts must sum to 7 or fewer"
// +kubebuilder:validation:XValidation:rule="!has(self.diskSets) || self.diskSets.all(x, self.diskSets.exists_one(y, y.name == x.name))",message="each diskSet must have a distinct name"
// +kubebuilder:validation:XValidation:rule="!has(self.diskSets) || self.diskSets.all(x, self.diskSets.exists_one(y, (has(y.mountPath) ? y.mountPath : y.mountDir) == (has(x.mountPath) ? x.mountPath : x.mountDir)))",message="each diskSet must have a distinct mountPath or mountDir"
type Storage struct {
	// Boot is the disk the instance boots from. Its image comes from spec.imageSelector rather than
	// from here: the boot image is what the whole nodeclass is built around, it is what
	// nodeClaim.Status.ImageID records, and it is what image drift is decided on.
	// +kubebuilder:validation:Required
	Boot BootDisk `json:"boot"`

	// DiskSets are additional disks attached to every instance, in sets of identically configured
	// disks.
	//
	// Order is not significant to what the guest sees, but it does decide the disk names, so
	// reordering a nodeclass's diskSets changes the names of the disks the next launch creates. That
	// is deliberately not drift: the names are internal and every launch renders its own, so the
	// nodeclass hash is unchanged by a reorder even though the names are not.
	// +kubebuilder:validation:MaxItems=7
	// +optional
	DiskSets []DiskSet `json:"diskSets,omitempty"`
}

// BootDisk is the disk the instance boots from. Always distributed: a local disk cannot be created
// from an image, and there is no other way to get an operating system onto it.
type BootDisk struct {
	// Size is the boot disk's capacity. It must be at least as large as the image it is created
	// from, which is checked when the nodeclass is reconciled rather than here, because only the
	// resolved image knows how large it is.
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`
}

// DiskSet is a group of identically configured disks attached to every launched instance.
//
// mountPath and mountDir are two mutually exclusive fields rather than one field with two meanings.
// mountPath is a literal mount point; mountDir is a parent directory, and disk k lands at
// <mountDir>/<name>-<k>. Naming the two cases is what makes the incoherent combination
// unrepresentable: the local static provisioner discovers the *contents* of its hostDir, so a
// hostDir that is itself a symlink to a raw block device discovers nothing. It also means there is
// no discontinuity in what a path means as count grows from one to two.
//
// +kubebuilder:validation:XValidation:rule="[has(self.mountPath), has(self.mountDir)].exists_one(x, x)",message="exactly one of mountPath or mountDir must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.mountPath) || self.count == 1",message="mountPath is a single mount point, so it needs count 1; a set of several disks wants mountDir, which each disk is mounted beneath"
// +kubebuilder:validation:XValidation:rule="!has(self.mountPath) || !has(self.filesystem) || self.filesystem != 'None'",message="a raw block diskSet is not mounted, so it needs mountDir, the directory its block devices are linked into, rather than mountPath"
// +kubebuilder:validation:XValidation:rule="self.type == 'Distributed' || (!has(self.imageSelector) && !has(self.snapshotSelector))",message="a Local diskSet cannot be created from an image or a snapshot: the Oxide API has no disk source for a local disk, and local disks cannot be created from distributed disk images"
// +kubebuilder:validation:XValidation:rule="!(has(self.imageSelector) && has(self.snapshotSelector))",message="a diskSet may select an image or a snapshot, not both"
// +kubebuilder:validation:XValidation:rule="!has(self.readOnly) || has(self.imageSelector) || has(self.snapshotSelector)",message="readOnly only means something for a diskSet created from an image or a snapshot"
// +kubebuilder:validation:XValidation:rule="!has(self.filesystem) || (!has(self.imageSelector) && !has(self.snapshotSelector))",message="a diskSet created from an image or a snapshot already carries a filesystem, so filesystem must not be set: it would be formatted over"
type DiskSet struct {
	// Name identifies the set. It appears in the disk names Karpenter creates and in the directory
	// each disk is mounted under, so it is constrained to what is legal in both.
	//
	// The minimum of three characters is not cosmetic. It is what makes every disk name at least 20
	// bytes long, and 20 bytes is exactly what the guest sees of a disk name as its NVMe serial
	// number. A shorter name would be padded by the NVMe layer, which the guest would then have to
	// know to strip. See pkg/providers/storage.
	// +kubebuilder:validation:Pattern=`^[a-z]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=20
	Name string `json:"name"`

	// Type is whether the set's disks are Distributed, replicated across the rack by Crucible and
	// able to outlive the sled, or Local, sitting on the sled the instance is placed on.
	//
	// Local disks have a fixed 4 KiB block size, cannot be created from an image or a snapshot, and
	// do not outlive the instance. They are also provisioned differently: a local disk's capacity
	// belongs to one sled, so it is created by the instance create itself rather than ahead of it.
	// See pkg/providers/instance.
	// +kubebuilder:validation:Enum=Distributed;Local
	// +kubebuilder:default=Distributed
	Type string `json:"type"`

	// Count is how many disks the set has. Every disk in the set has the same size, type, source and
	// filesystem; what differs is where it is mounted.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=7
	// +kubebuilder:default=1
	// +optional
	Count int32 `json:"count,omitempty"`

	// Size is the capacity of each disk in the set, not of the set as a whole. A set of four 500Gi
	// disks provisions 2Ti.
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`

	// Layout is how the set's disks are presented to the guest. Individual, the only value, gives one
	// filesystem per disk, which is what the local static provisioner consumes.
	// +kubebuilder:validation:Enum=Individual
	// +kubebuilder:default=Individual
	// +optional
	Layout string `json:"layout,omitempty"`

	// Filesystem is what the guest formats each disk with. None leaves the disk as a raw block
	// device: nothing is created on it and nothing is mounted, which is what a
	// consumer that manages its own on-disk format wants.
	//
	// Deliberately without a CRD default even though an unset filesystem means ext4. A default
	// applies unconditionally, and a disk created from an image or a snapshot must never be
	// formatted, so a defaulted ext4 would be an instruction to destroy the image the user asked
	// for, and CRDs have no conditional default. Unset is therefore resolved to ext4 only where
	// formatting is meaningful, and the effective value is published on the status so it is still
	// readable off the object.
	// +kubebuilder:validation:Enum=ext4;xfs;None
	// +optional
	Filesystem string `json:"filesystem,omitempty"`

	// MountPath is the single mount point for a set of one disk. Mutually exclusive with MountDir.
	// +kubebuilder:validation:Pattern=`^/[^/]+(/[^/]+)*$`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// MountDir is the parent directory the set's disks are placed beneath: disk k is mounted at
	// <mountDir>/<name>-<k>, counting from zero, or for a raw block set is linked there instead.
	// That is the layout the local static provisioner's hostDir expects. Mutually exclusive with
	// MountPath.
	// +kubebuilder:validation:Pattern=`^/[^/]+(/[^/]+)*$`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	MountDir string `json:"mountDir,omitempty"`

	// ImageSelector selects the image each disk in the set is created from. Only for Distributed
	// sets: the Oxide API has no disk source for a local disk at all.
	//
	// Resolved per set, so two sets carrying the same selector resolve once between them. A set whose
	// selector matches nothing holds the nodeclass un-Ready rather than launching a disk with an
	// empty source.
	//
	// Excluded from the nodeclass hash for the reason every selector is: what it matches is a
	// function of the project as well as of the nodeclass, so its drift is decided by comparing
	// resolved state. Excluded by storageFingerprint simply not covering it, rather than by a tag,
	// which at this depth would do nothing.
	// +optional
	ImageSelector []ImageSelector `json:"imageSelector,omitempty"`

	// SnapshotSelector selects the snapshot each disk in the set is created from.
	//
	// Excluded from the nodeclass hash for the same reason ImageSelector is.
	// +optional
	SnapshotSelector []SnapshotSelector `json:"snapshotSelector,omitempty"`

	// ReadOnly asks the rack to attach the disks read-only, which is only meaningful for a disk
	// created from an image or a snapshot. It is a property of the attachment rather than of the
	// guest's mount: the guest cannot write to it whatever it does with the mount options.
	//
	// Note that the disks of a set are still deleted with the node. A snapshot-sourced set reads
	// like "restore this dataset onto every node", and the per-node copy does not outlive the node.
	// +optional
	ReadOnly *bool `json:"readOnly,omitempty"`
}

// SnapshotSelector selects a snapshot by ID or by name.
//
// It has no OS or version term, unlike ImageSelector, because oxide.Snapshot has no such fields: a
// snapshot in the Oxide API is an ID, a name, a project, the disk it came from, a size and a state,
// and there is nothing else to match on.
//
// Deliberately without CEL requiring exactly one of id/name, matching ImageSelector and the rest of
// the selectors, which validate at reconcile instead.
type SnapshotSelector struct {
	// ID is the exact snapshot UUID.
	// +optional
	ID string `json:"id,omitempty"`
	// Name is the name of a snapshot. A name without a "*" must match exactly, ignoring case; a name
	// containing "*" selects every snapshot whose name matches, where each "*" stands for any run of
	// characters including none. Matched character by character, the same way an image name is.
	//
	// When a wildcard matches several snapshots the most recently created one is used.
	// +optional
	Name string `json:"name,omitempty"`
	// Project where the snapshot is located. Unlike an image, a snapshot is always in a project:
	// there is no silo-level snapshot, so an empty project means the project Karpenter runs in.
	// +optional
	Project string `json:"project,omitempty"`
}

// Affinity controls which Oxide affinity and anti-affinity groups launched instances join.
//
// Selected groups are referenced only: Karpenter adds its instances to them, removes its instances
// from them, and never creates, updates or deletes the groups themselves.
type Affinity struct {
	// AffinityGroupSelector selects existing affinity groups that every launched instance joins.
	// Affinity groups are an experimental Oxide API.
	//
	// Excluded from the nodeclass hash for the same reason AntiAffinityGroupSelector is.
	// +optional
	AffinityGroupSelector []AffinityGroupSelector `json:"affinityGroupSelector,omitempty" hash:"ignore"`

	// AntiAffinityGroupSelector selects existing anti-affinity groups that every launched instance
	// joins.
	//
	// Excluded from the nodeclass hash because whether nodes are replaced when this changes is
	// decided by comparing resolved groups, not by hashing the spec: a selector rewritten to name
	// the group it already matched has changed nothing about where an instance sits. See IsDrifted.
	// +optional
	AntiAffinityGroupSelector []AffinityGroupSelector `json:"antiAffinityGroupSelector,omitempty" hash:"ignore"`

	// Managed asks Karpenter to own one group for this nodeclass that every launched instance joins.
	// It is created when the nodeclass is reconciled and deleted when the nodeclass is. Its name is
	// derived from the nodeclass name and from the fields below, so that changing either produces a
	// new group rather than an edit the Oxide API cannot make.
	//
	// Excluded from the nodeclass hash for the same reason the selectors are: whether nodes are
	// replaced when this changes is decided by comparing resolved groups, not by hashing the spec.
	// A hash cannot tell a group being added from one being removed, and only the first replaces
	// nodes. See IsDrifted.
	// +optional
	Managed *ManagedAffinityGroup `json:"managed,omitempty" hash:"ignore"`
}

// ManagedAffinityGroup is the single group Karpenter owns end to end for a nodeclass.
//
// Being a single pointer with a Type enum is what structurally enforces one managed group per
// nodeclass, with no CEL needed. Both its fields are mutable, and changing either creates a
// different group: the Oxide API cannot patch a group's policy or its kind, so a successor has to
// be creatable while the predecessor still exists, which is why the name encodes both.
type ManagedAffinityGroup struct {
	// Type is whether the managed group co-locates instances on a sled (Affinity) or spreads them
	// across sleds (AntiAffinity). Affinity groups are an experimental Oxide API.
	// +kubebuilder:validation:Enum=Affinity;AntiAffinity
	Type string `json:"type"`
	// Policy is what the rack does when the request cannot be satisfied. Allow places the instance
	// anyway; fail refuses the launch, which is a hazard worth opting into rather than defaulting
	// to, since a refusal for want of a satisfiable placement is not a refusal any smaller instance
	// shape would avoid.
	// +kubebuilder:validation:Enum=allow;fail
	// +kubebuilder:default=allow
	// +optional
	Policy string `json:"policy,omitempty"`
}

// AffinityGroupSelector selects affinity or anti-affinity groups by ID or by name.
//
// Deliberately without CEL requiring exactly one of id/name, matching ImageSelector and the rest of
// the selectors, which validate at reconcile instead.
type AffinityGroupSelector struct {
	// ID is the exact group UUID.
	// +optional
	ID string `json:"id,omitempty"`
	// Name is the name of a group. A name without a "*" must match exactly, ignoring case.
	//
	// A name containing "*" selects every group whose name matches, where each "*" stands for any
	// run of characters including none. So "karpenter-*" selects both "karpenter-spread-a" and
	// "karpenter-spread-b".
	//
	// Unlike an image selector, where a wildcard picks a single winner, every matching group is
	// joined. A wildcard therefore makes the set of groups a function of the project's contents
	// rather than of the nodeclass alone: a group created in the project whose name matches an
	// existing selector joins the set, and because an instance can only join a group at launch,
	// every node then drifts and is replaced so it can join. That is a fleet replacement triggered
	// from outside Kubernetes.
	// +optional
	Name string `json:"name,omitempty"`
}

// Bootstrap kinds, one per supported bootstrap provider. The kind is derived from which
// provider stanza is set rather than being spelled out by the user.
const (
	BootstrapKindRKE2    = "rke2"
	BootstrapKindKubeadm = "kubeadm"
)

// Bootstrap holds provider-specific bootstrap configuration. Fields shared by every provider
// (Server, KubeletArgs) live on OxideNodeClassSpec instead.
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

// KubeadmBootstrap configures joining nodes with kubeadm. Discovery information is read from the
// kube-public/cluster-info ConfigMap and a short-lived bootstrap token is minted per NodeClaim,
// so no long-lived join credential needs to be configured here.
type KubeadmBootstrap struct {
	// TokenTTL is how long the per-node bootstrap token remains valid. The token only needs to
	// survive until the node completes TLS bootstrap, so this should stay short.
	// +kubebuilder:default="15m"
	// +optional
	TokenTTL *metav1.Duration `json:"tokenTTL,omitempty"`
	// ConfigVersion is the kubeadm configuration API version used to render the JoinConfiguration.
	// v1beta4 requires Kubernetes 1.31 or newer.
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
	// ID is the exact image UUID
	// +optional
	ID string `json:"id,omitempty"`
	// Name is the name of an image. A name without a "*" must match exactly, ignoring case.
	//
	// A name containing "*" selects every image whose name matches, where each "*" stands for any
	// run of characters including none. So "rke2-ubuntu-*" and "rke2-*" both select
	// "rke2-ubuntu-1780929039", and "*-ubuntu-*" selects it as well. Unlike a version, a name is
	// matched character by character, so a wildcard does not have to fall on a "-" boundary.
	//
	// When a wildcard matches several images the most recently uploaded one is launched.
	// +optional
	Name string `json:"name,omitempty"`
	// Project where the image is located.
	// If the project is not specified, then we query for Silo level images
	// that are not linked to a project.
	// +optional
	Project string `json:"project,omitempty"`
	// OS is the operating system of the image
	// +optional
	OS string `json:"os,omitempty"`
	// Version is the version of the image or OS. Oxide image versions are free-form, so a version
	// without a "*" is matched literally and case-insensitively.
	//
	// A version containing "*" is instead matched a segment at a time, where segments are separated
	// by ".", "-", "_", or whitespace, and a leading "v" is ignored on both sides. A "*" matches
	// exactly one segment, except as the last segment of the version, where it matches one or more
	// remaining segments. So "1.2.*" selects "1.2.3" but not "1.2" or "1.20"; "1.*" selects both
	// "1.4" and "1.4.2"; and "24.04 *" selects "24.04 2026-05-09".
	//
	// When a wildcard matches several images the most recently uploaded one is launched.
	// +optional
	Version string `json:"version,omitempty"`
}

type SubnetSelector struct {
	// ID is the exact subnet ID
	// +optional
	ID string `json:"id,omitempty"`
	// VPCID is the exact VPC ID
	// +optional
	VPCID string `json:"vpcID,omitempty"`
	// VPCName is the exact name of the VPC
	// +optional
	VPCName string `json:"vpcName,omitempty"`
	// Name is the exact name of a subnet
	// +optional
	Name string `json:"name,omitempty"`
	// Project where the subnet is located.
	// +optional
	Project string `json:"project,omitempty"`
}

type IPPoolSelector struct {
	// ID is the exact IP Pool ID
	// +optional
	ID string `json:"id,omitempty"`
	// Name is the exact name of an IP Pool
	// +optional
	Name string `json:"name,omitempty"`
	// Default is the default IP Pool
	// +optional
	Default bool `json:"default,omitempty"`
	// IPVersion is either v4 or v6
	// +optional
	IPVersion string `json:"ipVersion,omitempty"`
}

type SSHKeySelector struct {
	// ID is the exact SSH Key ID
	// +optional
	ID string `json:"id,omitempty"`
	// Name is the exact name of an SSH Key
	// +optional
	Name string `json:"name,omitempty"`
}

// OxideNodeClass is the Schema for the OxideNodeClass API
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

// We need to bump the OxideNodeClassHashVersion when we make an update to the OxideNodeClass CRD under these conditions:
// 1. A field changes its default value for an existing field that is already hashed
// 2. A field is added to the hash calculation with an already-set value
// 3. A field is removed from the hash calculations
//
// Bumping it is what stops such a change from replacing every node in the cluster over a hash that
// moved for reasons the nodeclass did not: a NodeClaim recorded at a different version is not
// compared, and is instead re-stamped with the current hash. See pkg/controllers/nodeclass/hash.go.
// Bumped to v1 when spec.bootDiskSize became spec.storage, which is conditions 2 and 3 above in one
// change: a hashed field removed, and a new one added with a value already set. Without the bump
// every node in every cluster would be replaced on upgrade over a hash that moved for reasons the
// nodeclass did not.
const OxideNodeClassHashVersion = "v1"

// Hash is the fingerprint of everything about a nodeclass that is fixed into an instance at launch
// and cannot be changed afterwards: its user data and environment, how it bootstraps, the args its
// kubelet runs with, the disks it is created with. A node whose hash no longer matches its nodeclass
// has drifted, because the only way to give it the current configuration is to launch a new one.
//
// Deliberately not everything in the spec. The selector fields carry hash:"ignore" because what they
// match depends on the project as much as on the nodeclass, so their drift is decided by comparing
// resolved state instead -- otherwise a wildcard that quietly matched a newly uploaded image would
// never drift, and a selector rewritten to name the resource it already matched always would.
//
// A secret referenced by an EnvVar is hashed as the reference, not as its contents. Rotating the
// secret in place therefore replaces no nodes; the nodes that come after it pick the new value up.
func (in *OxideNodeClass) Hash() string {
	return fmt.Sprint(lo.Must(hashstructure.Hash([]any{
		in.Spec,
		// The storage layout, written out rather than reflected over. See storageFingerprint.
		in.Spec.Storage.storageFingerprint(),
	}, hashstructure.FormatV2, &hashstructure.HashOptions{
		SlicesAsSets:    true,
		IgnoreZeroValue: true,
		ZeroNil:         true,
	})))
}

// storageFingerprint renders the storage layout as a canonical string, covering everything about it
// that is fixed into an instance at launch.
//
// Written out rather than reflected over, for two reasons that both make reflection wrong here. The
// sizes are resource.Quantity, whose value lives in unexported fields and whose formatting lives in
// an exported one, so a reflected hash would miss a resize entirely and would instead report "1Gi"
// and "1G" -- or a quantity that had merely been through a round trip -- as different disks. And
// hashstructure honours hash:"ignore" only on fields of the struct handed to it, not on fields of
// structs nested within, so there is no tag that could exclude the quantities from a reflected walk
// of this type.
//
// The source selectors are deliberately not covered, for the reason no selector is hashed: what a
// selector matches is a function of the project as well as of the nodeclass, so its drift is decided
// by comparing resolved state instead.
//
// Entries are keyed by set name and sorted, so reordering diskSets does not change the value. That is
// intentional even though reordering does change the disk names a launch mints: the names are internal
// and every launch renders its own, so a reorder replaces no nodes.
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

// OxideNodeClassList contains a list of OxideNodeClass
// +kubebuilder:object:root=true
type OxideNodeClassList struct {
	metav1.TypeMeta `                 json:",inline"`
	metav1.ListMeta `                 json:"metadata,omitempty"`
	Items           []OxideNodeClass `json:"items"`
}

// NeedsSourceResolution reports whether any diskSet carries an image or snapshot selector, and so
// whether ConditionTypeStorageResolved applies to this nodeclass at all.
//
// Keyed on the spec rather than on what resolved, so the condition does not appear and disappear as
// the project changes underneath it.
func (in Storage) NeedsSourceResolution() bool {
	for _, set := range in.DiskSets {
		if len(set.ImageSelector) > 0 || len(set.SnapshotSelector) > 0 {
			return true
		}
	}
	return false
}

// Disks is how many disks a launch from this nodeclass creates, the boot disk included. It is what
// the Oxide attachment limit applies to.
func (in Storage) Disks() int {
	total := 1
	for _, set := range in.DiskSets {
		total += int(set.EffectiveCount())
	}
	return total
}

// MaxDisks is the Oxide API's limit on disks attached to one instance.
func MaxDisks() int { return maxAttachedDisks }

// EffectiveCount is the set's disk count, treating an unset count as one. The CRD defaults it, so
// this only matters for an object that never went through the apiserver, such as one built in a test.
func (in DiskSet) EffectiveCount() int32 {
	if in.Count < 1 {
		return 1
	}
	return in.Count
}

// EffectiveLayout is the set's layout, treating an unset layout as Individual, for the same reason
// EffectiveCount exists.
func (in DiskSet) EffectiveLayout() string {
	if in.Layout == "" {
		return DiskSetLayoutIndividual
	}
	return in.Layout
}

// EffectiveType is the set's type, treating an unset type as Distributed, for the same reason
// EffectiveCount exists.
func (in DiskSet) EffectiveType() string {
	if in.Type == "" {
		return DiskSetTypeDistributed
	}
	return in.Type
}

// Sourced reports whether the set's disks are created from an image or a snapshot, and so whether
// they arrive already carrying a filesystem that must never be formatted over.
func (in DiskSet) Sourced() bool {
	return len(in.ImageSelector) > 0 || len(in.SnapshotSelector) > 0
}

// EffectiveFilesystem is what the guest should format each of the set's disks with: empty when
// nothing is to be formatted, and otherwise ext4, xfs or FilesystemNone.
//
// This is where the deliberate absence of a CRD default on Filesystem is resolved. A default would
// apply unconditionally, including to a set created from an image, where formatting would destroy
// exactly what the user asked for. So an unset filesystem means ext4 only where formatting is
// meaningful, and means "leave it alone" on a sourced set.
func (in DiskSet) EffectiveFilesystem() string {
	if in.Sourced() {
		return ""
	}
	if in.Filesystem == "" {
		return FilesystemExt4
	}
	return in.Filesystem
}

// RawBlock reports whether the set's disks are left as raw block devices, formatted with nothing and
// mounted nowhere.
func (in DiskSet) RawBlock() bool {
	return in.EffectiveFilesystem() == FilesystemNone
}
