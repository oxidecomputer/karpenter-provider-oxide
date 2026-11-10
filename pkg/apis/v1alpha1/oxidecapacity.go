/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OxideCapacityName is the name of the single OxideCapacity the controller publishes.
const OxideCapacityName = "rack"

// Values of InventoryStatus.State.
const (
	// InventoryDisabled is sled inventory not being switched on.
	InventoryDisabled = "Disabled"
	// InventoryUnknown is enabled inventory without a successful reading.
	InventoryUnknown = "Unknown"
	// InventoryStale is a reading that has not been refreshed recently enough to be believed.
	InventoryStale = "Stale"
	// InventoryFresh is a current, trusted reading.
	InventoryFresh = "Fresh"
)

const (
	CapacityScopeGlobal    = "Global"
	CapacityScopePlacement = "Placement"

	BackoffWaiting       = "Backoff"
	BackoffProbeEligible = "ProbeEligible"
	BackoffProbing       = "Probing"

	QuotaUnknown = "Unknown"
	QuotaFresh   = "Fresh"
	QuotaStale   = "Stale"
)

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="GlobalUnavailable",type="integer",JSONPath=".status.globalInstanceTypes.unavailableCount"
// +kubebuilder:printcolumn:name="GlobalTotal",type="integer",JSONPath=".status.globalInstanceTypes.count",priority=1
// +kubebuilder:printcolumn:name="Inventory",type="string",JSONPath=".status.inventory.state"
// +kubebuilder:printcolumn:name="Quota",type="string",JSONPath=".status.quota.state"
// +kubebuilder:printcolumn:name="LastStorageFailure",type="string",JSONPath=".status.history.storage.reason",priority=1
// +kubebuilder:printcolumn:name="Updated",type="date",JSONPath=".status.observedAt"
// +kubebuilder:resource:path=oxidecapacities,scope=Cluster,categories=karpenter,shortName={oxcap,oxcaps}
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
type OxideCapacity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Status OxideCapacityStatus `json:"status,omitempty"`
}

// OxideCapacityStatus is what the provider currently believes about capacity.
type OxideCapacityStatus struct {
	// ObservedAt is when this view was last published.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// +optional
	GlobalInstanceTypes GlobalInstanceTypeSummary `json:"globalInstanceTypes,omitempty"`
	// Inventory is the latest rack capacity snapshot and its inventory state.
	// +optional
	Inventory InventoryStatus `json:"inventory,omitempty"`
	// Quota is the silo's remaining entitlement.
	// +optional
	Quota QuotaStatus `json:"quota,omitempty"`
	// +optional
	// +listType=atomic
	LaunchBackoffs []LaunchBackoff `json:"launchBackoffs,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	OmittedLaunchBackoffCount int `json:"omittedLaunchBackoffCount,omitempty"`
	// +optional
	History *CapacityHistory `json:"history,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.unavailableCount <= self.count"
type GlobalInstanceTypeSummary struct {
	// +kubebuilder:validation:Minimum=0
	Count int `json:"count"`
	// +kubebuilder:validation:Minimum=0
	UnavailableCount int `json:"unavailableCount"`
}

type CapacityShape struct {
	// +kubebuilder:validation:Minimum=1
	CPUs int `json:"cpus"`
	// +kubebuilder:validation:Minimum=1
	MemoryGiB int `json:"memoryGiB"`
}

// +kubebuilder:validation:XValidation:rule="self.type == 'Placement' ? has(self.placement) && size(self.placement) > 0 && !has(self.cpuPlatforms) : !has(self.placement)"
type CapacityScope struct {
	// +kubebuilder:validation:Enum=Global;Placement
	Type string `json:"type"`
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	CPUPlatforms []string `json:"cpuPlatforms,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	// +listMapKey=id
	Placement []CapacityPlacementConstraint `json:"placement,omitempty"`
}

type CapacityPlacementConstraint struct {
	// +kubebuilder:validation:Enum=Affinity;AntiAffinity
	Type string `json:"type"`
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`
}

// +kubebuilder:validation:XValidation:rule="(self.state == 'Backoff') == has(self.retryAfter)"
type LaunchBackoff struct {
	Scope            CapacityScope `json:"scope"`
	MinimumShape     CapacityShape `json:"minimumShape"`
	LastRefusedShape CapacityShape `json:"lastRefusedShape"`
	// +optional
	KnownLaunchableShape *CapacityShape `json:"knownLaunchableShape,omitempty"`
	// +kubebuilder:validation:Enum=Backoff;ProbeEligible;Probing
	State string `json:"state"`
	// +kubebuilder:validation:MinLength=1
	Reason string `json:"reason"`
	// +kubebuilder:validation:Minimum=1
	ConsecutiveRefusals int `json:"consecutiveRefusals"`
	// +optional
	LastRefusal *metav1.Time `json:"lastRefusal,omitempty"`
	// +optional
	RetryAfter *metav1.Time `json:"retryAfter,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
	// +optional
	MessageTruncated bool `json:"messageTruncated,omitempty"`
}

type CapacityHistory struct {
	// +optional
	Storage *StorageRefusal `json:"storage,omitempty"`
	// +optional
	// +listType=atomic
	Placement []PlacementFailure `json:"placement,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	OmittedPlacementCount int `json:"omittedPlacementCount,omitempty"`
}

type PlacementFailure struct {
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=type
	// +listMapKey=id
	Placement []CapacityPlacementConstraint `json:"placement"`
	Shape     CapacityShape                 `json:"shape"`
	// +optional
	LastRefusal *metav1.Time `json:"lastRefusal,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
	// +optional
	MessageTruncated bool `json:"messageTruncated,omitempty"`
}

// StorageRefusal is the most recent launch refused for want of boot disk storage.
type StorageRefusal struct {
	// Reason is which limit was diagnosed as binding: QuotaExceeded for the silo's storage entitlement.
	Reason string `json:"reason"`
	// Message is the diagnosis as it was reported on the NodeClaim that was refused.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
	// +optional
	MessageTruncated bool `json:"messageTruncated,omitempty"`
	// ConsecutiveRefusals is how many launches in a row have been refused this way.
	ConsecutiveRefusals int `json:"consecutiveRefusals"`
	// LastRefusal is when the most recent one happened.
	// +optional
	LastRefusal *metav1.Time `json:"lastRefusal,omitempty"`
}

// InventoryStatus contains the latest rack capacity snapshot and its inventory state.
type InventoryStatus struct {
	// State is what the reading is contributing: Disabled, Unknown, Stale or Fresh.
	// +optional
	State string `json:"state,omitempty"`
	// ObservedAt is when the reading was taken, which is not when this view was published.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// LargestFeasibleInstanceType is the largest shape any single sled could take, as an instance type name.
	// +optional
	LargestFeasibleInstanceType string `json:"largestFeasibleInstanceType,omitempty"`
	// Sleds is the per-sled headroom the reading is made of.
	// +optional
	Sleds []SledCapacity `json:"sleds,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	OmittedSledCount int `json:"omittedSledCount,omitempty"`
}

// SledCapacity is one sled's remaining headroom.
type SledCapacity struct {
	// ID is the Oxide sled ID.
	ID string `json:"id"`
	// Provisionable is whether the sled will accept a new instance at all.
	Provisionable bool `json:"provisionable"`
	// FreeCPUs and FreeMemoryGiB are what is left on the sled for instances.
	FreeCPUs      int `json:"freeCPUs"`
	FreeMemoryGiB int `json:"freeMemoryGiB"`
	// LargestFeasibleInstanceType is the largest shape this sled could take.
	// +optional
	LargestFeasibleInstanceType string `json:"largestFeasibleInstanceType,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.state == 'Unknown' ? !has(self.remaining) && !has(self.observedAt) : has(self.remaining) && has(self.observedAt)"
type QuotaStatus struct {
	// +kubebuilder:validation:Enum=Unknown;Fresh;Stale
	State string `json:"state"`
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// +optional
	Remaining *QuotaHeadroom `json:"remaining,omitempty"`
}

// QuotaHeadroom is what the silo is still entitled to launch.
type QuotaHeadroom struct {
	// CPUs, MemoryGiB and StorageGiB are the quota's capacity less what the silo has provisioned.
	CPUs       int64 `json:"cpus"`
	MemoryGiB  int64 `json:"memoryGiB"`
	StorageGiB int64 `json:"storageGiB"`
}

// OxideCapacityList contains OxideCapacities.
// +kubebuilder:object:root=true
type OxideCapacityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OxideCapacity `json:"items"`
}
