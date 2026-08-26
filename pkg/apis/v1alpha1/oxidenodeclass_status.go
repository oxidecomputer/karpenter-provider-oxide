package v1alpha1

import (
	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionTypeImagesResolved reports whether the image selectors matched at least one image.
// Nodes cannot launch without one, so this holds the nodeclass un-Ready rather than letting the
// launch fail later against the Oxide API with nothing on the nodeclass to explain why.
const ConditionTypeImagesResolved = "ImagesResolved"

// Condition types owned by the bootstrap providers. Each type is only registered on nodeclasses
// using the provider that owns it, so an unused provider never holds Ready false.
const (
	// ConditionTypeTokenRefSecretAccessible reports whether the RKE2 agent token Secret can be read.
	ConditionTypeTokenRefSecretAccessible = "TokenRefSecretAccessible"
	// ConditionTypeClusterInfoAvailable reports whether kube-public/cluster-info could be read and
	// parsed into kubeadm discovery information.
	ConditionTypeClusterInfoAvailable = "ClusterInfoAvailable"
)

// ConditionTypeAffinityGroupsResolved reports whether every affinity group selector resolved. It is
// only registered on a nodeclass that sets spec.affinity, so a nodeclass not using the feature is
// never held un-Ready by it.
const ConditionTypeAffinityGroupsResolved = "AffinityGroupsResolved"

// ConditionTypeAffinityPolicySatisfiable reports whether the rack could satisfy the nodeclass's
// fail-policy affinity groups the last time it was asked.
//
// False holds the nodeclass un-Ready, which is what takes it out of provisioning: an unsatisfiable
// fail policy refuses every launch regardless of shape, so the alternative is a NodeClaim created and
// deleted for every pending pod, forever. It clears itself when the back-off elapses, and one launch
// then probes whether the policy has become satisfiable.
//
// This is registered on every nodeclass that sets spec.affinity, not only those with a fail policy.
// A group's policy is only known once the group is resolved, and a condition that appeared and
// disappeared as the project changed underneath it would flap; a nodeclass with no fail policy simply
// carries this as always true.
const ConditionTypeAffinityPolicySatisfiable = "AffinityPolicySatisfiable"

// ConditionReasonAffinityPolicyUnsatisfiable is a launch refused while a fail-policy group was in the
// nodeclass's desired set. It names the policy rather than capacity because the shape had no bearing
// on the refusal, even though the Oxide API reports the two identically today.
const ConditionReasonAffinityPolicyUnsatisfiable = "AffinityPolicyUnsatisfiable"

// ConditionTypeStorageResolved reports whether every diskSet's image or snapshot selector matched
// something a disk can actually be created from, and whether each disk is large enough for the
// source it is created from.
//
// Registered only on a nodeclass with at least one diskSet carrying a source selector, following
// ConditionTypeAffinityGroupsResolved: a nodeclass whose data disks are all blank has nothing here to
// resolve and must never be held un-Ready by it. Registration is keyed on the spec rather than on
// what resolved, so the condition does not appear and disappear as the project changes underneath it.
const ConditionTypeStorageResolved = "StorageResolved"

// ConditionTypeEphemeralIPsResolved reports whether every ephemeral IP entry named a pool that
// exists, and whether the entries are mutually valid. Registered only when the nodeclass declares
// ephemeral IPs, for the reason ConditionTypeAffinityGroupsResolved is: a nodeclass with none has
// nothing here to report and must not be held un-Ready by it.
const ConditionTypeEphemeralIPsResolved = "EphemeralIPsResolved"

// Reasons recorded on ConditionTypeEphemeralIPsResolved.
const (
	// ConditionReasonIPPoolNotFound is an ephemeral IP entry naming a pool that does not exist. Nodes
	// would launch with fewer addresses than the nodeclass asked for, or none, so this holds the
	// nodeclass un-Ready rather than letting the launch fail later against the Oxide API.
	ConditionReasonIPPoolNotFound = "IPPoolNotFound"
	// ConditionReasonIPVersionMismatch is an entry setting both ipVersion and pool where the pool
	// allocates the other family. Reported rather than resolved in the pool's favour: which of the
	// two the operator meant is not knowable from here.
	ConditionReasonIPVersionMismatch = "IPVersionMismatch"
	// ConditionReasonDuplicateIPVersion is two entries resolving to the same address family. Oxide
	// allows at most one ephemeral address per version and refuses such a pair at launch, so this
	// turns a provisioning-time 400 into something readable off the nodeclass.
	ConditionReasonDuplicateIPVersion = "DuplicateIPVersion"
)

// Reasons recorded on ConditionTypeStorageResolved.
const (
	// ConditionReasonDiskSourceNotFound is a diskSet selector, wildcard included, that matched
	// nothing. Nodes cannot launch without a source for every sourced set, so this holds the
	// nodeclass un-Ready rather than letting the launch fail later against the Oxide API with
	// nothing on the nodeclass to explain why.
	ConditionReasonDiskSourceNotFound = "DiskSourceNotFound"
	// ConditionReasonDiskTooSmall is a diskSet whose size is smaller than the image or snapshot its
	// disks are created from. Reported rather than rounded up: a disk silently larger than the
	// nodeclass asked for is a storage bill nobody agreed to.
	ConditionReasonDiskTooSmall = "DiskTooSmall"
	// ConditionReasonTooManyDisks is a layout asking for more than the eight disks an instance can
	// attach. Normally refused by CEL; reported for a nodeclass stored before that rule shipped,
	// which the apiserver does not re-validate until it is next written.
	ConditionReasonTooManyDisks = "TooManyDisks"
)

// Reasons recorded on ConditionTypeAffinityGroupsResolved.
const (
	// ConditionReasonAffinityGroupNotFound is a selector, wildcard included, that matched nothing.
	ConditionReasonAffinityGroupNotFound = "AffinityGroupNotFound"
	// ConditionReasonInvalidGroupName is a nodeclass name that cannot produce a legal Oxide name for
	// the managed group. Reported rather than truncated, because a truncated name silently collides
	// with every other nodeclass that shares its first 58 characters.
	ConditionReasonInvalidGroupName = "InvalidGroupName"
	// ConditionReasonGroupNotOwned is a group already carrying the managed group's name without
	// carrying its ownership marker. It is never adopted: silently taking ownership of a hand-made
	// group means silently deleting it later.
	ConditionReasonGroupNotOwned = "GroupNotOwned"
)

// Group kinds reported on OxideAffinityGroup.Type. The two are separate collections in the Oxide
// API, reached by different endpoints, so a resolved group has to carry which one it came from.
const (
	AffinityGroupTypeAffinity     = "Affinity"
	AffinityGroupTypeAntiAffinity = "AntiAffinity"
)

// OxideNodeClassStatus contains the resolved state of the OxideNodeClass
type OxideNodeClassStatus struct {
	// Server is the user-defined or system discovered endpoint that nodes will use to join the cluster.
	// It is rendered by the configured bootstrap provider, so its form and port are provider-specific.
	// +optional
	Server string `json:"server,omitempty"`
	// KubeletArgs contain user-defined and system default kubelet args that are applied to launched nodes
	// +optional
	KubeletArgs []string `json:"kubeletArgs,omitempty"`
	// NodeTaints contain user-defined and system default node taints that are applied to launched nodes
	// +optional
	NodeTaints []string `json:"nodeTaints,omitempty"`
	// Images contain selected images to use for the boot disk of launched nodes, most recently
	// uploaded first. Nodes always launch with the first entry; the remaining entries are the
	// runners-up a wildcard version selector also matched, and are reported only so the selection
	// can be understood. The list is truncated, so it is not necessarily every match.
	// +optional
	Images []OxideImage `json:"images"`
	// Subnets contain selected subnets for launching nodes into
	// +optional
	Subnets []OxideSubnet `json:"subnets"`
	// SSHKeys contain selected ssh keys to copy to launched nodes
	// +optional
	SSHKeys []OxideSSHKey `json:"sshKeys"`
	// EphemeralIPs contain the resolved pool each launched instance takes an external address from,
	// in the order spec.ephemeralIPs declares them.
	// +optional
	EphemeralIPs []OxideEphemeralIP `json:"ephemeralIPs,omitempty"`
	// AffinityGroups contain the resolved groups that every instance launched from this nodeclass
	// joins. It is only published when every selector resolved, so a group that has gone missing
	// holds the previous set rather than silently shrinking it.
	//
	// This list is the desired set: the single source of truth for what a launch joins, what drift
	// compares against, and what memberships are allowed to persist.
	// +optional
	AffinityGroups []OxideAffinityGroup `json:"affinityGroups,omitempty"`
	// Storage is the resolved disk layout of a launched instance: the source each diskSet's disks are
	// created from, and where each one lands in the guest.
	//
	// Published because several of these values are resolved rather than written -- the filesystem
	// when the spec leaves it unset, and the mount point of each disk in a set with a mountDir -- so
	// this is what makes the effective layout readable off the object rather than re-derived. It is
	// also what a launch reads to decide which image or snapshot a data disk is created from, which
	// is why it holds its previous value on an incomplete resolution rather than shrinking.
	// +optional
	Storage *StorageStatus `json:"storage,omitempty"`
	// Bootstrap contains resolved state that only applies to a single bootstrap provider.
	// +optional
	Bootstrap *BootstrapStatus `json:"bootstrap,omitempty"`
	// Conditions contains signals for health and readiness
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
}

// BootstrapStatus holds resolved state that is meaningful to exactly one bootstrap provider.
// Anything both providers understand belongs on OxideNodeClassStatus directly.
type BootstrapStatus struct {
	// +optional
	Kubeadm *KubeadmStatus `json:"kubeadm,omitempty"`
}

// KubeadmStatus is the kubeadm discovery information resolved from kube-public/cluster-info.
type KubeadmStatus struct {
	// CACertHashes pin the cluster CA during token-based discovery, in "sha256:<hex>" form over the
	// CA certificate's SubjectPublicKeyInfo.
	// +optional
	CACertHashes []string `json:"caCertHashes,omitempty"`
	// ClusterName is the cluster name advertised by the cluster-info kubeconfig.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`
}

// StorageStatus is the resolved disk layout every instance launched from this nodeclass gets.
type StorageStatus struct {
	// BootSizeBytes is the boot disk's size as a byte count, rather than as the quantity it was
	// written as.
	// +optional
	BootSizeBytes int64 `json:"bootSizeBytes,omitempty"`
	// DiskSets is the resolved form of spec.storage.diskSets, in the same order.
	// +optional
	DiskSets []DiskSetStatus `json:"diskSets,omitempty"`
}

// DiskSetStatus is one resolved disk set.
//
// It deliberately carries no disk names. A disk's name is minted per-NodeClaim, so it is a property
// of a launch rather than of the nodeclass, and there is nothing about it a reconcile of the
// nodeclass could resolve.
type DiskSetStatus struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Count is how many disks the set has, as resolved after defaulting.
	Count int32 `json:"count"`
	// SizeBytes is the capacity of each disk in the set, not of the set as a whole.
	SizeBytes int64  `json:"sizeBytes"`
	Layout    string `json:"layout"`
	// Filesystem is the effective value: ext4 where the spec left it unset and formatting applies,
	// and empty for a set created from an image or a snapshot, which is never formatted.
	// +optional
	Filesystem string `json:"filesystem,omitempty"`
	// MountPoints is where each of the set's disks is mounted, in ordinal order. It is the resolved
	// answer to mountPath versus mountDir, so nothing has to re-derive it to configure a consumer
	// such as the local static provisioner's hostDir. Empty for a raw block set, which is not
	// mounted.
	// +optional
	MountPoints []string `json:"mountPoints,omitempty"`
	// BlockLinks is where each of a raw block set's devices is linked, in ordinal order. Empty for a
	// set with a filesystem.
	// +optional
	BlockLinks []string `json:"blockLinks,omitempty"`
	// Images and Snapshots are the resolved sources, most recent first and truncated the same way
	// Status.Images is. Only the first is ever used; the rest are reported so a wildcard selector can
	// be understood from the nodeclass alone.
	// +optional
	Images []OxideImage `json:"images,omitempty"`
	// +optional
	Snapshots []OxideSnapshot `json:"snapshots,omitempty"`
}

// OxideSnapshot is one resolved snapshot.
//
// It carries no OS or version, unlike OxideImage, because the Oxide API's snapshot has neither.
type OxideSnapshot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Project is the project the snapshot is in. A snapshot is always in one: there is no silo-level
	// snapshot the way there is a silo-level image.
	Project string `json:"project"`
	// DiskID is the disk the snapshot was taken of, reported so a snapshot can be traced back to
	// where it came from without a second API call.
	// +optional
	DiskID string `json:"diskID,omitempty"`
	// SizeBytes is what makes "this diskSet is smaller than its source" checkable when the nodeclass
	// is reconciled rather than at launch.
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
	// SizeBytes is the image's size, which is what makes "this disk is smaller than the image it is
	// created from" checkable when the nodeclass is reconciled rather than at launch.
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
	// Name is the group's name at the time it resolved. The ID is the identity: a group deleted and
	// recreated under the same name is a different group that the running instances are not members
	// of, and comparing IDs is what makes that visible.
	Name string `json:"name"`
	// Type is Affinity or AntiAffinity, which is also which Oxide endpoint the group is reached on.
	Type string `json:"type"`
	// Policy is what the rack does when the group's constraint cannot be satisfied, allow or fail.
	Policy string `json:"policy"`
	// Managed marks the one group Karpenter owns for this nodeclass, and so the only one it will
	// ever delete.
	Managed bool `json:"managed"`
}

// OxideEphemeralIP is one resolved entry of spec.ephemeralIPs.
type OxideEphemeralIP struct {
	// IPVersion is the resolved address family, taken from the pool when the spec named one.
	IPVersion string `json:"ipVersion"`
	// PoolID and PoolName are empty when the entry takes the silo's default pool: the default is
	// resolved by Nexus at launch, so naming a pool here would record a guess that a change of
	// silo default would silently invalidate.
	// +optional
	PoolID string `json:"poolID,omitempty"`
	// +optional
	PoolName string `json:"poolName,omitempty"`
}

// StatusConditions registers the Ready condition along with the condition types owned by whichever
// bootstrap provider this nodeclass uses. Registering provider conditions conditionally keeps an
// unset provider from ever holding the nodeclass un-Ready.
func (in *OxideNodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	// ImagesResolved applies to every nodeclass regardless of how it bootstraps. Registering at
	// least one dependent unconditionally is also what lets Ready be derived rather than asserted:
	// with no dependents at all, the condition set never recomputes the root.
	conds := []string{ConditionTypeImagesResolved}
	switch in.Spec.Bootstrap.Kind() {
	case BootstrapKindRKE2:
		if in.Spec.Bootstrap.RKE2.TokenRefSecret.Name != "" {
			conds = append(conds, ConditionTypeTokenRefSecretAccessible)
		}
	case BootstrapKindKubeadm:
		conds = append(conds, ConditionTypeClusterInfoAvailable)
	}
	if in.Spec.Storage.NeedsSourceResolution() {
		conds = append(conds, ConditionTypeStorageResolved)
	}
	if len(in.Spec.EphemeralIPs) > 0 {
		conds = append(conds, ConditionTypeEphemeralIPsResolved)
	}
	if in.Spec.Affinity != nil {
		conds = append(
			conds,
			ConditionTypeAffinityGroupsResolved,
			ConditionTypeAffinityPolicySatisfiable,
		)
	}
	return status.NewReadyConditions(conds...).For(in)
}

func (in *OxideNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *OxideNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}
