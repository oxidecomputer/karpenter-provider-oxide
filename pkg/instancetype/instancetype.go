package instancetype

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

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

	// cpuCostWeight and memCostWeight turn a shape into the synthetic price Karpenter compares
	// offerings by. There is no dollar price on a rack, but consolidation only ever asks whether a
	// replacement is cheaper than what is running, so any cost that rises with the resources a shape
	// consumes is enough. The weights are scaled so that memory-heavy ratios cost more than
	// CPU-heavy ones at equal CPU, which matches which of the two runs a rack out of headroom first.
	cpuCostWeight = 1.0
	memCostWeight = 0.125
)

var (
	// memRatios is used to generate instance type shapes (1:2, 1:4, 1:8, etc.)
	memRatios = []int{2, 4, 8, 16, 32}
)

type Provider struct {
	mu            sync.Mutex
	instanceTypes []*cloudprovider.InstanceType
	// generation is the combined capacity and quota generation instanceTypes was built against, which
	// is what makes the list stale.
	generation generation
	built      bool

	capacity *CapacityManager
	// quotas is the silo's quota, or nil when quota awareness is switched off. It is consulted when
	// offerings are built so that a shape the silo is not entitled to is never offered in the first
	// place, rather than offered, launched, and refused.
	quotas QuotaProvider
}

// QuotaProvider is the silo quota this provider constrains its offerings by. It is an interface so
// that the instance type provider does not depend on how the quota is fetched, and so that quota
// awareness can be left out entirely by passing nil.
type QuotaProvider interface {
	// Quota returns the current quota, or an error if it has never been read. An error means "do not
	// constrain": a quota that cannot be read must not stop nodes from launching.
	Quota() (utilization.Quota, error)
	// Generation changes whenever the quota does.
	Generation() uint64
	// Refresh re-reads the quota. force bypasses the provider's own rate limit floor; callers reacting
	// to a refused launch pass false so that a burst of refusals collapses into one read.
	Refresh(ctx context.Context, force bool) (bool, error)
}

// generation is the pair of counters the built instance type list depends on. Both move
// independently, so staleness is only correctly detected by comparing them together.
type generation struct {
	capacity uint64
	quota    uint64
}

type Specs struct {
	Ncpus    int
	MemInGiB int
}

// NewProvider builds an instance type provider.
//
// Both dependencies are optional and are left out by passing nil: without quotas offerings are not
// constrained by the silo's entitlement, and without inventory they are not constrained by what the
// rack physically has room for. Neither absence is a degraded mode. A provider with neither still
// launches nodes and still learns from what the rack refuses, which is exactly how this provider
// behaved before either existed.
func NewProvider(quotas QuotaProvider, inventory InventoryProvider) *Provider {
	return &Provider{
		capacity: NewCapacityManager(inventory),
		quotas:   quotas,
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

// RecordICE takes a shape, and every shape at least as large, out of consideration until its backoff
// elapses. It is called when a launch of that shape is refused, with the reason the refusal was
// diagnosed as, and each consecutive refusal lengthens the wait.
func (i *Provider) RecordICE(ctx context.Context, specs Specs, reason Reason) {
	i.capacity.RecordICE(ctx, specs, reason)
}

// RecordSuccess reports that a launch of this shape succeeded, which clears the refusal history of
// every shape that success disproves.
func (i *Provider) RecordSuccess(ctx context.Context, specs Specs) {
	i.capacity.RecordSuccess(ctx, specs)
}

// RecordCapacityReleased reports that the rack has taken back the resources of an instance of this
// shape, which gives every shape that release could have made room for an immediate probe instead of
// the rest of its backoff.
func (i *Provider) RecordCapacityReleased(ctx context.Context, specs Specs) {
	i.capacity.RecordCapacityReleased(ctx, specs)
}

// TryAcquire reserves the right to launch this shape, and reports whether the caller got it. It only
// refuses when a shape whose backoff just elapsed is already being probed by another launch, in which
// case the caller should pick a different shape rather than pile onto the probe.
func (i *Provider) TryAcquire(specs Specs) bool {
	return i.capacity.TryAcquire(specs)
}

// ReleaseProbe hands back a reservation taken by TryAcquire for a launch that never reached the rack.
func (i *Provider) ReleaseProbe(specs Specs) {
	i.capacity.ReleaseProbe(specs)
}

// IsUnavailable reports whether a shape is currently ruled out, and why.
//
// A shape is ruled out either because a launch of it was refused recently, or because the silo's
// quota has no room for it. The quota is checked live rather than through the capacity manager so
// that raising a quota takes effect on the next refresh instead of waiting out a backoff.
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

// Quota returns the silo's quota, for callers that need to explain a refusal in terms of it. The
// error is utilization.ErrNoSnapshot when the quota is unknown or quota awareness is off.
func (i *Provider) Quota() (utilization.Quota, error) {
	return i.quota()
}

// RefreshQuota re-reads the silo's quota, for callers that have just learned the snapshot was wrong.
//
// A launch refused for quota is direct evidence that the headroom is smaller than the last read said,
// usually because a concurrent launch consumed it. Re-reading turns that one failure into an accurate
// picture for every subsequent decision, rather than letting the rest of the burst fail the same way.
// Errors are swallowed: the caller is already returning a failure and a stale quota is what it
// currently has either way.
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

// requestFor is what launching one instance of this shape adds to the silo's provisioned CPU and
// memory.
//
// Storage is deliberately left at zero. The boot disk size comes from the OxideNodeClass rather than
// the shape, so it is not knowable here, and claiming zero storage is the honest answer for a
// question about an instance type rather than about a particular launch. The storage quota is checked
// at launch time, where the disk size is known.
func requestFor(specs Specs) utilization.Request {
	return utilization.Request{
		CPUs:   int64(specs.Ncpus),
		Memory: uint64(specs.MemInGiB) * GiB,
	}
}

// InstanceTypes returns every instance type, with the offerings of shapes that cannot currently be
// launched marked unavailable so scheduling passes over them.
//
// A shape is unavailable because a launch of it was recently refused, because the silo's quota has no
// room for it, or because no sled on the rack has room for it. Each is a different way of being
// unlaunchable and each is the reason to consult its source at all: an unconstrained list offers the
// scheduler shapes up to a whole sled, and a silo entitled to 32 vCPUs, or a rack with 8 CPUs free on
// its emptiest sled, would otherwise have Karpenter confidently pick a 256 vCPU node, create a
// NodeClaim, fail, and delete it.
//
// The list is rebuilt rather than updated in place, since the one already returned is read
// concurrently by the scheduler. Rebuilding only happens when the unavailable set, the rack reading or
// the quota has actually changed, so the steady state is a cache hit.
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
	// Read once for the whole build rather than per shape. There are thousands of shapes and they all
	// have to be judged against the same quota, or the list is internally inconsistent.
	quota, quotaErr := i.quota()
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
			specs := Specs{Ncpus: cpus, MemInGiB: mem}
			refused, _ := availability.Unavailable(specs)
			// A quota that could not be read constrains nothing. Failing open here is deliberate: the
			// alternative is that a permissions problem or an API blip stops the cluster from scaling.
			overQuota := false
			if quotaErr == nil {
				admits, _ := quota.Admits(requestFor(specs))
				overQuota = !admits
			}
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
							Available: !refused && !overQuota,
							// Deliberately independent of Available and of the quota: price has to stay
							// stable across rebuilds or the disruption controller churns every time quota
							// moves.
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
		}
	}
	i.instanceTypes = instanceTypes
	i.generation = current
	i.built = true
	return instanceTypes
}

// price is the synthetic hourly cost of a shape. It is strictly increasing in both CPU and memory,
// which is what stops consolidation from oscillating between two shapes it would otherwise consider
// equally cheap. The absolute scale carries no meaning; only the ordering does.
func price(specs Specs) float64 {
	return float64(specs.Ncpus)*cpuCostWeight + float64(specs.MemInGiB)*memCostWeight
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
