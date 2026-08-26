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

// Reasons an UnavailableRange can be out.
const (
	// RangeReasonInsufficientCapacity is a launch the rack refused for want of room.
	RangeReasonInsufficientCapacity = "InsufficientCapacity"
	// RangeReasonQuotaExceeded is a launch the silo was not entitled to make.
	RangeReasonQuotaExceeded = "QuotaExceeded"
	// RangeReasonNoSledWithRoom is what no sled currently has room for.
	RangeReasonNoSledWithRoom = "NoSledWithRoom"
)

// States a shape can be in while it is out of consideration.
const (
	// ShapeStateBackoff is a shape whose last launch was refused and whose wait has not elapsed.
	ShapeStateBackoff = "Backoff"
	// ShapeStateHalfOpen is a shape whose wait has elapsed, or that something has made newly plausible.
	ShapeStateHalfOpen = "HalfOpen"
)

// OxideCapacity reports which instance shapes the provider will not currently launch, and why.
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Unavailable",type="integer",JSONPath=".status.instanceTypes.unavailableCount",description="Instance types the scheduler is not currently offered"
// +kubebuilder:printcolumn:name="Total",type="integer",JSONPath=".status.instanceTypes.count",priority=1,description="Instance types the provider offers in total"
// +kubebuilder:printcolumn:name="Ranges",type="integer",JSONPath=".status.instanceTypes.unavailableRangeCount",priority=1,description="Regions of the catalog that are out, each covering many instance types"
// +kubebuilder:printcolumn:name="Largest",type="string",JSONPath=".status.inventory.largestFeasibleInstanceType",description="Largest shape the rack can currently place"
// +kubebuilder:printcolumn:name="Inventory",type="string",JSONPath=".status.inventory.state",description=""
// +kubebuilder:printcolumn:name="Storage",type="string",JSONPath=".status.storage.reason",description="Why the last boot disk could not be provisioned, if one could not"
// +kubebuilder:printcolumn:name="Updated",type="date",JSONPath=".status.observedAt",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",priority=1,description=""
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
	// InstanceTypes is what the scheduler is and is not being offered, and why.
	// +optional
	InstanceTypes InstanceTypeAvailability `json:"instanceTypes,omitempty"`
	// Inventory is what the rack was last read to have room for, and how much that reading is worth.
	// +optional
	Inventory InventoryStatus `json:"inventory,omitempty"`
	// Quota is the silo's remaining entitlement.
	// +optional
	Quota *QuotaHeadroom `json:"quota,omitempty"`
	// Storage is the last launch refused because its boot disk could not be provisioned.
	// +optional
	Storage *StorageRefusal `json:"storage,omitempty"`
}

// InstanceTypeAvailability is how much of the catalog the scheduler is being offered.
type InstanceTypeAvailability struct {
	// Count is how many instance types the provider offers in total, which is the denominator for UnavailableCount.
	// +optional
	Count int `json:"count"`
	// UnavailableCount is how many instance types the scheduler is currently not being offered, out of Count.
	// +optional
	UnavailableCount int `json:"unavailableCount"`
	// UnavailableRangeCount is how many entries UnavailableRanges holds.
	// +optional
	UnavailableRangeCount int `json:"unavailableRangeCount"`
	// UnavailableRanges is every region of the catalog the provider is not currently offering.
	// +optional
	UnavailableRanges []UnavailableRange `json:"unavailableRanges,omitempty"`
}

// StorageRefusal is the most recent launch refused for want of boot disk storage.
type StorageRefusal struct {
	// Reason is which limit was diagnosed as binding: QuotaExceeded for the silo's storage entitlement.
	Reason string `json:"reason"`
	// Message is the diagnosis as it was reported on the NodeClaim that was refused.
	// +optional
	Message string `json:"message,omitempty"`
	// ConsecutiveRefusals is how many launches in a row have been refused this way.
	ConsecutiveRefusals int `json:"consecutiveRefusals"`
	// LastRefusal is when the most recent one happened.
	// +optional
	LastRefusal *metav1.Time `json:"lastRefusal,omitempty"`
}

// UnavailableRange is a region of the instance type catalog the provider will not currently launch from.
type UnavailableRange struct {
	// Range is what is out, in the form an operator reads: ">=11 vCPUs and >=22 GiB" for a refusal.
	Range string `json:"range"`
	// FromNcpus and FromMemoryGiB are the lower corner of a refusal range.
	// +optional
	FromNcpus int `json:"fromNcpus,omitempty"`
	// +optional
	FromMemoryGiB int `json:"fromMemoryGiB,omitempty"`
	// MaxNcpus and MaxMemoryGiB are the headroom of an inventory range.
	// +optional
	MaxNcpus int `json:"maxNcpus,omitempty"`
	// +optional
	MaxMemoryGiB int `json:"maxMemoryGiB,omitempty"`
	// RefusedInstanceType is the shape whose launch was actually refused, which is the evidence the range rests on.
	// +optional
	RefusedInstanceType string `json:"refusedInstanceType,omitempty"`
	// LargestLaunchable is the largest shape known to work within this range.
	// +optional
	LargestLaunchable string `json:"largestLaunchable,omitempty"`
	// Reason is what put the range out: InsufficientCapacity for a launch the rack refused for want of room.
	Reason string `json:"reason"`
	// State is Backoff while the range is out of consideration entirely.
	// +optional
	State string `json:"state,omitempty"`
	// ConsecutiveRefusals is how many times in a row a launch in this range has been refused.
	// +optional
	ConsecutiveRefusals int `json:"consecutiveRefusals,omitempty"`
	// LastRefusal is when a launch in this range was most recently refused.
	// +optional
	LastRefusal *metav1.Time `json:"lastRefusal,omitempty"`
	// RetryAfter is when the shape will be offered again for a trial launch.
	// +optional
	RetryAfter *metav1.Time `json:"retryAfter,omitempty"`
}

// InventoryStatus is what the rack had room for when it was last read, and how much that reading is worth.
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
