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

// Package metrics holds every Prometheus series this provider publishes.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	namespace = "oxide_karpenter"

	capacitySubsystem  = "capacity"
	launchSubsystem    = "launch"
	reaperSubsystem    = "reaper"
	bootDiskSubsystem  = "boot_disk_pool"
	inventorySubsystem = "inventory"
	providerSubsystem  = "provider"

	instanceTypeLabel = "instance_type"

	kindLabel = "kind"

	stateLabel = "state"

	sledLabel = "sled"

	reasonLabel = "reason"
	resultLabel = "result"

	providerLabel     = "provider"
	nodeClassLabel    = "nodeclass"
	operationLabel    = "operation"
	resourceKindLabel = "resource_kind"
)

// Results of trying to reserve the single trial launch a reopened shape allows.
const (
	// ProbeAcquired is the launch that got the reservation and is testing the shape.
	ProbeAcquired = "acquired"
	// ProbeRejected is a launch turned away because another one is already testing the shape.
	ProbeRejected = "rejected"
)

// Outcomes of a provider resolving its selectors against the Oxide API.
const (
	// ResolutionSuccess is a resolution that reached the API and produced a reading.
	ResolutionSuccess = "success"
	// ResolutionError is a resolution that could not be made, whether or not a previous reading was served in its place.
	ResolutionError = "error"
)

// Reasons a shape stops being out of consideration.
const (
	// ReopenTimeout is the shape's wait having elapsed.
	ReopenTimeout = "timeout"
	// ReopenCapacityReleased is a terminated instance having freed room the shape fits in.
	ReopenCapacityReleased = "capacity-released"
	// ReopenInventory is the rack having been read again and now having room for the shape.
	ReopenInventory = "inventory"
)

var (
	// StuckLaunchResources counts launches abandoned because an Oxide resource never left a transitional state.
	StuckLaunchResources = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: launchSubsystem,
		Name:      "stuck_resources_total",
		Help: "Number of launches abandoned because an Oxide resource never left a transitional " +
			"state, by resource kind and the state it was stuck in.",
	}, []string{kindLabel, stateLabel})

	// WatchdogDeletions counts NodeClaims the launch watchdog deleted, by what it found the instance doing.
	WatchdogDeletions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: launchSubsystem,
		Name:      "watchdog_deletions_total",
		Help: "Number of launched but unregistered NodeClaims deleted by the launch watchdog, " +
			"by the instance run state that prompted it.",
	}, []string{stateLabel})

	// OrphanResources counts suspected orphaned Oxide resources.
	OrphanResources = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: reaperSubsystem,
		Name:      "orphan_resources",
		Help: "Oxide resources owned by this cluster whose NodeClaim no longer exists, " +
			"by resource kind.",
	}, []string{kindLabel})

	// OrphanAgeSeconds is how long the oldest orphan has been one.
	OrphanAgeSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: reaperSubsystem,
		Name:      "orphan_age_seconds",
		Help: "Seconds since the oldest outstanding orphan was first observed. " +
			"Zero when there are none.",
	})

	// ReaperDeleteAttempts and ReaperDeleteFailures are the reaper's work and how much of it is not working.
	ReaperDeleteAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: reaperSubsystem,
		Name:      "delete_attempts_total",
		Help:      "Number of orphaned Oxide resources the reaper has tried to delete, by kind.",
	}, []string{kindLabel})

	ReaperDeleteFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: reaperSubsystem,
		Name:      "delete_failures_total",
		Help:      "Number of orphan deletions the reaper could not complete, by kind.",
	}, []string{kindLabel})

	BootDiskPoolDisks = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: bootDiskSubsystem,
		Name:      "disks",
		Help:      "Boot disks by NodeClass and state.",
	}, []string{nodeClassLabel, stateLabel})

	BootDiskPoolAcquisitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: bootDiskSubsystem,
		Name:      "acquisitions_total",
		Help:      "Boot disk pool acquisitions by result.",
	}, []string{resultLabel})

	BootDiskPoolCreationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: bootDiskSubsystem,
		Name:      "creation_seconds",
		Help:      "Time to create a pooled boot disk.",
		Buckets:   prometheus.DefBuckets,
	})

	BootDiskPoolCreateFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: bootDiskSubsystem,
		Name:      "create_failures_total",
		Help:      "Failed pooled boot disk creates.",
	})

	BootDiskPoolDeleteFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: bootDiskSubsystem,
		Name:      "delete_failures_total",
		Help:      "Failed pooled boot disk deletes.",
	})

	// ICEs counts refused launches, by which limit was diagnosed as binding.
	ICEs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "ice_total",
		Help: "Number of instance launches refused for insufficient capacity, " +
			"by the limit diagnosed as binding.",
	}, []string{reasonLabel})

	// Successes counts launches that succeeded and so cleared a shape's refusal history.
	Successes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "success_total",
		Help:      "Number of instance launches that succeeded and cleared a shape's refusal history.",
	})

	// Probes counts attempts to take the single trial launch a reopened shape allows.
	Probes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "probe_total",
		Help: "Number of attempts to reserve the trial launch of a reopened shape, " +
			"by whether the reservation was granted.",
	}, []string{resultLabel})

	// Reopens counts shapes returning to consideration, by what put them there.
	Reopens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "reopen_total",
		Help:      "Number of times a shape returned to consideration, by what reopened it.",
	}, []string{reasonLabel})

	// StorageRefusals counts launches refused because the boot disk could not be provisioned.
	StorageRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "storage_refusal_total",
		Help: "Number of launches refused because the boot disk could not be provisioned, " +
			"by which limit was diagnosed as binding.",
	}, []string{reasonLabel})

	// StorageRefusalsConsecutive is how many launches in a row have been refused for want of boot disk storage.
	StorageRefusalsConsecutive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "storage_refusals_consecutive",
		Help: "Consecutive launches refused because the boot disk could not be provisioned. " +
			"Zero means the most recent launch provisioned its boot disk.",
	})

	// UnavailableShapes is how many shapes are currently out of consideration.
	UnavailableShapes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "unavailable_shapes",
		Help: "Number of instance shapes currently out of consideration. " +
			"Each covers every shape at least as large in both CPU and memory.",
	})

	// UnavailableInstanceTypes is how many instance types the scheduler is not being offered.
	UnavailableInstanceTypes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "unavailable_instance_types",
		Help: "Number of instance types the scheduler is not currently being offered, " +
			"whether refused, over quota, or too large for any sled.",
	})

	// InstanceTypes is the size of the list, as the denominator for UnavailableInstanceTypes.
	InstanceTypes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "instance_types",
		Help:      "Number of instance types the provider offers in total.",
	})

	// BackoffSeconds is how much longer each unavailable shape has to wait.
	BackoffSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "backoff_seconds",
		Help: "Seconds remaining before an unavailable instance shape is offered again. " +
			"Zero means it is being offered now for a trial launch.",
	}, []string{instanceTypeLabel})

	// ConsecutiveICEs is how many refusals in a row each unavailable shape has accumulated.
	ConsecutiveICEs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "consecutive_ices",
		Help:      "Consecutive refused launches recorded against an unavailable instance shape.",
	}, []string{instanceTypeLabel})

	// FeasibleInstances is how many instances of a shape the rack could place right now.
	FeasibleInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: inventorySubsystem,
		Name:      "feasible_instances",
		Help: "Instances of an unavailable shape the rack could place right now, per the last " +
			"sled inventory reading.",
	}, []string{instanceTypeLabel})

	// SnapshotAgeSeconds is how old the sled inventory reading is.
	SnapshotAgeSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: inventorySubsystem,
		Name:      "snapshot_age_seconds",
		Help: "Age of the sled inventory reading in seconds. Absent when inventory is disabled or " +
			"has never been read.",
	})

	// SnapshotFresh is whether the reading is recent enough to rule shapes out at all.
	SnapshotFresh = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: inventorySubsystem,
		Name:      "snapshot_fresh",
		Help: "1 when the sled inventory reading is recent enough to rule shapes out, " +
			"0 when it is stale, disabled or unread.",
	})

	// SledFreeCPUs and SledFreeMemoryBytes are per-sled headroom, bounded by the size of the rack.
	SledFreeCPUs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: inventorySubsystem,
		Name:      "sled_free_cpus",
		Help:      "CPUs free on a sled, per the last reading. Zero for a sled accepting no placements.",
	}, []string{sledLabel})

	SledFreeMemoryBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: inventorySubsystem,
		Name:      "sled_free_memory_bytes",
		Help: "Memory free on a sled in bytes, per the last reading. " +
			"Zero for a sled accepting no placements.",
	}, []string{sledLabel})

	// LargestFeasibleCPUs and LargestFeasibleMemoryBytes are the largest shape any single sled could take.
	LargestFeasibleCPUs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: inventorySubsystem,
		Name:      "largest_feasible_cpus",
		Help:      "CPUs of the largest instance shape the rack could place right now.",
	})

	LargestFeasibleMemoryBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: inventorySubsystem,
		Name:      "largest_feasible_memory_bytes",
		Help:      "Memory in bytes of the largest instance shape the rack could place right now.",
	})

	// ProviderCacheHits counts resolutions served from a cached reading.
	ProviderCacheHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: providerSubsystem,
		Name:      "cache_hits_total",
		Help:      "Number of resource resolutions served from a cached reading, without an Oxide API call.",
	}, []string{providerLabel})

	// ProviderResolutions counts resolutions that actually went to the API, by how they turned out.
	ProviderResolutions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: providerSubsystem,
		Name:      "resolutions_total",
		Help: "Number of resource resolutions made against the Oxide API, " +
			"by whether the resolution succeeded.",
	}, []string{providerLabel, resultLabel})

	// ProviderStaleServed counts failed resolutions absorbed by serving the previous reading.
	ProviderStaleServed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: providerSubsystem,
		Name:      "stale_served_total",
		Help: "Number of failed resource resolutions served from a previous reading " +
			"rather than returned as an error.",
	}, []string{providerLabel})

	// ProviderOldestEntryAge is the worst-case staleness of anything a provider is currently serving.
	ProviderOldestEntryAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: providerSubsystem,
		Name:      "oldest_entry_age_seconds",
		Help: "Age in seconds of the oldest reading a resource provider is currently holding. " +
			"Zero when it holds none.",
	}, []string{providerLabel})

	// ProviderCacheEntries is how many readings a provider holds.
	ProviderCacheEntries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: providerSubsystem,
		Name:      "cache_entries",
		Help:      "Number of cached readings a resource provider currently holds.",
	}, []string{providerLabel})

	// OwnershipRejections counts resources excluded from lifecycle operations by marker checks.
	OwnershipRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "karpenter_oxide",
		Name:      "ownership_rejections_total",
		Help:      "Number of Oxide resources rejected by ownership verification.",
	}, []string{operationLabel, resourceKindLabel, reasonLabel})
)

func init() {
	crmetrics.Registry.MustRegister(
		StuckLaunchResources,
		WatchdogDeletions,
		OrphanResources,
		OrphanAgeSeconds,
		ReaperDeleteAttempts,
		ReaperDeleteFailures,
		BootDiskPoolDisks,
		BootDiskPoolAcquisitions,
		BootDiskPoolCreationSeconds,
		BootDiskPoolCreateFailures,
		BootDiskPoolDeleteFailures,
		ICEs,
		Successes,
		Probes,
		Reopens,
		StorageRefusals,
		StorageRefusalsConsecutive,
		UnavailableShapes,
		UnavailableInstanceTypes,
		InstanceTypes,
		BackoffSeconds,
		ConsecutiveICEs,
		FeasibleInstances,
		SnapshotAgeSeconds,
		SnapshotFresh,
		SledFreeCPUs,
		SledFreeMemoryBytes,
		LargestFeasibleCPUs,
		LargestFeasibleMemoryBytes,
		ProviderCacheHits,
		ProviderResolutions,
		ProviderStaleServed,
		ProviderOldestEntryAge,
		ProviderCacheEntries,
		OwnershipRejections,
	)
}

// ResetShapeGauges drops every per-shape series before a publisher writes the current set.
func ResetShapeGauges() {
	BackoffSeconds.Reset()
	ConsecutiveICEs.Reset()
	FeasibleInstances.Reset()
}

// ResetSledGauges clears per-sled series.
func ResetSledGauges() {
	SledFreeCPUs.Reset()
	SledFreeMemoryBytes.Reset()
}

// Seconds is the value to publish for a duration gauge, floored at zero.
func Seconds(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return d.Seconds()
}
