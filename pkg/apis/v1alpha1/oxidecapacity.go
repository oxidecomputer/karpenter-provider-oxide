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

// OxideCapacityName is the name of the single OxideCapacity the controller publishes. There is one
// rack and one silo behind a given installation, so there is nothing to distinguish a second one by.
const OxideCapacityName = "rack"

// Values of InventoryStatus.State.
const (
	// InventoryDisabled is sled inventory not being switched on. Nothing is known about what the rack
	// has room for, and shapes are ruled out only by what launches the rack has refused.
	InventoryDisabled = "Disabled"
	// InventoryUnknown is sled inventory being enabled but never yet read successfully, which is
	// almost always a token without fleet-level access to the system API.
	InventoryUnknown = "Unknown"
	// InventoryStale is a reading that has not been refreshed recently enough to be believed. It is
	// ignored rather than trusted, so provisioning falls back to what the rack refuses.
	InventoryStale = "Stale"
	// InventoryFresh is a current reading, which is the only state in which "no sled has room for this
	// shape" rules a shape out.
	InventoryFresh = "Fresh"
)

// Reasons an UnavailableRange can be out.
const (
	// RangeReasonInsufficientCapacity is a launch the rack refused for want of room. It is evidence
	// from an attempt, so it names the shape that was refused and carries a backoff.
	RangeReasonInsufficientCapacity = "InsufficientCapacity"
	// RangeReasonQuotaExceeded is a launch the silo was not entitled to make.
	RangeReasonQuotaExceeded = "QuotaExceeded"
	// RangeReasonNoSledWithRoom is what no sled currently has room for, read off the sled inventory
	// rather than learned from a refusal.
	//
	// It is spelled differently from InsufficientCapacity on purpose. Nothing was attempted and
	// nothing is waiting: there is no shape to name, no refusal count and no retry, and the range
	// disappears the moment the rack has room again rather than when a timer elapses. It is also the
	// only reason that can rule out most of the catalog while nothing has been refused at all, which
	// is the state an operator is in when the unavailable count is in the hundreds and every other
	// field says nothing is wrong.
	//
	// It is also the only reason that describes itself with MaxNcpus and MaxMemoryGiB rather than a
	// corner, because it is a sled's headroom rather than a threshold. See UnavailableRange.
	RangeReasonNoSledWithRoom = "NoSledWithRoom"
)

// States a shape can be in while it is out of consideration.
const (
	// ShapeStateBackoff is a shape whose last launch was refused and whose wait has not elapsed. It is
	// not offered to the scheduler at all.
	ShapeStateBackoff = "Backoff"
	// ShapeStateHalfOpen is a shape whose wait has elapsed, or that something has made newly
	// plausible. It is offered again, but only one launch at a time is allowed to test it.
	ShapeStateHalfOpen = "HalfOpen"
)

// OxideCapacity reports which instance shapes the provider will not currently launch, and why.
//
// It is a read-only view of state the controller holds in memory, published so that an operator
// wondering why a pod is not scheduling can ask a question instead of reading logs: which shapes are
// out, whether that is the rack or the silo's quota, and when each one will be tried again. It has no
// spec, and nothing reads it back. Deleting it is harmless; it is republished.
//
// The view is not durable and does not try to be. After a restart the controller has no refusal
// history, so this reports none, and it fills in again as launches are refused and the rack is read.
// That direction is deliberate: an empty view offers every shape, which costs a refused launch,
// whereas a view restored from disk could suppress shapes on evidence nobody can check anymore.
//
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
	// ObservedAt is when this view was last published. It is refreshed whenever anything in it changes
	// and periodically otherwise, so a timestamp that has stopped moving means the controller has
	// stopped publishing rather than that nothing is happening.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// InstanceTypes is what the scheduler is and is not being offered, and why. It is the conclusion:
	// everything that rules a shape out ends up counted here, whatever the evidence was.
	//
	// It is one field rather than several at the top level so that the whole answer to "what is out"
	// stays together, and stays above the per-sled rack reading, which is long enough to push anything
	// after it off the screen. Keys serialize alphabetically, so this is ordering by name.
	// +optional
	InstanceTypes InstanceTypeAvailability `json:"instanceTypes,omitempty"`
	// Inventory is what the rack was last read to have room for, and how much that reading is worth.
	// It is the evidence behind whichever of InstanceTypes.UnavailableRanges say NoSledWithRoom, and
	// is grouped rather than spread across the top level so the reading, its freshness and its age
	// cannot be read apart from each other.
	// +optional
	Inventory InventoryStatus `json:"inventory,omitempty"`
	// Quota is the silo's remaining entitlement. It is absent when quota awareness is disabled or the
	// quota has never been read.
	//
	// It is not part of Inventory. Inventory is what the rack physically has room for; this is what
	// the silo is administratively allowed to ask for, and either can bind while the other has plenty.
	// +optional
	Quota *QuotaHeadroom `json:"quota,omitempty"`
	// Storage is the last launch refused because its boot disk could not be provisioned, and is absent
	// once a launch has provisioned one.
	//
	// It is reported here rather than in UnavailableShapes because it is not about a shape. Boot disk
	// size comes from the OxideNodeClass, and a disk's replicas are placed on three sleds chosen
	// independently of wherever the instance lands, so no shape was the constraint and no smaller one
	// would have fared better. An operator seeing every shape available and nothing scaling up is
	// looking for exactly this field.
	// +optional
	Storage *StorageRefusal `json:"storage,omitempty"`
}

// InstanceTypeAvailability is how much of the catalog the scheduler is being offered, and the
// boundaries of what it is not.
type InstanceTypeAvailability struct {
	// Count is how many instance types the provider offers in total, which is the denominator for
	// UnavailableCount. It is fixed for a given build.
	// +optional
	Count int `json:"count"`
	// UnavailableCount is how many instance types the scheduler is currently not being offered, out of
	// Count.
	//
	// This is what "how much is unavailable" usually means, and it counts everything that rules a shape
	// out: refusal history, the silo's quota, and the rack having no sled with room. Each of those
	// covers every shape at least as large, so this climbs into the hundreds while UnavailableRanges
	// holds one or two entries.
	// +optional
	UnavailableCount int `json:"unavailableCount"`
	// UnavailableRangeCount is how many entries UnavailableRanges holds. It exists because a table
	// column cannot count a list, and carries no information the list does not.
	//
	// It is a count of regions, not of instance types. Each one covers many shapes, so this being 1 is
	// compatible with almost nothing being launchable; the number that answers that question is
	// UnavailableCount.
	// +optional
	UnavailableRangeCount int `json:"unavailableRangeCount"`
	// UnavailableRanges is every region of the catalog the provider is not currently offering, whether
	// because a launch from it was refused or because no sled has room for it.
	//
	// Each entry covers many instance types: a refusal rules out every shape at least as large as the
	// one refused, and a sled's headroom rules out every shape that outgrows it. So the list is the
	// boundary of what is out, not an enumeration of it, and UnavailableCount is what that boundary
	// costs.
	// +optional
	UnavailableRanges []UnavailableRange `json:"unavailableRanges,omitempty"`
}

// StorageRefusal is the most recent launch refused for want of boot disk storage.
//
// Unlike an UnavailableShape this suppresses nothing: no shape is taken out of consideration for it
// and no retry is scheduled from it. It is a report, and the provider goes on discovering storage the
// only way it can, by trying to create a disk.
type StorageRefusal struct {
	// Reason is which limit was diagnosed as binding: QuotaExceeded for the silo's storage entitlement,
	// or InsufficientCapacity for the rack having nowhere to place the disk. As with shapes, the Oxide
	// API reports both identically and this is the provider's diagnosis.
	Reason string `json:"reason"`
	// Message is the diagnosis as it was reported on the NodeClaim that was refused.
	// +optional
	Message string `json:"message,omitempty"`
	// ConsecutiveRefusals is how many launches in a row have been refused this way. Any launch that
	// succeeds clears the whole record, since every launch provisions a boot disk.
	ConsecutiveRefusals int `json:"consecutiveRefusals"`
	// LastRefusal is when the most recent one happened.
	// +optional
	LastRefusal *metav1.Time `json:"lastRefusal,omitempty"`
}

// UnavailableRange is a region of the instance type catalog the provider will not currently launch
// from, and what it would take to change that.
//
// It is deliberately not a shape. Naming one instance type reads as that type being unavailable,
// which is never what an entry means: a refusal of 11 CPUs and 22 GiB takes every shape at least
// that large out with it, so one entry is hundreds of instance types and reporting it as "11-22"
// hides all of them. What is published is the boundary instead.
//
// A range comes from one of two places, which Reason says which of, and they are bounded from
// opposite ends. Read Range first; the numeric fields are that sentence in a form a program can
// apply, and which pair is filled in follows from Reason.
//
//   - A refusal is evidence from a launch that was actually attempted. It is bounded from below by
//     FromNcpus and FromMemoryGiB, and carries the shape that was refused, a refusal count and a
//     retry.
//   - Sled inventory is a reading of what the rack has room for. It is bounded from above by
//     MaxNcpus and MaxMemoryGiB, and carries none of those: nothing was attempted, so there is no
//     shape to name and nothing is waiting on a clock. The fields only a refusal can fill are
//     omitted rather than zeroed.
//
// The lower bound is an AND of both dimensions and the upper bound is an OR, which is not an
// inconsistency but the same fact stated from each side. A limit that refuses 11 CPUs and 22 GiB
// refuses shapes asking for at least that much of *both*, so ">=11 vCPUs and >=22 GiB" leaves 16 CPUs
// with 16 GiB perfectly available. A sled with 128 CPUs and 880 GiB free takes a shape only if it fits
// in *both*, so anything wanting more of *either* is out. That is why an inventory range is one entry
// naming both numbers rather than a CPU entry and a memory entry: outgrowing a sled in one dimension
// is already enough, and splitting it in two invited reading each half as a separate constraint.
type UnavailableRange struct {
	// Range is what is out, in the form an operator reads: ">=11 vCPUs and >=22 GiB" for a refusal,
	// ">128 vCPUs or >880 GiB" for a sled's headroom.
	Range string `json:"range"`
	// FromNcpus and FromMemoryGiB are the lower corner of a refusal range, so the rule can be applied
	// without parsing Range. A shape is out when it asks for at least this many CPUs *and* at least
	// this much memory.
	//
	// Either may be zero, which is that dimension not being part of the threshold. Zero is not a shape
	// size and nothing asks for it. Both are zero on a range that came from sled inventory, which is
	// bounded by MaxNcpus and MaxMemoryGiB instead.
	// +optional
	FromNcpus int `json:"fromNcpus,omitempty"`
	// +optional
	FromMemoryGiB int `json:"fromMemoryGiB,omitempty"`
	// MaxNcpus and MaxMemoryGiB are the headroom of an inventory range: the largest shape that still
	// fits, so a shape is out when it wants more CPUs *or* more memory than this.
	//
	// Both being zero is no sled having room for anything at all, which rules out the whole catalog.
	// Either being at or above the largest the catalog offers in that dimension is that dimension not
	// constraining anything, which Range says as "any".
	//
	// When more than one inventory range is published the rack has sleds with genuinely different
	// headroom, and a shape is out only if it outgrows every one of them, since it needs to fit on just
	// one sled. A rack of like sleds publishes a single entry, where that distinction does not arise.
	//
	// Both are absent on a refusal range, which is bounded from below instead.
	// +optional
	MaxNcpus int `json:"maxNcpus,omitempty"`
	// +optional
	MaxMemoryGiB int `json:"maxMemoryGiB,omitempty"`
	// RefusedInstanceType is the shape whose launch was actually refused, which is the evidence the
	// range rests on. It can be larger than the corner above: once a run of refusals establishes that
	// the scheduler is walking down through sizes, the provider stops offering shapes well below the
	// last one refused rather than testing every size on the way down, and the range widens to say so.
	// When they differ, everything between the corner and this shape is skipped rather than refused.
	//
	// It is absent on a range that came from sled inventory, where no launch was attempted.
	// +optional
	RefusedInstanceType string `json:"refusedInstanceType,omitempty"`
	// LargestLaunchable is the biggest shape known to be launchable while this range stands, and is
	// absent until there is one. It is the other end of the search: the range covers what is above it,
	// so a range with a launchable shape just below its boundary has found that boundary. On a refusal
	// range it is the biggest shape that has launched; on an inventory range it is the biggest shape
	// the rack currently has room for.
	// +optional
	LargestLaunchable string `json:"largestLaunchable,omitempty"`
	// Reason is what put the range out: InsufficientCapacity for a launch the rack refused for want of
	// room, QuotaExceeded for one the silo was not entitled to, or NoSledWithRoom for a region sled
	// inventory says no sled can take. The Oxide API reports the first two identically, so telling
	// those apart is the provider's diagnosis rather than something it was told.
	Reason string `json:"reason"`
	// State is Backoff while the range is out of consideration entirely, or HalfOpen once the refused
	// shape is being offered again for a single trial launch.
	//
	// It is absent on a range that came from sled inventory. Those have no state to be in: they are not
	// waiting to be retried, and they end when the rack has room rather than when a wait elapses.
	// +optional
	State string `json:"state,omitempty"`
	// ConsecutiveRefusals is how many times in a row a launch in this range has been refused. It is
	// what the wait is computed from, and a launch that succeeds clears it. It is absent on a range
	// that came from sled inventory, which has refused nothing.
	// +optional
	ConsecutiveRefusals int `json:"consecutiveRefusals,omitempty"`
	// LastRefusal is when a launch in this range was most recently refused.
	// +optional
	LastRefusal *metav1.Time `json:"lastRefusal,omitempty"`
	// RetryAfter is when the shape will be offered again for a trial launch. It is absent once that
	// time has arrived and the shape is HalfOpen, which is to say it is being offered now.
	//
	// It is a ceiling on the wait rather than a forecast. Only the clock is predictable here, and every
	// other thing that moves a shape moves it sooner: a successful launch clears the shape immediately,
	// and capacity coming back or the rack gaining room reopens a backed off shape ahead of it. A
	// refusal is the one event that moves it out, by restarting a longer backoff.
	// +optional
	RetryAfter *metav1.Time `json:"retryAfter,omitempty"`
}

// InventoryStatus is what the rack had room for when it was last read, and how much that reading is
// worth. Everything read off the rack lives here, so a reading and its own freshness cannot be found
// at different levels of the object and believed independently.
type InventoryStatus struct {
	// State is what the reading is contributing: Disabled, Unknown, Stale or Fresh. Only a Fresh
	// reading is allowed to rule a shape out, so the rest of this is a report rather than a constraint
	// when State is anything else.
	// +optional
	State string `json:"state,omitempty"`
	// ObservedAt is when the reading was taken, which is not when this view was published. The gap
	// between this and the status-level ObservedAt is what State is derived from.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// LargestFeasibleInstanceType is the largest shape any single sled could take, as an instance type
	// name. It is empty when nothing has been read, which is not the same as nothing fitting; that is
	// what State next to it distinguishes.
	// +optional
	LargestFeasibleInstanceType string `json:"largestFeasibleInstanceType,omitempty"`
	// Sleds is the per-sled headroom the reading is made of. This is reported per sled rather than
	// summed because that is how placement works: an instance runs on one sled, so a rack with 4 CPUs
	// free on each of two sleds cannot take an 8 CPU node.
	// +optional
	Sleds []SledCapacity `json:"sleds,omitempty"`
}

// SledCapacity is one sled's remaining headroom.
type SledCapacity struct {
	// ID is the Oxide sled ID.
	ID string `json:"id"`
	// Provisionable is whether the sled will accept a new instance at all. A sled being drained or
	// expunged reports its free resources and still contributes no capacity.
	Provisionable bool `json:"provisionable"`
	// FreeCPUs and FreeMemoryGiB are what is left on the sled for instances.
	//
	// Memory is headroom in the sled's VMM reservoir, which is where guest memory is allocated from,
	// rather than in its physical RAM: the host OS and the control plane are paid for first. The
	// reservoir is not on this API, so it is estimated as a fixed percentage of physical RAM.
	//
	// Both remain upper bounds. The estimate errs high, and the control plane holds back resources this
	// API does not expose at all, so the real headroom is smaller. Overstating it costs a refused
	// launch, which the provider recovers from; understating it would suppress a shape the rack could
	// have placed, which nothing would ever discover.
	FreeCPUs      int `json:"freeCPUs"`
	FreeMemoryGiB int `json:"freeMemoryGiB"`
	// LargestFeasibleInstanceType is the largest shape this sled could take.
	// +optional
	LargestFeasibleInstanceType string `json:"largestFeasibleInstanceType,omitempty"`
}

// QuotaHeadroom is what the silo is still entitled to launch.
type QuotaHeadroom struct {
	// CPUs, MemoryGiB and StorageGiB are the quota's capacity less what the silo has provisioned.
	// Storage is included because it refuses launches too, even though it is not a property of a shape:
	// boot disk size comes from the OxideNodeClass, so it is checked at launch time instead.
	CPUs       int64 `json:"cpus"`
	MemoryGiB  int64 `json:"memoryGiB"`
	StorageGiB int64 `json:"storageGiB"`
}

// OxideCapacityList contains a list of OxideCapacity
// +kubebuilder:object:root=true
type OxideCapacityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OxideCapacity `json:"items"`
}
