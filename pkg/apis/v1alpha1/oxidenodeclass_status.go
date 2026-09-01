package v1alpha1

import (
	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionTypeImagesResolved reports whether the image selectors matched at least one image.
const ConditionTypeImagesResolved = "ImagesResolved"

// ConditionReasonImagesNotFound is an image selector, wildcard included, that matched nothing.
const ConditionReasonImagesNotFound = "ImagesNotFound"

// ConditionTypeSubnetsResolved reports whether at least one launch subnet is available.
const ConditionTypeSubnetsResolved = "SubnetsResolved"

// ConditionReasonSubnetsNotFound is a subnet selector that matched nothing.
const ConditionReasonSubnetsNotFound = "SubnetsNotFound"

// Condition types owned by the bootstrap providers.
const (
	// ConditionTypeTokenRefSecretAccessible reports whether the RKE2 agent token Secret can be read.
	ConditionTypeTokenRefSecretAccessible = "TokenRefSecretAccessible"
	// ConditionTypeClusterInfoAvailable reports whether kube-public/cluster-info could be read.
	ConditionTypeClusterInfoAvailable = "ClusterInfoAvailable"
)

// ConditionTypeAffinityGroupsResolved reports whether every affinity group selector resolved.
const ConditionTypeAffinityGroupsResolved = "AffinityGroupsResolved"

// ConditionTypeStorageResolved reports whether the complete storage layout is valid and its sources resolved.
const ConditionTypeStorageResolved = "StorageResolved"

// ConditionTypeEphemeralIPsResolved reports whether every ephemeral IP entry named a pool that exists.
const ConditionTypeEphemeralIPsResolved = "EphemeralIPsResolved"

// Reasons recorded on ConditionTypeEphemeralIPsResolved.
const (
	// ConditionReasonIPPoolNotFound is an ephemeral IP entry naming a pool that does not exist.
	ConditionReasonIPPoolNotFound = "IPPoolNotFound"
	// ConditionReasonIPVersionMismatch identifies a pool with the wrong address family.
	ConditionReasonIPVersionMismatch = "IPVersionMismatch"
	// ConditionReasonDuplicateIPVersion is two entries resolving to the same address family.
	ConditionReasonDuplicateIPVersion = "DuplicateIPVersion"
)

// Reasons recorded on ConditionTypeStorageResolved.
const (
	// ConditionReasonDiskSourceNotFound is a diskSet selector, wildcard included, that matched nothing.
	ConditionReasonDiskSourceNotFound = "DiskSourceNotFound"
	// ConditionReasonSnapshotsNotFound is a snapshot selector, wildcard included, that matched nothing.
	ConditionReasonSnapshotsNotFound = "SnapshotsNotFound"
	// ConditionReasonInvalidDiskSize identifies a disk outside Nexus's static size contract.
	ConditionReasonInvalidDiskSize = "InvalidDiskSize"
	// ConditionReasonDiskSourceSizeMismatch identifies a disk incompatible with its source size.
	ConditionReasonDiskSourceSizeMismatch = "DiskSourceSizeMismatch"
	// ConditionReasonDiskTooSmall is the deprecated former reason for a source-size mismatch.
	ConditionReasonDiskTooSmall = "DiskTooSmall"
	// ConditionReasonTooManyDisks is a layout asking for more than the eight disks an instance can attach.
	ConditionReasonTooManyDisks = "TooManyDisks"
)

// Reasons recorded on ConditionTypeAffinityGroupsResolved.
const (
	// ConditionReasonAffinityGroupNotFound is a selector, wildcard included, that matched nothing.
	ConditionReasonAffinityGroupNotFound = "AffinityGroupNotFound"
	// ConditionReasonInvalidGroupName is a nodeclass name that cannot produce a legal Oxide name for the managed group.
	ConditionReasonInvalidGroupName = "InvalidGroupName"
	// ConditionReasonGroupNotOwned identifies a conflicting unmanaged group.
	ConditionReasonGroupNotOwned = "GroupNotOwned"
)

// Group kinds reported on OxideAffinityGroup.Type.
const (
	AffinityGroupTypeAffinity     = "Affinity"
	AffinityGroupTypeAntiAffinity = "AntiAffinity"
)

// OxideNodeClassStatus is the resolved node class state.
type OxideNodeClassStatus struct {
	// Server is the user-defined or system discovered endpoint that nodes will use to join the cluster.
	// +optional
	Server string `json:"server,omitempty"`
	// KubeletArgs are applied to launched nodes.
	// +optional
	KubeletArgs []string `json:"kubeletArgs,omitempty"`
	// NodeTaints are applied to launched nodes.
	// +optional
	NodeTaints []string `json:"nodeTaints,omitempty"`
	// Images contain selected images to use for the boot disk of launched nodes, most recently uploaded first.
	// +optional
	Images []OxideImage `json:"images"`
	// Subnets contains the selected subnets.
	// +optional
	Subnets []OxideSubnet `json:"subnets"`
	// SSHKeys contains the selected SSH keys.
	// +optional
	SSHKeys []OxideSSHKey `json:"sshKeys"`
	// EphemeralIPs contain the resolved pool each launched instance takes an external address from.
	// +optional
	EphemeralIPs []OxideEphemeralIP `json:"ephemeralIPs,omitempty"`
	// AffinityGroups contain the resolved groups that every instance launched from this nodeclass joins.
	// +optional
	AffinityGroups []OxideAffinityGroup `json:"affinityGroups,omitempty"`
	// Storage is the resolved disk layout of a launched instance.
	// +optional
	Storage *StorageStatus `json:"storage,omitempty"`
	// Bootstrap contains resolved state that only applies to a single bootstrap provider.
	// +optional
	Bootstrap *BootstrapStatus `json:"bootstrap,omitempty"`
	// Conditions contains health and readiness signals.
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
}

// BootstrapStatus holds resolved state that is meaningful to exactly one bootstrap provider.
type BootstrapStatus struct {
	// +optional
	Kubeadm *KubeadmStatus `json:"kubeadm,omitempty"`
}

// KubeadmStatus is the kubeadm discovery information resolved from kube-public/cluster-info.
type KubeadmStatus struct {
	// CACertHashes pin the cluster CA during token-based discovery.
	// +optional
	CACertHashes []string `json:"caCertHashes,omitempty"`
	// ClusterName is the cluster name advertised by the cluster-info kubeconfig.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`
}

// StorageStatus is the resolved disk layout every instance launched from this nodeclass gets.
type StorageStatus struct {
	// BootSizeBytes is the boot disk size in bytes.
	// +optional
	BootSizeBytes int64 `json:"bootSizeBytes,omitempty"`
	// DiskSets is the resolved form of spec.storage.diskSets, in the same order.
	// +optional
	DiskSets []DiskSetStatus `json:"diskSets,omitempty"`
	// BootDiskPool is the current unused boot disk pool.
	// +optional
	BootDiskPool *BootDiskPoolStatus `json:"bootDiskPool,omitempty"`
}

type BootDiskPoolStatus struct {
	Desired    int32  `json:"desired"`
	Ready      int32  `json:"ready"`
	Creating   int32  `json:"creating"`
	Claimed    int32  `json:"claimed"`
	Stale      int32  `json:"stale"`
	ConfigHash string `json:"configHash,omitempty"`
	Message    string `json:"message,omitempty"`
}

// DiskSetStatus is one resolved disk set.
type DiskSetStatus struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Count is how many disks the set has, as resolved after defaulting.
	Count int32 `json:"count"`
	// SizeBytes is the capacity of each disk in the set, not of the set as a whole.
	SizeBytes int64  `json:"sizeBytes"`
	Layout    string `json:"layout"`
	// Filesystem is the effective value: ext4 where the spec left it unset and formatting applies.
	// +optional
	Filesystem string `json:"filesystem,omitempty"`
	// MountPoints is where each of the set's disks is mounted, in ordinal order.
	// +optional
	MountPoints []string `json:"mountPoints,omitempty"`
	// BlockLinks is where each of a raw block set's devices is linked, in ordinal order.
	// +optional
	BlockLinks []string `json:"blockLinks,omitempty"`
	// Images and Snapshots are the resolved sources, most recent first and truncated the same way Status.Images is.
	// +optional
	Images []OxideImage `json:"images,omitempty"`
	// +optional
	Snapshots []OxideSnapshot `json:"snapshots,omitempty"`
}

// OxideSnapshot is one resolved snapshot.
type OxideSnapshot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Project is the project the snapshot is in.
	Project string `json:"project"`
	// DiskID is the snapshot's source disk.
	// +optional
	DiskID string `json:"diskID,omitempty"`
	// SizeBytes is the snapshot size.
	// +optional
	SizeBytes   int64       `json:"sizeBytes,omitempty"`
	TimeCreated metav1.Time `json:"timeCreated"`
}

type OxideImage struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OS      string `json:"os"`
	Version string `json:"version"`
	Project string `json:"project"`
	// SizeBytes is the image size.
	// +optional
	SizeBytes   int64       `json:"sizeBytes,omitempty"`
	TimeCreated metav1.Time `json:"timeCreated"`
}

type OxideSubnet struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	VPCID     string `json:"vpcID"`
	VPCName   string `json:"vpcName"`
	IPv4Block string `json:"ipv4Block,omitempty"`
	IPv6Block string `json:"ipv6Block,omitempty"`
}

type OxideSSHKey struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OxideAffinityGroup is one resolved Oxide affinity or anti-affinity group.
type OxideAffinityGroup struct {
	ID string `json:"id"`
	// Name is the group's name at the time it resolved.
	Name string `json:"name"`
	// Type is Affinity or AntiAffinity, which is also which Oxide endpoint the group is reached on.
	Type string `json:"type"`
	// Policy is what the rack does when the group's constraint cannot be satisfied, allow or fail.
	Policy string `json:"policy"`
	// Managed marks a provider-owned group.
	Managed bool `json:"managed"`
}

// OxideEphemeralIP is one resolved entry of spec.ephemeralIPs.
type OxideEphemeralIP struct {
	// IPVersion is the resolved address family, taken from the pool when the spec named one.
	IPVersion string `json:"ipVersion"`
	// PoolID and PoolName are empty when the entry takes the silo's default pool.
	// +optional
	PoolID string `json:"poolID,omitempty"`
	// +optional
	PoolName string `json:"poolName,omitempty"`
}

// StatusConditions returns the node class condition set.
func (in *OxideNodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {

	conds := []string{
		ConditionTypeImagesResolved,
		ConditionTypeSubnetsResolved,
		ConditionTypeStorageResolved,
	}
	switch in.Spec.Bootstrap.Kind() {
	case BootstrapKindRKE2:
		if in.Spec.Bootstrap.RKE2.TokenRefSecret.Name != "" {
			conds = append(conds, ConditionTypeTokenRefSecretAccessible)
		}
	case BootstrapKindKubeadm:
		conds = append(conds, ConditionTypeClusterInfoAvailable)
	}
	if len(in.Spec.EphemeralIPs) > 0 {
		conds = append(conds, ConditionTypeEphemeralIPsResolved)
	}
	if in.Spec.Affinity != nil {
		conds = append(conds, ConditionTypeAffinityGroupsResolved)
	}
	return status.NewReadyConditions(conds...).For(in)
}

func (in *OxideNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *OxideNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}
