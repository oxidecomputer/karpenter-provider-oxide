package v1alpha1

import (
	"fmt"

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
	ImageSelector []ImageSelector `json:"imageSelector,omitempty"`
	// BootDiskSize is the stoage capcity of the boot disk
	BootDiskSize resource.Quantity `json:"bootDiskSize,omitempty"`
	// SubnetSelector is used to select subnets to launch nodes into.
	// Multiple subnets can match. Node launches will use subnets with available IP space.
	SubnetSelector []SubnetSelector `json:"subnetSelector,omitempty"`
	// SSHKeySelector is used to select SSH keys that will be copied to the node
	// +optional
	SSHKeySelector []SSHKeySelector `json:"sshKeySelector,omitempty"`
	// IPPoolSelector is used to select IP Pools to allocate ephemeral IPs from
	// +optional
	IPPoolSelector []IPPoolSelector `json:"ipPoolSelector,omitempty"`
	// Affinity controls which Oxide affinity and anti-affinity groups launched instances join.
	// +optional
	Affinity *Affinity `json:"affinity,omitempty"`
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
const OxideNodeClassHashVersion = "v0"

func (in *OxideNodeClass) Hash(caBundle *string) string {
	return fmt.Sprint(lo.Must(hashstructure.Hash([]any{
		in.Spec,
		lo.FromPtr(caBundle),
	}, hashstructure.FormatV2, &hashstructure.HashOptions{
		SlicesAsSets:    true,
		IgnoreZeroValue: true,
		ZeroNil:         true,
	})))
}

// OxideNodeClassList contains a list of OxideNodeClass
// +kubebuilder:object:root=true
type OxideNodeClassList struct {
	metav1.TypeMeta `                 json:",inline"`
	metav1.ListMeta `                 json:"metadata,omitempty"`
	Items           []OxideNodeClass `json:"items"`
}
