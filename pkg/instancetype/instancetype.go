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

	// These match Nexus MAX_VCPU_PER_INSTANCE and MAX_MEMORY_BYTES_PER_INSTANCE in
	// omicron/nexus/src/app/mod.rs.
	maxInstanceVCPUs     = 384
	maxInstanceMemoryGiB = 1536

	// DefaultShapeIncrementPercent is the default catalog step.
	DefaultShapeIncrementPercent = 10.0
	minInstanceMemoryGiB         = 1

	cpuCostWeight = 1.0
	memCostWeight = 0.125
)

// Topology is the region and zone every offering is placed in.
type Topology struct {
	Region string
	Zone   string
}

// SiloTopology names the silo as both region and zone. A silo is one control plane
// that will span availability zones, which is what a region is. Until an AZ can be
// targeted at launch there is no finer failure domain to offer, so the zone is the
// silo too; it becomes the AZ name once there is one to pick.
func SiloTopology(silo string) Topology {
	return Topology{Region: silo, Zone: silo}
}

// DefaultTopology stands in until the silo is known.
var DefaultTopology = SiloTopology("silo")

type Provider struct {
	mu sync.Mutex
	// instanceTypes is the built catalog, keyed by the options it was built for.
	instanceTypes map[string][]*cloudprovider.InstanceType

	generation generation

	availability *availability.Manager

	quotas QuotaProvider

	shapes []Specs

	topology Topology
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
		topology:     DefaultTopology,
	}, nil
}

// SetTopology replaces the region and zone the catalog is offered in.
func (i *Provider) SetTopology(topology Topology) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.topology = topology
	i.instanceTypes = nil
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
	opts Options,
) (*cloudprovider.InstanceType, bool) {
	for _, it := range i.InstanceTypes(opts) {
		if it.Name == instanceType {
			return it, true
		}
	}
	return nil, false
}

// UnavailableInstanceTypeCount is how many instance types the scheduler is currently not being offered.
func (i *Provider) UnavailableInstanceTypeCount() (unavailable, total int) {
	// Availability does not vary with the nodeclass, so any catalog answers this.
	instanceTypes := i.InstanceTypes(Options{})
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

// InstanceTypes returns the current instance type catalog for the nodeclass
// these options describe. A non-positive boot size leaves ephemeral storage
// unadvertised, which only fits callers that never schedule against it.
func (i *Provider) InstanceTypes(opts Options) []*cloudprovider.InstanceType {
	i.mu.Lock()
	defer i.mu.Unlock()

	current := generation{availability: i.availability.Generation()}
	if i.quotas != nil {
		current.quota = i.quotas.Generation()
	}
	if i.generation != current {
		i.instanceTypes = nil
	}
	key := opts.key()
	if cached, ok := i.instanceTypes[key]; ok {
		return cached
	}
	ephemeral, hasEphemeral := EphemeralStorageFor(opts.BootSizeBytes, opts.Kubelet.Reserved)

	reading := i.availability.Snapshot()

	quota, quotaErr := i.quota()
	var instanceTypes []*cloudprovider.InstanceType
	for _, specs := range i.shapes {
		cpus, mem := specs.Ncpus, specs.MemInGiB
		reserved := ReservedFor(specs, opts.Kubelet.Reserved)
		resources := corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(cpus), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(guestMemory(mem), resource.BinarySI),
			corev1.ResourcePods:   *resource.NewQuantity(opts.Kubelet.Pods(cpus), resource.DecimalSI),
		}
		if hasEphemeral {
			resources[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(
				ephemeral.Capacity, resource.BinarySI,
			)
		}
		instanceTypeName := fmt.Sprintf("%d-%d", cpus, mem)
		cpuMemoryRatio := reducedCPUMemoryRatio(cpus, mem)

		overQuota := false
		if quotaErr == nil {
			admits, _ := quota.Admits(requestFor(specs))
			overQuota = !admits
		}
		// One offering per pin, so a refusal under a narrow pin leaves the broader
		// ones on offer. An unpinned launch is as placeable as any of them.
		offerings := make(cloudprovider.Offerings, 0, len(v1alpha1.CPUPlatforms))
		for _, platform := range v1alpha1.CPUPlatforms {
			refused, _ := reading.Unavailable(specs, availability.Platform(platform))
			offerings = append(offerings, &cloudprovider.Offering{
				Available: !refused && !overQuota,

				Price: price(specs),
				Requirements: scheduling.Requirements{
					corev1.LabelTopologyZone: scheduling.NewRequirement(
						corev1.LabelTopologyZone,
						corev1.NodeSelectorOpIn,
						i.topology.Zone,
					),
					karpv1.CapacityTypeLabelKey: scheduling.NewRequirement(
						karpv1.CapacityTypeLabelKey,
						corev1.NodeSelectorOpIn,
						karpv1.CapacityTypeOnDemand,
					),
					v1alpha1.LabelInstanceCPUPlatform: scheduling.NewRequirement(
						v1alpha1.LabelInstanceCPUPlatform,
						corev1.NodeSelectorOpIn,
						platform,
					),
				},
			})
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
				v1alpha1.LabelInstanceCPUPlatform: scheduling.NewRequirement(
					v1alpha1.LabelInstanceCPUPlatform,
					corev1.NodeSelectorOpIn,
					v1alpha1.CPUPlatforms...,
				),
				// Mirrors the offerings. Upstream builds the topology-spread domain
				// universe from instance type requirements, never from offerings.
				corev1.LabelTopologyZone: scheduling.NewRequirement(
					corev1.LabelTopologyZone,
					corev1.NodeSelectorOpIn,
					i.topology.Zone,
				),
				corev1.LabelTopologyRegion: scheduling.NewRequirement(
					corev1.LabelTopologyRegion,
					corev1.NodeSelectorOpIn,
					i.topology.Region,
				),
				karpv1.CapacityTypeLabelKey: scheduling.NewRequirement(
					karpv1.CapacityTypeLabelKey,
					corev1.NodeSelectorOpIn,
					karpv1.CapacityTypeOnDemand,
				),
			},
			Overhead: &cloudprovider.InstanceTypeOverhead{
				KubeReserved:   withEphemeral(reserved.KubeReserved, hasEphemeral),
				SystemReserved: withEphemeral(reserved.SystemReserved, hasEphemeral),
				EvictionThreshold: reserved.evictionThreshold(
					guestMemory(mem), ephemeral, hasEphemeral,
				),
			},
			Offerings: offerings,
		})
	}
	if i.instanceTypes == nil {
		i.instanceTypes = map[string][]*cloudprovider.InstanceType{}
	}
	i.instanceTypes[key] = instanceTypes
	i.generation = current
	return instanceTypes
}

// withEphemeral drops the ephemeral-storage entry when the shape advertises
// none, since an overhead on a resource the node lacks means nothing.
func withEphemeral(list corev1.ResourceList, hasEphemeral bool) corev1.ResourceList {
	out := make(corev1.ResourceList, len(list))
	for name, quantity := range list {
		if name == corev1.ResourceEphemeralStorage && !hasEphemeral {
			continue
		}
		out[name] = quantity
	}
	return out
}

func (r Reserved) evictionThreshold(
	memoryCapacity int64,
	ephemeral EphemeralStorage,
	hasEphemeral bool,
) corev1.ResourceList {
	list := corev1.ResourceList{}
	if threshold, ok := r.EvictionHard[SignalMemoryAvailable]; ok {
		list[corev1.ResourceMemory] = *resource.NewQuantity(
			threshold.Of(memoryCapacity), resource.BinarySI,
		)
	}
	if threshold, ok := r.EvictionHard[SignalNodeFSAvailable]; ok && hasEphemeral {
		list[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(
			threshold.Of(ephemeral.Capacity), resource.BinarySI,
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

// EphemeralStorage is what kubelet will report as ephemeral-storage capacity
// for a boot disk of a given size, in bytes.
type EphemeralStorage struct {
	Capacity int64
}

// EphemeralStorageFor returns the ephemeral storage a node booting off a disk
// of this size has, and whether the disk is big enough to advertise any once
// the reservations and eviction threshold come off. kubelet is the nodeclass's
// reservations; the ephemeral-storage defaults do not vary by shape, so they
// are applied here. A diskSet mounted over the kubelet root makes this an
// under-estimate, which is the safe direction.
func EphemeralStorageFor(bootSizeBytes int64, kubelet Reserved) (EphemeralStorage, bool) {
	usable := bootSizeBytes - rootFSFixedBytes
	if usable <= 0 {
		return EphemeralStorage{}, false
	}
	available := usable - usable*rootFSLossBasisP/10000
	reserved := ReservedFor(Specs{}, kubelet)
	held := reserved.KubeReserved.StorageEphemeral().Value() +
		reserved.SystemReserved.StorageEphemeral().Value()
	if threshold, ok := reserved.EvictionHard[SignalNodeFSAvailable]; ok {
		held += threshold.Of(available)
	}
	if available-held <= 0 {
		return EphemeralStorage{}, false
	}
	return EphemeralStorage{Capacity: available}, true
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
