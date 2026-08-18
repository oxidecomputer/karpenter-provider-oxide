package instancetype

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	GiB = 1 << 30
	MiB = 1 << 20

	systemReservedMemBytes int64 = 0
	systemReservedMilliCPU int64 = 0

	// GKE default hard eviction threshold (memory.available<100Mi).
	evictionMemBytes int64 = 100 * MiB

	// maxSledCPUs is the maximum number of CPUs a sled could have. Not every sled or rack
	// configuration will support this. If the control plane errors, we'll mark this offering as
	// unavailable, so the system will stabilize even when this number is too big.
	maxSledCPUs = 384
	// maxSledMemGiB if the maximum amount of memory a sled could have. Not every sled or rack
	// configuration will support this. If the control plane errors, we'll mark this offering as
	// unavailable, so the system will stabilize even when this number is too big.
	maxSledMemGiB = 1536
)

var (
	// memRatios is used to generate instance type shapes (1:2, 1:4, 1:8, etc.)
	memRatios = []int{2, 4, 8, 16, 32}
)

type Provider struct {
	mu            sync.Mutex
	instanceTypes []*cloudprovider.InstanceType
	// generation is the unavailable offering generation instanceTypes was built against, which is
	// what makes the list stale.
	generation uint64
	built      bool

	unavailable *UnavailableOfferings
}

type Specs struct {
	Ncpus    int
	MemInGiB int
}

func NewProvider() *Provider {
	return &Provider{
		unavailable: NewUnavailableOfferings(),
	}
}

func SpecsToInstanceTypeName(ncpus, memInGiB int) string {
	return fmt.Sprintf("%d-%d", ncpus, memInGiB)
}

func InstanceTypeToSpecs(instanceType string) (Specs, error) {
	ncpusStr, memInGiBStr, ok := strings.Cut(instanceType, "-")
	if !ok {
		return Specs{}, fmt.Errorf("invalid instance type: %v", instanceType)
	}
	ncpus, err := strconv.Atoi(ncpusStr)
	if err != nil {
		return Specs{}, fmt.Errorf("invalid instance type, failed parsing ncpus: %w", err)
	}
	memInGiB, err := strconv.Atoi(memInGiBStr)
	if err != nil {
		return Specs{}, fmt.Errorf("invalid instance type, failed parsing memory in GiB: %w", err)
	}
	return Specs{
		Ncpus:    ncpus,
		MemInGiB: memInGiB,
	}, nil
}

func (i *Provider) Get(instanceType string) (*cloudprovider.InstanceType, bool) {
	for _, it := range i.InstanceTypes() {
		if it.Name == instanceType {
			return it, true
		}
	}
	return nil, false
}

// MarkUnavailable takes a shape, and every shape at least as large, out of consideration until the
// entry expires. It is called when the rack refuses to place an instance of that shape.
func (i *Provider) MarkUnavailable(ctx context.Context, specs Specs) {
	i.unavailable.Mark(ctx, specs)
}

// IsUnavailable reports whether a shape is currently ruled out by an insufficient capacity error.
func (i *Provider) IsUnavailable(specs Specs) bool {
	return i.unavailable.IsUnavailable(specs)
}

// InstanceTypes returns every instance type, with the offerings of the shapes the rack has recently
// refused to place marked unavailable so scheduling passes over them.
//
// The list is rebuilt rather than updated in place, since the one already returned is read
// concurrently by the scheduler. Rebuilding only happens when the set of unavailable shapes has
// actually changed, so the steady state is a cache hit.
func (i *Provider) InstanceTypes() []*cloudprovider.InstanceType {
	i.mu.Lock()
	defer i.mu.Unlock()

	generation := i.unavailable.Generation()
	if i.built && i.generation == generation {
		return i.instanceTypes
	}

	unavailable := i.unavailable.Snapshot()
	var instanceTypes []*cloudprovider.InstanceType
	for cpus := 1; cpus <= maxSledCPUs; cpus++ {
		for _, memRatio := range memRatios {
			mem := cpus * memRatio
			capacity := corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(int64(cpus), resource.DecimalSI),
				corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", mem)),
				corev1.ResourcePods:   *resource.NewQuantity(int64(110), resource.DecimalSI),
			}
			instanceTypeName := fmt.Sprintf("%d-%d", cpus, mem)
			if mem <= maxSledMemGiB {
				instanceTypes = append(instanceTypes, &cloudprovider.InstanceType{
					Name:     fmt.Sprintf("%d-%d", cpus, mem),
					Capacity: capacity,
					Requirements: scheduling.Requirements{
						corev1.LabelInstanceTypeStable: scheduling.NewRequirement(
							corev1.LabelInstanceTypeStable,
							corev1.NodeSelectorOpIn,
							instanceTypeName,
						),
						corev1.LabelArchStable: scheduling.NewRequirement(
							corev1.LabelArchStable,
							corev1.NodeSelectorOpIn,
							"amd64",
						),
						corev1.LabelOSStable: scheduling.NewRequirement(
							corev1.LabelOSStable,
							corev1.NodeSelectorOpIn,
							"linux",
						),
						corev1.LabelWindowsBuild: scheduling.NewRequirement(
							corev1.LabelWindowsBuild,
							corev1.NodeSelectorOpDoesNotExist,
						),
					},
					Overhead: &cloudprovider.InstanceTypeOverhead{
						KubeReserved: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuReserved(capacity.Cpu().MilliValue()), resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(memReserved(capacity.Memory().Value()), resource.BinarySI),
						},
						SystemReserved: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewMilliQuantity(systemReservedMilliCPU, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(systemReservedMemBytes, resource.BinarySI),
						},
						EvictionThreshold: corev1.ResourceList{
							corev1.ResourceMemory: *resource.NewQuantity(evictionMemBytes, resource.BinarySI),
						},
					},
					Offerings: cloudprovider.Offerings{
						&cloudprovider.Offering{
							Available: !covered(unavailable, Specs{Ncpus: cpus, MemInGiB: mem}),
							Requirements: scheduling.Requirements{
								corev1.LabelTopologyZone: scheduling.NewRequirement(
									corev1.LabelTopologyZone,
									corev1.NodeSelectorOpIn,
									"rack",
								),
								karpv1.CapacityTypeLabelKey: scheduling.NewRequirement(
									karpv1.CapacityTypeLabelKey,
									corev1.NodeSelectorOpIn,
									"on-demand",
								),
							},
						},
					},
				})
			}
		}
	}
	i.instanceTypes = instanceTypes
	i.generation = generation
	i.built = true
	return instanceTypes
}

type tier struct {
	limit   int64 // upper bound of this tier, in the resource's base unit
	percent float64
}

// reserveTiered walks declining-percentage tiers and sums the reservation.
func reserveTiered(total int64, tiers []tier) int64 {
	var reserved float64
	var prev int64
	for _, t := range tiers {
		if total <= prev {
			break
		}
		upper := min(total, t.limit)
		reserved += float64(upper-prev) * t.percent
		prev = t.limit
	}
	return int64(reserved)
}

// memReserved returns kube-reserved memory (bytes) using tiers.
func memReserved(memBytes int64) int64 {
	return reserveTiered(memBytes, []tier{
		{4 * GiB, 0.25},   // first 4 GiB
		{8 * GiB, 0.20},   // 4–8 GiB
		{16 * GiB, 0.10},  // 8–16 GiB
		{128 * GiB, 0.06}, // 16–128 GiB
		{math.MaxInt64, 0.02},
	})
}

// cpuReserved returns kube-reserved CPU (millicores) using tiers.
func cpuReserved(milliCPU int64) int64 {
	return reserveTiered(milliCPU, []tier{
		{1000, 0.06},  // 1st core
		{2000, 0.01},  // 2nd core
		{4000, 0.005}, // 3rd–4th cores
		{math.MaxInt64, 0.0025},
	})
}
