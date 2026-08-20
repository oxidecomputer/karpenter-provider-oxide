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
	// IPPools contain selected IP Pools for ephemeral IP attachment
	// +optional
	IPPools []OxideIPPool `json:"ipPools"`
	// AffinityGroups contain the resolved groups that every instance launched from this nodeclass
	// joins. It is only published when every selector resolved, so a group that has gone missing
	// holds the previous set rather than silently shrinking it.
	//
	// This list is the desired set: the single source of truth for what a launch joins, what drift
	// compares against, and what memberships are allowed to persist.
	// +optional
	AffinityGroups []OxideAffinityGroup `json:"affinityGroups,omitempty"`
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

type OxideImage struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	OS          string      `json:"os"`
	Version     string      `json:"version"`
	Project     string      `json:"project"`
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

type OxideIPPool struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IPVersion string `json:"ipVersion"`
	Default   bool   `json:"default"`
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
