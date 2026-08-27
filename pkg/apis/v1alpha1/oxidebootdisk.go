package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const BootDiskFinalizer = "karpenter.oxide.computer/boot-disk"

const (
	BootDiskStatePending = "Pending"
	BootDiskStateReady   = "Ready"
)

// OxideBootDisk is one provider-owned boot disk.
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state"
// +kubebuilder:printcolumn:name="NodeClass",type="string",JSONPath=".spec.nodeClassRef.name"
// +kubebuilder:printcolumn:name="NodeClaim",type="string",JSONPath=".spec.claimRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:path=oxidebootdisks,scope=Cluster,categories=karpenter,shortName={oxbd,oxbds}
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
type OxideBootDisk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OxideBootDiskSpec   `json:"spec"`
	Status OxideBootDiskStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.claimRef) || self.claimRef == oldSelf.claimRef",message="claimRef cannot change"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.retire) || !oldSelf.retire || (has(self.retire) && self.retire)",message="retire cannot be cleared"
type OxideBootDiskSpec struct {
	NodeClassRef BootDiskOwnerRef `json:"nodeClassRef"`
	DiskName     string           `json:"diskName"`
	ImageID      string           `json:"imageID"`
	SizeBytes    int64            `json:"sizeBytes"`
	ConfigHash   string           `json:"configHash"`
	// +optional
	ClaimRef *BootDiskOwnerRef `json:"claimRef,omitempty"`
	// +optional
	Retire bool `json:"retire,omitempty"`
}

type BootDiskOwnerRef struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

type OxideBootDiskStatus struct {
	// +optional
	ID string `json:"id,omitempty"`
	// +optional
	State string `json:"state,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
type OxideBootDiskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OxideBootDisk `json:"items"`
}
