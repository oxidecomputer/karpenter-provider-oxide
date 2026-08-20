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
//
// They live together rather than next to the code that moves them so that the cost of the whole set
// can be read in one file. Cardinality is the thing to watch: the provider offers close to two
// thousand instance shapes, so anything labeled by shape has to be restricted to the handful of
// shapes something has actually happened to, and anything labeled by sled is bounded by the rack.
// Nothing here is labeled by node, NodeClaim or pod.
//
// Karpenter's operator serves the controller-runtime registry, so registering with it is all it takes
// to appear on the existing metrics endpoint.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// namespace prefixes every series, keeping them distinct from the karpenter_ series core
	// publishes. These describe an Oxide rack, and nobody should have to guess whose they are.
	namespace = "oxide_karpenter"

	capacitySubsystem  = "capacity"
	inventorySubsystem = "inventory"

	// instanceTypeLabel is the shape a series is about, as an instance type name.
	instanceTypeLabel = "instance_type"
	// sledLabel is the Oxide sled ID a series is about.
	sledLabel = "sled"
	// reasonLabel is why something happened, and resultLabel is how it turned out. Both are closed
	// sets defined by the caller, never free-form strings.
	reasonLabel = "reason"
	resultLabel = "result"
)

// Results of trying to reserve the single trial launch a reopened shape allows.
const (
	// ProbeAcquired is the launch that got the reservation and is testing the shape.
	ProbeAcquired = "acquired"
	// ProbeRejected is a launch turned away because another one is already testing the shape. It is
	// not a capacity failure, and a burst of them means the interlock is doing its job.
	ProbeRejected = "rejected"
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
	// ICEs counts refused launches, by which limit was diagnosed as binding. This is the rate to alert
	// on: a rack that refuses launches steadily is a rack that needs attention, whatever the provider
	// does about it in the meantime.
	ICEs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "ice_total",
		Help: "Number of instance launches refused for insufficient capacity, " +
			"by the limit diagnosed as binding.",
	}, []string{reasonLabel})

	// Successes counts launches that succeeded and so cleared a shape's refusal history. Compared
	// against ICEs it says whether the provider is converging or thrashing.
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

	// Reopens counts shapes returning to consideration, by what put them there. The split is the
	// interesting part: reopens by timeout are the breaker working on its own, and reopens by
	// inventory or by released capacity are the provider learning something instead of waiting.
	Reopens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "reopen_total",
		Help:      "Number of times a shape returned to consideration, by what reopened it.",
	}, []string{reasonLabel})

	// StorageRefusals counts launches refused because the boot disk could not be provisioned, by which
	// limit was diagnosed as binding.
	//
	// Kept apart from ICEs on purpose. A storage refusal takes no shape out of consideration, so
	// folding it into the shape counter would show refusals climbing with nothing going unavailable to
	// account for them. This is the series to alert on for "nodes are not launching and no shape is to
	// blame".
	StorageRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "storage_refusal_total",
		Help: "Number of launches refused because the boot disk could not be provisioned, " +
			"by which limit was diagnosed as binding.",
	}, []string{reasonLabel})

	// StorageRefusalsConsecutive is how many launches in a row have been refused for want of boot disk
	// storage, and zero when the last launch created its disk. Unlabeled, because a storage refusal is
	// not about any shape.
	StorageRefusalsConsecutive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "storage_refusals_consecutive",
		Help: "Consecutive launches refused because the boot disk could not be provisioned. " +
			"Zero means the most recent launch provisioned its boot disk.",
	})

	// UnavailableShapes is how many shapes are currently out of consideration. Each covers every
	// shape at least as large, so this counts boundaries rather than shapes.
	UnavailableShapes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "unavailable_shapes",
		Help: "Number of instance shapes currently out of consideration. " +
			"Each covers every shape at least as large in both CPU and memory.",
	})

	// UnavailableInstanceTypes is how many instance types the scheduler is not being offered, which is
	// what UnavailableShapes reads as but is not. One boundary covers every shape above it, so alerting
	// on the boundary count would miss a rack that has just ruled out most of the list.
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

	// BackoffSeconds is how much longer each unavailable shape has to wait. Labeled by shape, which is
	// only affordable because the label set is reset on every publish and so only ever holds shapes
	// that are actually out.
	BackoffSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "backoff_seconds",
		Help: "Seconds remaining before an unavailable instance shape is offered again. " +
			"Zero means it is being offered now for a trial launch.",
	}, []string{instanceTypeLabel})

	// ConsecutiveICEs is how many refusals in a row each unavailable shape has accumulated, which is
	// what its wait is computed from.
	ConsecutiveICEs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: capacitySubsystem,
		Name:      "consecutive_ices",
		Help:      "Consecutive refused launches recorded against an unavailable instance shape.",
	}, []string{instanceTypeLabel})

	// FeasibleInstances is how many instances of a shape the rack could place right now, published
	// only for shapes that are currently unavailable. Those are the shapes where the question matters:
	// a zero here alongside a backoff says the rack agrees, and a non-zero says the refusal came from
	// something the reading cannot see.
	FeasibleInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: inventorySubsystem,
		Name:      "feasible_instances",
		Help: "Instances of an unavailable shape the rack could place right now, per the last " +
			"sled inventory reading.",
	}, []string{instanceTypeLabel})

	// SnapshotAgeSeconds is how old the sled inventory reading is. Alert on this rather than on
	// inventory errors: a reading that stops being refreshed stops constraining anything, which is
	// safe but means the provider is back to learning from refused launches.
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
	// They are per sled rather than summed because that is how placement works.
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

	// LargestFeasibleCPUs and LargestFeasibleMemoryBytes are the largest shape any single sled could
	// take. This is the one number that answers "how big a node can I get right now".
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
)

func init() {
	crmetrics.Registry.MustRegister(
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
	)
}

// ResetShapeGauges drops every per-shape series before a publisher writes the current set.
//
// A gauge labeled by shape has to be cleared rather than overwritten: a shape that is no longer
// unavailable has no value to write, and leaving its last one behind would report a backoff that
// expired long ago as though it were still counting down.
func ResetShapeGauges() {
	BackoffSeconds.Reset()
	ConsecutiveICEs.Reset()
	FeasibleInstances.Reset()
}

// ResetSledGauges drops every per-sled series, for the same reason: a sled removed from the rack, or a
// reading that no longer mentions it, must stop reporting headroom.
func ResetSledGauges() {
	SledFreeCPUs.Reset()
	SledFreeMemoryBytes.Reset()
}

// Seconds is the value to publish for a duration gauge, floored at zero so that a wait that has
// already elapsed reads as "now" rather than as a negative number.
func Seconds(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return d.Seconds()
}
