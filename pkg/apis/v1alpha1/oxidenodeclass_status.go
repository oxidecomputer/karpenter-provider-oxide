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

type OxideIPPool struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IPVersion string `json:"ipVersion"`
	Default   bool   `json:"default"`
}

// StatusConditions registers the Ready condition along with the condition types owned by whichever
// bootstrap provider this nodeclass uses. Registering provider conditions conditionally keeps an
// unset provider from ever holding the nodeclass un-Ready.
func (in *OxideNodeClass) StatusConditions() status.ConditionSet {
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
	return status.NewReadyConditions(conds...).For(in)
}

func (in *OxideNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *OxideNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}
