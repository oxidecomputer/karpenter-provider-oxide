package v1alpha1

import (
	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	DefaultBootDiskPoolMinReady   = int32(2)
	DefaultBootDiskPoolPercentage = int32(10)
	DefaultBootDiskPoolMaxReady   = int32(20)
)

// ConditionTypeNodeClassResolved reports whether the pool's nodeclass exists and can supply a boot disk config.
const ConditionTypeNodeClassResolved = "NodeClassResolved"

// Reasons recorded on ConditionTypeNodeClassResolved.
const (
	// ConditionReasonNodeClassNotFound is a nodeClassRef naming no OxideNodeClass.
	ConditionReasonNodeClassNotFound = "NodeClassNotFound"
	// ConditionReasonNodeClassNotReady is a nodeclass without a resolved boot image or size.
	ConditionReasonNodeClassNotReady = "NodeClassNotReady"
	// ConditionReasonNodeClassClaimed is a nodeclass already pooled by an older OxideBootDiskPool.
	ConditionReasonNodeClassClaimed = "NodeClassClaimed"
)

// OxideBootDiskPool keeps unused boot disks ready for one OxideNodeClass so launches skip the disk create.
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="NodeClass",type="string",JSONPath=".spec.nodeClassRef.name"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".status.desired"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Claimed",type="integer",JSONPath=".status.claimed",priority=1
// +kubebuilder:printcolumn:name="Stale",type="integer",JSONPath=".status.stale",priority=1
// +kubebuilder:printcolumn:name="Resolved",type="string",JSONPath=".status.conditions[?(@.type==\"NodeClassResolved\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:path=oxidebootdiskpools,scope=Cluster,categories=karpenter,shortName={oxbdp,oxbdps}
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
type OxideBootDiskPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OxideBootDiskPoolSpec   `json:"spec"`
	Status OxideBootDiskPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.minReady) || !has(self.maxReady) || self.minReady <= self.maxReady",message="minReady must not exceed maxReady"
type OxideBootDiskPoolSpec struct {
	// NodeClassRef is the OxideNodeClass whose boot image and size the pooled disks are built from.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="nodeClassRef is immutable"
	NodeClassRef NodeClassReference `json:"nodeClassRef"`
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

// NodeClassReference names an OxideNodeClass.
type NodeClassReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type OxideBootDiskPoolStatus struct {
	// +optional
	Desired int32 `json:"desired"`
	// +optional
	Ready int32 `json:"ready"`
	// +optional
	Creating int32 `json:"creating"`
	// +optional
	Claimed int32 `json:"claimed"`
	// +optional
	Stale int32 `json:"stale"`
	// ConfigHash identifies the boot image and size the ready disks were built from.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type OxideBootDiskPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OxideBootDiskPool `json:"items"`
}

func (in OxideBootDiskPoolSpec) EffectiveMinReady() int32 {
	if in.MinReady != nil {
		return *in.MinReady
	}
	return DefaultBootDiskPoolMinReady
}

func (in OxideBootDiskPoolSpec) EffectivePercentage() int32 {
	if in.Percentage != nil {
		return *in.Percentage
	}
	return DefaultBootDiskPoolPercentage
}

func (in OxideBootDiskPoolSpec) EffectiveMaxReady() int32 {
	if in.MaxReady != nil {
		return *in.MaxReady
	}
	return DefaultBootDiskPoolMaxReady
}

func (in *OxideBootDiskPool) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions(ConditionTypeNodeClassResolved).For(in, opts...)
}

func (in *OxideBootDiskPool) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *OxideBootDiskPool) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}
