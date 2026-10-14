package instancetype

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
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

	// kubelet's own ephemeral-storage defaults, restated so that what the
	// scheduler models and what the node computes agree.
	ephemeralReservedBytes int64 = 1 * GiB
	evictionNodeFSPercent  int64 = 10

	// These match Nexus MAX_VCPU_PER_INSTANCE and MAX_MEMORY_BYTES_PER_INSTANCE in
	// omicron/nexus/src/app/mod.rs.
	maxInstanceVCPUs     = 384
	maxInstanceMemoryGiB = 1536

	// DefaultShapeIncrementPercent is the default catalog step.
	DefaultShapeIncrementPercent = 10.0
	minInstanceMemoryGiB         = 1

	cpuCostWeight = 1.0
	memCostWeight = 0.125

	// Oxide has no zones; the rack is the whole failure domain. Advertised anyway so
	// that topology spreads on topology.kubernetes.io/zone see a domain to spread over.
	ZoneName = "rack"
)

type Provider struct {
	mu sync.Mutex
	// instanceTypes is the built catalog keyed by the boot disk size its
	// ephemeral storage was derived from.
	instanceTypes map[int64][]*cloudprovider.InstanceType

	generation generation

	availability *availability.Manager

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
	availability uint64
	quota        uint64
}

// Specs is retained as the catalog-facing name for an availability shape.
type Specs = availability.Shape

// NewProvider builds a catalog over the default instance types.
func NewProvider(quotas QuotaProvider, manager *availability.Manager) *Provider {
	provider, err := NewProviderWithShapeConfig(quotas, manager, DefaultShapeConfig())
	if err != nil {
		panic(err)
	}
	return provider
}

// NewProviderWithShapeConfig builds a provider with a configurable instance type catalog.
func NewProviderWithShapeConfig(
	quotas QuotaProvider,
	manager *availability.Manager,
	config ShapeConfig,
) (*Provider, error) {
	shapes, err := generateShapes(config.IncrementPercent)
	if err != nil {
		return nil, err
	}
	return &Provider{
		availability: manager,
		quotas:       quotas,
		shapes:       shapes,
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

func (i *Provider) Get(
	instanceType string,
	bootSizeBytes int64,
) (*cloudprovider.InstanceType, bool) {
	for _, it := range i.InstanceTypes(bootSizeBytes) {
		if it.Name == instanceType {
			return it, true
		}
	}
	return nil, false
}

// UnavailableInstanceTypeCount is how many instance types the scheduler is currently not being offered.
func (i *Provider) UnavailableInstanceTypeCount() (unavailable, total int) {
	// Availability does not vary with the boot disk, so any catalog answers this.
	instanceTypes := i.InstanceTypes(0)
	for _, it := range instanceTypes {
		if len(it.Offerings.Available()) == 0 {
			unavailable++
		}
	}
	return unavailable, len(instanceTypes)
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

// IsOverQuota reports whether the current silo quota rules out this shape.
func (i *Provider) IsOverQuota(specs Specs) bool {
	quota, err := i.quota()
	if err != nil {
		return false
	}
	admits, _ := quota.Admits(requestFor(specs))
	return !admits
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

// InstanceTypes returns the current instance type catalog for nodes whose boot
// disk is bootSizeBytes. A non-positive size leaves ephemeral storage
// unadvertised, which only fits callers that never schedule against it.
func (i *Provider) InstanceTypes(bootSizeBytes int64) []*cloudprovider.InstanceType {
	i.mu.Lock()
	defer i.mu.Unlock()

	current := generation{availability: i.availability.Generation()}
	if i.quotas != nil {
		current.quota = i.quotas.Generation()
	}
	if i.generation != current {
		i.instanceTypes = nil
	}
	if cached, ok := i.instanceTypes[bootSizeBytes]; ok {
		return cached
	}
	ephemeral, hasEphemeral := EphemeralStorageFor(bootSizeBytes)

	reading := i.availability.Snapshot()

	quota, quotaErr := i.quota()
	var instanceTypes []*cloudprovider.InstanceType
	for _, specs := range i.shapes {
		cpus, mem := specs.Ncpus, specs.MemInGiB
		resources := corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpus), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(guestMemory(mem), resource.BinarySI),
			corev1.ResourcePods:   *resource.NewQuantity(int64(110), resource.DecimalSI),
		}
		if hasEphemeral {
			resources[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(
				ephemeral.Capacity, resource.BinarySI,
			)
		}
		instanceTypeName := fmt.Sprintf("%d-%d", cpus, mem)
		reserved := ReservedFor(specs)
		cpuMemoryRatio := reducedCPUMemoryRatio(cpus, mem)
		refused, _ := reading.Unavailable(specs)

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
				// Mirrors the sole offering. Upstream builds the topology-spread domain
				// universe from instance type requirements, never from offerings.
				corev1.LabelTopologyZone: scheduling.NewRequirement(
					corev1.LabelTopologyZone,
					corev1.NodeSelectorOpIn,
					ZoneName,
				),
				karpv1.CapacityTypeLabelKey: scheduling.NewRequirement(
					karpv1.CapacityTypeLabelKey,
					corev1.NodeSelectorOpIn,
					karpv1.CapacityTypeOnDemand,
				),
			},
			Overhead: &cloudprovider.InstanceTypeOverhead{
				KubeReserved: kubeReserved(reserved, ephemeral, hasEphemeral),
				SystemReserved: corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewMilliQuantity(systemReservedMilliCPU, resource.DecimalSI),
					corev1.ResourceMemory: *resource.NewQuantity(systemReservedMemBytes, resource.BinarySI),
				},
				EvictionThreshold: evictionThreshold(reserved, ephemeral, hasEphemeral),
			},
			Offerings: cloudprovider.Offerings{
				&cloudprovider.Offering{
					Available: !refused && !overQuota,

					Price: price(specs),
					Requirements: scheduling.Requirements{
						corev1.LabelTopologyZone: scheduling.NewRequirement(
							corev1.LabelTopologyZone,
							corev1.NodeSelectorOpIn,
							ZoneName,
						),
						karpv1.CapacityTypeLabelKey: scheduling.NewRequirement(
							karpv1.CapacityTypeLabelKey,
							corev1.NodeSelectorOpIn,
							karpv1.CapacityTypeOnDemand,
						),
					},
				},
			},
		})
	}
	if i.instanceTypes == nil {
		i.instanceTypes = map[int64][]*cloudprovider.InstanceType{}
	}
	i.instanceTypes[bootSizeBytes] = instanceTypes
	i.generation = current
	return instanceTypes
}

func kubeReserved(
	reserved Reserved,
	ephemeral EphemeralStorage,
	hasEphemeral bool,
) corev1.ResourceList {
	list := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(reserved.CPU, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(reserved.Memory, resource.BinarySI),
	}
	if hasEphemeral {
		list[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(
			ephemeral.KubeReserved, resource.BinarySI,
		)
	}
	return list
}

func evictionThreshold(
	reserved Reserved,
	ephemeral EphemeralStorage,
	hasEphemeral bool,
) corev1.ResourceList {
	list := corev1.ResourceList{
		corev1.ResourceMemory: *resource.NewQuantity(reserved.EvictionMemory, resource.BinarySI),
	}
	if hasEphemeral {
		list[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(
			ephemeral.EvictionNodeFS, resource.BinarySI,
		)
	}
	return list
}

// Propolis presents the whole shape, but the guest kernel keeps some of it
// (struct page, text, firmware ranges). Measured loss on a rack: 1 GiB 68 MiB,
// 4 GiB 185 MiB, 10 GiB 306 MiB, 40 GiB 858 MiB, 128 GiB 2260 MiB, 196 GiB
// 3446 MiB. Each vCPU adds about 0.4 MiB (per-CPU chunks), which the slope's
// slack covers at the catalog's 2 vCPU/GiB cap. These bound every point from
// above so modeled allocatable never exceeds the node's.
const (
	guestMemoryFixedBytes int64 = 128 * MiB
	guestMemoryLossBasisP int64 = 200
)

// guestMemory is how much of a shape's memory the guest kernel actually reports.
func guestMemory(memGiB int) int64 {
	total := int64(memGiB) * GiB
	return total - guestMemoryFixedBytes - total*guestMemoryLossBasisP/10000
}

// A boot disk is not all usable filesystem: the image's partition table and
// boot partitions come off the front, and the root filesystem spends a further
// fraction on metadata. Under-reporting the remainder is safe, over-reporting
// puts pods on nodes that then hit disk pressure.
const (
	rootFSFixedBytes int64 = 1 * GiB
	rootFSLossBasisP int64 = 500
)

// EphemeralStorage is the ephemeral-storage side of an instance type: what
// kubelet will report for a boot disk of a given size, and what it holds back.
type EphemeralStorage struct {
	// Capacity is the ephemeral-storage capacity in bytes.
	Capacity int64
	// KubeReserved is kube-reserved ephemeral-storage in bytes.
	KubeReserved int64
	// EvictionNodeFS is the hard nodefs.available eviction threshold in bytes.
	EvictionNodeFS int64
}

// EphemeralStorageFor returns the ephemeral storage a node booting off a disk
// of this size has, and whether the disk is big enough to advertise any. A
// diskSet mounted over the kubelet root makes this an under-estimate, which is
// the safe direction.
func EphemeralStorageFor(bootSizeBytes int64) (EphemeralStorage, bool) {
	usable := bootSizeBytes - rootFSFixedBytes
	if usable <= 0 {
		return EphemeralStorage{}, false
	}
	available := usable - usable*rootFSLossBasisP/10000
	eviction := available * evictionNodeFSPercent / 100
	if available-eviction <= ephemeralReservedBytes {
		return EphemeralStorage{}, false
	}
	return EphemeralStorage{
		Capacity:       available,
		KubeReserved:   ephemeralReservedBytes,
		EvictionNodeFS: eviction,
	}, true
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
	// EphemeralStorage is kube-reserved ephemeral-storage in bytes.
	EphemeralStorage int64
	// EvictionNodeFSPercent is the hard nodefs.available eviction threshold, as
	// a percentage of whatever capacity kubelet computes for the root filesystem.
	EvictionNodeFSPercent int64
}

// ReservedFor returns the reservations for a shape.
func ReservedFor(specs Specs) Reserved {
	return Reserved{
		CPU:                   cpuReserved(int64(specs.Ncpus) * 1000),
		Memory:                memReserved(int64(specs.MemInGiB) * GiB),
		EvictionMemory:        evictionMemBytes,
		EphemeralStorage:      ephemeralReservedBytes,
		EvictionNodeFSPercent: evictionNodeFSPercent,
	}
}

// KubeletArgs renders the reservations as kubelet "name=value" arguments.
func (r Reserved) KubeletArgs() []string {
	return []string{
		fmt.Sprintf(
			"kube-reserved=cpu=%s,memory=%s,ephemeral-storage=%s",
			resource.NewMilliQuantity(r.CPU, resource.DecimalSI),
			resource.NewQuantity(r.Memory, resource.BinarySI),
			resource.NewQuantity(r.EphemeralStorage, resource.BinarySI),
		),
		fmt.Sprintf(
			"eviction-hard=memory.available<%s,nodefs.available<%d%%",
			resource.NewQuantity(r.EvictionMemory, resource.BinarySI),
			r.EvictionNodeFSPercent,
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

// ComparePrice orders shapes the way the scheduler does, cheapest first, so a launch
// never picks a shape consolidation would immediately replace. Ties break on CPU, then memory.
func ComparePrice(a, b Specs) int {
	if pa, pb := price(a), price(b); pa != pb {
		return cmp.Compare(pa, pb)
	}
	if delta := a.Ncpus - b.Ncpus; delta != 0 {
		return delta
	}
	return a.MemInGiB - b.MemInGiB
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
