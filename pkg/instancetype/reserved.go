package instancetype

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	evictionMemBytes int64 = 100 * MiB

	// kubelet's own ephemeral-storage defaults, restated so that what the
	// scheduler models and what the node computes agree.
	ephemeralReservedBytes int64 = 1 * GiB
	evictionNodeFSPercent  int64 = 10

	// Eviction signals the overhead model understands. Others pass through to kubelet.
	SignalMemoryAvailable = "memory.available"
	SignalNodeFSAvailable = "nodefs.available"
)

// Threshold is an eviction threshold as kubelet takes it: an absolute quantity,
// or a percentage of the signal's capacity.
type Threshold struct {
	raw      string
	percent  float32
	quantity resource.Quantity
}

// ParseThreshold reads a threshold in kubelet's syntax, "100Mi" or "10%".
func ParseThreshold(s string) (Threshold, error) {
	if percent, ok := strings.CutSuffix(s, "%"); ok {
		value, err := strconv.ParseFloat(percent, 32)
		if err != nil || value < 0 {
			return Threshold{}, fmt.Errorf("invalid eviction threshold percentage %q", s)
		}
		return Threshold{raw: s, percent: float32(value / 100)}, nil
	}
	quantity, err := resource.ParseQuantity(s)
	if err != nil {
		return Threshold{}, fmt.Errorf("invalid eviction threshold %q: %w", s, err)
	}
	if quantity.Sign() < 0 {
		return Threshold{}, fmt.Errorf("invalid eviction threshold %q: must not be negative", s)
	}
	return Threshold{raw: quantity.String(), quantity: quantity}, nil
}

func quantityThreshold(quantity *resource.Quantity) Threshold {
	return Threshold{raw: quantity.String(), quantity: *quantity}
}

func percentThreshold(percent int64) Threshold {
	return Threshold{raw: fmt.Sprintf("%d%%", percent), percent: float32(percent) / 100}
}

// Of is the threshold applied to this much capacity, computed the way kubelet
// does so the modeled threshold never falls short of the node's.
func (t Threshold) Of(capacity int64) int64 {
	if t.percent > 0 {
		return int64(float64(capacity) * float64(t.percent))
	}
	return t.quantity.Value()
}

func (t Threshold) String() string { return t.raw }

// Reserved is what kubelet must be told to hold back so that a node's real
// allocatable matches the allocatable the scheduler was promised. Bootstrap
// renders these into kubelet flags; anything that changes here changes the
// instance type overhead in lockstep.
type Reserved struct {
	// KubeReserved is kube-reserved: cpu in milliCPU, memory and ephemeral-storage in bytes.
	KubeReserved corev1.ResourceList
	// SystemReserved is system-reserved, in the same units.
	SystemReserved corev1.ResourceList
	// EvictionHard maps an eviction signal to its hard threshold.
	EvictionHard map[string]Threshold
}

// DefaultMaxPods is what kubelet caps pods at when a nodeclass does not say.
const DefaultMaxPods = 110

// Kubelet is a nodeclass's kubelet configuration, parsed into what the catalog
// models and what is only rendered to the node.
type Kubelet struct {
	// Reserved is the nodeclass's reservations, layered over each shape's.
	Reserved Reserved
	// MaxPods and PodsPerCore cap the pods a shape advertises.
	MaxPods     *int32
	PodsPerCore *int32
	// Args are the rendered flags that change nothing the scheduler models.
	Args []string
}

// Pods is how many pods a shape with this many vCPUs can hold, computed as
// kubelet does: max-pods, further capped by pods-per-core when that is set.
func (k Kubelet) Pods(ncpus int) int64 {
	pods := int64(DefaultMaxPods)
	if k.MaxPods != nil {
		pods = int64(*k.MaxPods)
	}
	if k.PodsPerCore != nil && *k.PodsPerCore > 0 {
		pods = min(pods, int64(*k.PodsPerCore)*int64(ncpus))
	}
	return pods
}

// ParseKubelet reads a nodeclass's kubelet configuration.
func ParseKubelet(kc *v1alpha1.KubeletConfiguration) (Kubelet, error) {
	if kc == nil {
		return Kubelet{}, nil
	}
	reserved, err := parseReserved(kc)
	if err != nil {
		return Kubelet{}, err
	}
	return Kubelet{
		Reserved:    reserved,
		MaxPods:     kc.MaxPods,
		PodsPerCore: kc.PodsPerCore,
		Args:        renderArgs(kc),
	}, nil
}

// renderArgs renders the flags that only kubelet acts on, in a fixed order.
func renderArgs(kc *v1alpha1.KubeletConfiguration) []string {
	var args []string
	if len(kc.ClusterDNS) > 0 {
		args = append(args, "cluster-dns="+strings.Join(kc.ClusterDNS, ","))
	}
	if kc.MaxPods != nil {
		args = append(args, fmt.Sprintf("max-pods=%d", *kc.MaxPods))
	}
	if kc.PodsPerCore != nil {
		args = append(args, fmt.Sprintf("pods-per-core=%d", *kc.PodsPerCore))
	}
	if len(kc.EvictionSoft) > 0 {
		args = append(args, "eviction-soft="+renderSignals(kc.EvictionSoft, "<"))
	}
	if len(kc.EvictionSoftGracePeriod) > 0 {
		periods := make(map[string]string, len(kc.EvictionSoftGracePeriod))
		for signal, period := range kc.EvictionSoftGracePeriod {
			periods[signal] = period.Duration.String()
		}
		args = append(args, "eviction-soft-grace-period="+renderSignals(periods, "="))
	}
	if kc.EvictionMaxPodGracePeriod != nil {
		args = append(
			args,
			fmt.Sprintf("eviction-max-pod-grace-period=%d", *kc.EvictionMaxPodGracePeriod),
		)
	}
	if kc.ImageGCHighThresholdPercent != nil {
		args = append(
			args,
			fmt.Sprintf("image-gc-high-threshold=%d", *kc.ImageGCHighThresholdPercent),
		)
	}
	if kc.ImageGCLowThresholdPercent != nil {
		args = append(
			args,
			fmt.Sprintf("image-gc-low-threshold=%d", *kc.ImageGCLowThresholdPercent),
		)
	}
	if kc.CPUCFSQuota != nil {
		args = append(args, fmt.Sprintf("cpu-cfs-quota=%t", *kc.CPUCFSQuota))
	}
	return args
}

func renderSignals[V any](signals map[string]V, sep string) string {
	parts := make([]string, 0, len(signals))
	for _, signal := range orderedKeys(signals, SignalMemoryAvailable, SignalNodeFSAvailable) {
		parts = append(parts, fmt.Sprintf("%s%s%v", signal, sep, signals[signal]))
	}
	return strings.Join(parts, ",")
}

func parseReserved(kc *v1alpha1.KubeletConfiguration) (Reserved, error) {
	systemReserved, err := parseResourceList(kc.SystemReserved)
	if err != nil {
		return Reserved{}, fmt.Errorf("invalid systemReserved: %w", err)
	}
	kubeReserved, err := parseResourceList(kc.KubeReserved)
	if err != nil {
		return Reserved{}, fmt.Errorf("invalid kubeReserved: %w", err)
	}
	var evictionHard map[string]Threshold
	for signal, value := range kc.EvictionHard {
		threshold, err := ParseThreshold(string(value))
		if err != nil {
			return Reserved{}, fmt.Errorf("invalid evictionHard %s: %w", signal, err)
		}
		if evictionHard == nil {
			evictionHard = map[string]Threshold{}
		}
		evictionHard[signal] = threshold
	}
	return Reserved{
		KubeReserved:   kubeReserved,
		SystemReserved: systemReserved,
		EvictionHard:   evictionHard,
	}, nil
}

func parseResourceList(in map[string]v1alpha1.KubeletValue) (corev1.ResourceList, error) {
	if len(in) == 0 {
		return nil, nil
	}
	list := make(corev1.ResourceList, len(in))
	for name, value := range in {
		quantity, err := resource.ParseQuantity(string(value))
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", name, value, err)
		}
		if quantity.Sign() < 0 {
			return nil, fmt.Errorf("%s %q must not be negative", name, value)
		}
		list[corev1.ResourceName(name)] = quantity
	}
	return list, nil
}

// ReservedFor returns the reservations for a shape: the provider's own tiered
// defaults, with each key the nodeclass set replacing the default for it.
func ReservedFor(specs Specs, overrides Reserved) Reserved {
	reserved := Reserved{
		KubeReserved: corev1.ResourceList{
			corev1.ResourceCPU: *resource.NewMilliQuantity(
				cpuReserved(int64(specs.Ncpus)*1000), resource.DecimalSI,
			),
			corev1.ResourceMemory: *resource.NewQuantity(
				memReserved(int64(specs.MemInGiB)*GiB), resource.BinarySI,
			),
			corev1.ResourceEphemeralStorage: *resource.NewQuantity(
				ephemeralReservedBytes, resource.BinarySI,
			),
		},
		SystemReserved: corev1.ResourceList{},
		EvictionHard: map[string]Threshold{
			SignalMemoryAvailable: quantityThreshold(
				resource.NewQuantity(evictionMemBytes, resource.BinarySI),
			),
			SignalNodeFSAvailable: percentThreshold(evictionNodeFSPercent),
		},
	}
	maps.Copy(reserved.KubeReserved, overrides.KubeReserved)
	maps.Copy(reserved.SystemReserved, overrides.SystemReserved)
	maps.Copy(reserved.EvictionHard, overrides.EvictionHard)
	return reserved
}

// KubeletArgs renders the reservations as kubelet "name=value" arguments.
func (r Reserved) KubeletArgs() []string {
	var args []string
	if arg := renderResourceList(r.KubeReserved); arg != "" {
		args = append(args, "kube-reserved="+arg)
	}
	if arg := renderResourceList(r.SystemReserved); arg != "" {
		args = append(args, "system-reserved="+arg)
	}
	if len(r.EvictionHard) > 0 {
		args = append(args, "eviction-hard="+renderSignals(r.EvictionHard, "<"))
	}
	return args
}

func renderResourceList(list corev1.ResourceList) string {
	if len(list) == 0 {
		return ""
	}
	parts := make([]string, 0, len(list))
	for _, name := range orderedKeys(
		list,
		corev1.ResourceCPU,
		corev1.ResourceMemory,
		corev1.ResourceEphemeralStorage,
	) {
		quantity := list[name]
		parts = append(parts, string(name)+"="+quantity.String())
	}
	return strings.Join(parts, ",")
}

// orderedKeys lists m's keys with the well-known ones first, in the order given,
// and the rest sorted, so rendered flags are stable.
func orderedKeys[K ~string, V any](m map[K]V, first ...K) []K {
	keys := make([]K, 0, len(m))
	for _, key := range first {
		if _, ok := m[key]; ok {
			keys = append(keys, key)
		}
	}
	rest := slices.DeleteFunc(slices.Collect(maps.Keys(m)), func(key K) bool {
		return slices.Contains(first, key)
	})
	slices.Sort(rest)
	return append(keys, rest...)
}

// Options select the catalog for a nodeclass: the boot disk fixes the ephemeral
// storage shapes advertise, and the kubelet configuration fixes their overhead
// and pod capacity.
type Options struct {
	BootSizeBytes int64
	Kubelet       Kubelet
}

// OptionsFor returns the catalog options a nodeclass selects. A nil nodeclass
// selects the default catalog.
func OptionsFor(nodeClass *v1alpha1.OxideNodeClass) (Options, error) {
	if nodeClass == nil {
		return Options{}, nil
	}
	kubelet, err := ParseKubelet(nodeClass.Spec.Kubelet)
	if err != nil {
		return Options{}, err
	}
	return Options{BootSizeBytes: nodeClass.BootSizeBytes(), Kubelet: kubelet}, nil
}

func (o Options) key() string {
	return fmt.Sprintf(
		"%d|%s|%s",
		o.BootSizeBytes,
		strings.Join(o.Kubelet.Reserved.KubeletArgs(), " "),
		strings.Join(o.Kubelet.Args, " "),
	)
}
