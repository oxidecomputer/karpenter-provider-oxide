package instancetype

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	GiB = 1 << 30
	MiB = 1 << 20

	systemReservedMemBytes int64 = 0
	systemReservedMilliCPU int64 = 0

	evictionMemBytes int64 = 100 * MiB

	// These match Nexus MAX_VCPU_PER_INSTANCE and MAX_MEMORY_BYTES_PER_INSTANCE in
	// omicron/nexus/src/app/mod.rs.
	maxInstanceVCPUs     = 254
	maxInstanceMemoryGiB = 1536

	// DefaultShapeIncrementPercent is the default catalog step.
	DefaultShapeIncrementPercent = 10.0
	minInstanceMemoryGiB         = 4

	cpuCostWeight = 1.0
	memCostWeight = 0.125
)

type Provider struct {
	mu            sync.Mutex
	instanceTypes []*cloudprovider.InstanceType

	generation generation
	built      bool

	capacity *CapacityManager

	quotas QuotaProvider

	shapes []Specs
}

// ShapeConfig controls the density of the generated instance type catalog.
type ShapeConfig struct {
	// IncrementPercent is the geometric increase between successive CPU and memory values.
	IncrementPercent float64
}

func DefaultShapeConfig() ShapeConfig {
	return ShapeConfig{IncrementPercent: DefaultShapeIncrementPercent}
}

// Validate checks whether this configuration can produce a finite, useful catalog.
func (c ShapeConfig) Validate() error {
	if math.IsNaN(c.IncrementPercent) || math.IsInf(c.IncrementPercent, 0) ||
		c.IncrementPercent <= 0 || c.IncrementPercent > 100 {
		return fmt.Errorf(
			"instance type increment percent must be finite and in (0, 100], got %v",
			c.IncrementPercent,
		)
	}
	return nil
}

// QuotaProvider is the silo quota this provider constrains its offerings by.
type QuotaProvider interface {
	// Quota returns the current quota.
	Quota() (utilization.Quota, error)
	// Generation changes whenever the quota does.
	Generation() uint64
	// Refresh re-reads the quota.
	Refresh(ctx context.Context, force bool) (bool, error)
}

type generation struct {
	capacity uint64
	quota    uint64
}

type Specs struct {
	Ncpus    int
	MemInGiB int
}

// NewProvider builds an instance type provider.
func NewProvider(quotas QuotaProvider, inventory InventoryProvider) *Provider {
	provider, err := NewProviderWithShapeConfig(quotas, inventory, DefaultShapeConfig())
	if err != nil {
		panic(err)
	}
	return provider
}

// NewProviderWithShapeConfig builds a provider with a configurable instance type catalog.
func NewProviderWithShapeConfig(
	quotas QuotaProvider,
	inventory InventoryProvider,
	config ShapeConfig,
) (*Provider, error) {
	shapes, err := generateShapes(config.IncrementPercent)
	if err != nil {
		return nil, err
	}
	return &Provider{
		capacity: NewCapacityManager(inventory),
		quotas:   quotas,
		shapes:   shapes,
	}, nil
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

// RecordICE takes a shape, and every shape at least as large, out of consideration until its backoff elapses.
func (i *Provider) RecordICE(ctx context.Context, specs Specs, reason Reason) {
	i.capacity.RecordICE(ctx, specs, reason)
}

// RecordSuccess reports that a launch of this shape succeeded.
func (i *Provider) RecordSuccess(ctx context.Context, specs Specs) {
	i.capacity.RecordSuccess(ctx, specs)
	i.capacity.ClearStorageRefusal(ctx)
}

// SeedLaunchable reports that the cluster is running a node of this shape.
func (i *Provider) SeedLaunchable(ctx context.Context, specs Specs) {
	i.capacity.SeedLaunchable(ctx, specs)
}

// RecordStorageRefusal reports that a launch was refused because its boot disk could not be provisioned.
func (i *Provider) RecordStorageRefusal(ctx context.Context, reason Reason, message string) {
	i.capacity.RecordStorageRefusal(ctx, reason, message)
}

// StorageRefusal returns the last launch refused for want of boot disk storage.
func (i *Provider) StorageRefusal() (StorageRefusal, bool) {
	return i.capacity.StorageRefusal()
}

// Unavailable returns every shape currently out of consideration.
func (i *Provider) Unavailable() []Entry {
	return i.capacity.Snapshot()
}

// UnavailableInstanceTypeCount is how many instance types the scheduler is currently not being offered.
func (i *Provider) UnavailableInstanceTypeCount() (unavailable, total int) {
	instanceTypes := i.InstanceTypes()
	for _, it := range instanceTypes {
		if len(it.Offerings.Available()) == 0 {
			unavailable++
		}
	}
	return unavailable, len(instanceTypes)
}

// Rack returns the rack reading the provider is deciding against, and what that reading is worth.
func (i *Provider) Rack() (capacity.Snapshot, InventoryState) {
	return i.capacity.Rack()
}

// LargestShapeThatFits returns the largest shape the provider offers that fits in this much free space.
func LargestShapeThatFits(free capacity.Resources) (Specs, bool) {
	return largestShapeThatFits(shapes(), free)
}

// LargestShapeThatFits returns the largest configured shape that fits in free.
func (i *Provider) LargestShapeThatFits(free capacity.Resources) (Specs, bool) {
	return largestShapeThatFits(i.shapes, free)
}

func largestShapeThatFits(catalog []Specs, free capacity.Resources) (Specs, bool) {
	largest := Specs{}
	found := false
	for _, specs := range catalog {
		if specs.Ncpus > free.CPUs || uint64(specs.MemInGiB)*GiB > free.Memory {
			continue
		}
		if !found || price(specs) > price(largest) {
			largest, found = specs, true
		}
	}
	return largest, found
}

// LargestShapeOnRack returns the largest shape any single sled in this reading could take.
func LargestShapeOnRack(rack capacity.Snapshot) (Specs, bool) {
	return largestShapeOnRack(shapes(), rack)
}

// LargestShapeOnRack returns the largest configured shape any single sled can take.
func (i *Provider) LargestShapeOnRack(rack capacity.Snapshot) (Specs, bool) {
	return largestShapeOnRack(i.shapes, rack)
}

func largestShapeOnRack(catalog []Specs, rack capacity.Snapshot) (Specs, bool) {
	largest := Specs{}
	found := false
	for _, sled := range rack.Sleds() {
		if !sled.Provisionable {
			continue
		}
		shape, ok := largestShapeThatFits(catalog, sled.Free())
		if !ok {
			continue
		}
		if !found || price(shape) > price(largest) {
			largest, found = shape, true
		}
	}
	return largest, found
}

// RackHeadroom returns the headroom of every distinct class of sled in this reading.
func RackHeadroom(rack capacity.Snapshot) []Specs {

	var frontier []Specs
	for _, sled := range rack.Sleds() {
		if !sled.Provisionable {
			continue
		}
		free := sled.Free()

		room := Specs{Ncpus: free.CPUs, MemInGiB: int(free.Memory / GiB)}
		if room.Ncpus <= 0 || room.MemInGiB <= 0 {
			continue
		}
		frontier = append(frontier, room)
	}

	if len(frontier) == 0 {
		return []Specs{{}}
	}

	slices.SortFunc(frontier, func(a, b Specs) int {
		if delta := b.Ncpus - a.Ncpus; delta != 0 {
			return delta
		}
		return b.MemInGiB - a.MemInGiB
	})
	maximal := frontier[:0:0]
	mostMemory := 0
	for _, room := range frontier {
		if room.MemInGiB <= mostMemory {
			continue
		}
		maximal = append(maximal, room)
		mostMemory = room.MemInGiB
	}

	for _, room := range maximal {
		if room.Ncpus >= maxInstanceVCPUs && room.MemInGiB >= maxInstanceMemoryGiB {
			return nil
		}
	}

	slices.SortFunc(maximal, func(a, b Specs) int {
		if delta := a.Ncpus - b.Ncpus; delta != 0 {
			return delta
		}
		return a.MemInGiB - b.MemInGiB
	})
	return maximal
}

// CatalogLimits is the largest of each dimension the provider's catalog contains.
func CatalogLimits() Specs {
	return Specs{Ncpus: maxInstanceVCPUs, MemInGiB: maxInstanceMemoryGiB}
}

func shapes() []Specs {
	all, err := generateShapes(DefaultShapeIncrementPercent)
	if err != nil {
		panic(err)
	}
	return all
}

func generateShapes(incrementPercent float64) ([]Specs, error) {
	if err := (ShapeConfig{IncrementPercent: incrementPercent}).Validate(); err != nil {
		return nil, err
	}

	cpus := geometricValues(1, maxInstanceVCPUs, incrementPercent)
	memory := geometricValues(minInstanceMemoryGiB, maxInstanceMemoryGiB, incrementPercent)
	all := make([]Specs, 0, len(cpus)*len(memory))
	for _, ncpus := range cpus {
		for _, memInGiB := range memory {
			if memInGiB*2 < ncpus {
				continue
			}
			all = append(all, Specs{Ncpus: ncpus, MemInGiB: memInGiB})
		}
	}
	return all, nil
}

func geometricValues(min, max int, incrementPercent float64) []int {
	growth := 1 + incrementPercent/100
	logGrowth := math.Log(growth)
	values := []int{min}
	for values[len(values)-1] < max {
		last := values[len(values)-1]
		exponent := int(math.Floor(math.Log(float64(last)/float64(min))/logGrowth)) + 1
		next := int(math.Ceil(float64(min) * math.Pow(growth, float64(exponent))))

		for next <= last {
			exponent++
			next = int(math.Ceil(float64(min) * math.Pow(growth, float64(exponent))))
		}
		if next >= max {
			next = max
		}
		values = append(values, next)
	}
	return values
}

// RecordCapacityReleased reports that the rack has taken back the resources of an instance of this shape.
func (i *Provider) RecordCapacityReleased(ctx context.Context, specs Specs) {
	i.capacity.RecordCapacityReleased(ctx, specs)
}

// TryAcquire reserves the right to launch this shape, and reports whether the caller got it.
func (i *Provider) TryAcquire(specs Specs) bool {
	return i.capacity.TryAcquire(specs)
}

// ReleaseProbe hands back a reservation taken by TryAcquire for a launch that never reached the rack.
func (i *Provider) ReleaseProbe(specs Specs) {
	i.capacity.ReleaseProbe(specs)
}

// IsUnavailable reports whether a shape is currently ruled out, and why.
func (i *Provider) IsUnavailable(specs Specs) (bool, Reason) {
	if unavailable, reason := i.capacity.IsUnavailable(specs); unavailable {
		return true, reason
	}
	quota, err := i.quota()
	if err != nil {
		return false, ""
	}
	if admits, _ := quota.Admits(requestFor(specs)); !admits {
		return true, ReasonQuota
	}
	return false, ""
}

// Quota returns the silo quota.
func (i *Provider) Quota() (utilization.Quota, error) {
	return i.quota()
}

// RefreshQuota re-reads the silo quota.
func (i *Provider) RefreshQuota(ctx context.Context) {
	if i.quotas == nil {
		return
	}
	if _, err := i.quotas.Refresh(ctx, false); err != nil {
		log.FromContext(ctx).V(1).Info(
			"unable to re-read the silo quota after a refused launch",
			"reason", err.Error(),
		)
	}
}

func (i *Provider) quota() (utilization.Quota, error) {
	if i.quotas == nil {
		return utilization.Quota{}, utilization.ErrNoSnapshot
	}
	return i.quotas.Quota()
}

func requestFor(specs Specs) utilization.Request {
	return utilization.Request{
		CPUs:   int64(specs.Ncpus),
		Memory: uint64(specs.MemInGiB) * GiB,
	}
}

// InstanceTypes returns the current instance type catalog.
func (i *Provider) InstanceTypes() []*cloudprovider.InstanceType {
	i.mu.Lock()
	defer i.mu.Unlock()

	current := generation{capacity: i.capacity.Generation()}
	if i.quotas != nil {
		current.quota = i.quotas.Generation()
	}
	if i.built && i.generation == current {
		return i.instanceTypes
	}

	availability := i.capacity.Availability()

	quota, quotaErr := i.quota()
	var instanceTypes []*cloudprovider.InstanceType
	for _, specs := range i.shapes {
		cpus, mem := specs.Ncpus, specs.MemInGiB
		resources := corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpus), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(guestMemory(mem), resource.BinarySI),
			corev1.ResourcePods:   *resource.NewQuantity(int64(110), resource.DecimalSI),
		}
		instanceTypeName := fmt.Sprintf("%d-%d", cpus, mem)
		reserved := ReservedFor(specs)
		cpuMemoryRatio := reducedCPUMemoryRatio(cpus, mem)
		refused, _ := availability.Unavailable(specs)

		overQuota := false
		if quotaErr == nil {
			admits, _ := quota.Admits(requestFor(specs))
			overQuota = !admits
		}
		instanceTypes = append(instanceTypes, &cloudprovider.InstanceType{
			Name:     fmt.Sprintf("%d-%d", cpus, mem),
			Capacity: resources,
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
				v1alpha1.LabelInstanceCPU: scheduling.NewRequirement(
					v1alpha1.LabelInstanceCPU,
					corev1.NodeSelectorOpIn,
					strconv.Itoa(cpus),
				),
				v1alpha1.LabelInstanceMemory: scheduling.NewRequirement(
					v1alpha1.LabelInstanceMemory,
					corev1.NodeSelectorOpIn,
					strconv.Itoa(mem*1024),
				),
				v1alpha1.LabelInstanceCPUMemoryRatio: scheduling.NewRequirement(
					v1alpha1.LabelInstanceCPUMemoryRatio,
					corev1.NodeSelectorOpIn,
					cpuMemoryRatio,
				),
			},
			Overhead: &cloudprovider.InstanceTypeOverhead{
				KubeReserved: corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewMilliQuantity(reserved.CPU, resource.DecimalSI),
					corev1.ResourceMemory: *resource.NewQuantity(reserved.Memory, resource.BinarySI),
				},
				SystemReserved: corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewMilliQuantity(systemReservedMilliCPU, resource.DecimalSI),
					corev1.ResourceMemory: *resource.NewQuantity(systemReservedMemBytes, resource.BinarySI),
				},
				EvictionThreshold: corev1.ResourceList{
					corev1.ResourceMemory: *resource.NewQuantity(reserved.EvictionMemory, resource.BinarySI),
				},
			},
			Offerings: cloudprovider.Offerings{
				&cloudprovider.Offering{
					Available: !refused && !overQuota,

					Price: price(specs),
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
	i.instanceTypes = instanceTypes
	i.generation = current
	i.built = true
	return instanceTypes
}

// Nexus sells memory in whole GiB, but the guest kernel never sees all of it:
// firmware and struct page overhead take a cut that grows with the shape.
// Measured on a rack: 4 GiB loses 185 MiB, 5 GiB loses 211 MiB, 10 GiB loses
// 306 MiB. These constants bound all three from above, so modeled allocatable
// stays at or under the node's real allocatable and the scheduler cannot
// over-commit a node it has never seen.
const (
	guestMemoryFixedBytes int64 = 128 * MiB
	guestMemoryLossBasisP int64 = 250
)

// guestMemory is how much of a shape's memory the guest kernel actually reports.
func guestMemory(memGiB int) int64 {
	total := int64(memGiB) * GiB
	return total - guestMemoryFixedBytes - total*guestMemoryLossBasisP/10000
}

// Reserved is what kubelet must be told to hold back so that a node's real
// allocatable matches the allocatable the scheduler was promised. Bootstrap
// renders these into kubelet flags; anything that changes here changes the
// instance type overhead in lockstep.
type Reserved struct {
	// CPU is kube-reserved CPU in milliCPU.
	CPU int64
	// Memory is kube-reserved memory in bytes.
	Memory int64
	// EvictionMemory is the hard memory.available eviction threshold in bytes.
	EvictionMemory int64
}

// ReservedFor returns the reservations for a shape.
func ReservedFor(specs Specs) Reserved {
	return Reserved{
		CPU:            cpuReserved(int64(specs.Ncpus) * 1000),
		Memory:         memReserved(int64(specs.MemInGiB) * GiB),
		EvictionMemory: evictionMemBytes,
	}
}

// KubeletArgs renders the reservations as kubelet "name=value" arguments.
func (r Reserved) KubeletArgs() []string {
	return []string{
		fmt.Sprintf(
			"kube-reserved=cpu=%s,memory=%s",
			resource.NewMilliQuantity(r.CPU, resource.DecimalSI),
			resource.NewQuantity(r.Memory, resource.BinarySI),
		),
		fmt.Sprintf(
			"eviction-hard=memory.available<%s",
			resource.NewQuantity(r.EvictionMemory, resource.BinarySI),
		),
	}
}

func reducedCPUMemoryRatio(cpus, memGiB int) string {
	divisor := greatestCommonDivisor(cpus, memGiB)
	return fmt.Sprintf("%d-%d", cpus/divisor, memGiB/divisor)
}

func greatestCommonDivisor(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func price(specs Specs) float64 {
	return float64(specs.Ncpus)*cpuCostWeight + float64(specs.MemInGiB)*memCostWeight
}

type tier struct {
	limit   int64
	percent float64
}

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

func memReserved(memBytes int64) int64 {
	return reserveTiered(memBytes, []tier{
		{4 * GiB, 0.25},
		{8 * GiB, 0.20},
		{16 * GiB, 0.10},
		{128 * GiB, 0.06},
		{math.MaxInt64, 0.02},
	})
}

func cpuReserved(milliCPU int64) int64 {
	return reserveTiered(milliCPU, []tier{
		{1000, 0.06},
		{2000, 0.01},
		{4000, 0.005},
		{math.MaxInt64, 0.0025},
	})
}
