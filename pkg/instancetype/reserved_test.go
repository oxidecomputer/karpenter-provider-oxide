package instancetype

import (
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestThresholdOfMatchesKubelet(t *testing.T) {
	percent, err := ParseThreshold("10%")
	if err != nil {
		t.Fatal(err)
	}
	// kubelet keeps the percentage as a float32, so 10% of 100GiB comes out a
	// few bytes over an exact tenth; the model must hold back at least that.
	const capacity = 100 * GiB
	if got, exact := percent.Of(capacity), int64(capacity/10); got < exact {
		t.Errorf("10%% of %d = %d, below the exact %d", capacity, got, exact)
	}
	if got := percent.String(); got != "10%" {
		t.Errorf("String() = %q", got)
	}

	absolute, err := ParseThreshold("1024Mi")
	if err != nil {
		t.Fatal(err)
	}
	if got := absolute.Of(capacity); got != GiB {
		t.Errorf("1024Mi of anything = %d, want %d", got, GiB)
	}
	if got := absolute.String(); got != "1Gi" {
		t.Errorf("String() = %q", got)
	}

	for _, bad := range []string{"", "-1Gi", "ten%", "-5%"} {
		if _, err := ParseThreshold(bad); err == nil {
			t.Errorf("ParseThreshold(%q) accepted", bad)
		}
	}
}

func TestReservedForLayersTheNodeClassOverTheShape(t *testing.T) {
	kubelet, err := ParseKubelet(&v1alpha1.KubeletConfiguration{
		SystemReserved: map[string]v1alpha1.KubeletValue{"cpu": "1"},
		KubeReserved:   map[string]v1alpha1.KubeletValue{"memory": "2Gi"},
		EvictionHard:   map[string]v1alpha1.KubeletValue{"nodefs.available": "5%"},
	})
	if err != nil {
		t.Fatal(err)
	}
	specs := Specs{Ncpus: 4, MemInGiB: 16}
	defaults := ReservedFor(specs, Reserved{})
	reserved := ReservedFor(specs, kubelet.Reserved)

	if got := reserved.SystemReserved.Cpu().MilliValue(); got != 1000 {
		t.Errorf("system-reserved cpu = %dm", got)
	}
	if got := reserved.KubeReserved.Memory().Value(); got != 2*GiB {
		t.Errorf("kube-reserved memory = %d, want the override", got)
	}
	if got, want := reserved.KubeReserved.Cpu(), defaults.KubeReserved.Cpu(); !got.Equal(*want) {
		t.Errorf("kube-reserved cpu = %s, want the shape's %s", got, want)
	}
	if got := reserved.EvictionHard[SignalNodeFSAvailable].String(); got != "5%" {
		t.Errorf("nodefs eviction = %s", got)
	}
	if got := reserved.EvictionHard[SignalMemoryAvailable].String(); got != "100Mi" {
		t.Errorf("memory eviction = %s, want the default", got)
	}
	if len(defaults.SystemReserved) != 0 {
		t.Errorf("the provider reserves %v for the system by default", defaults.SystemReserved)
	}
}

// Two nodeclasses with the same boot disk but different reservations share one
// catalog build; neither may see the other's overhead.
func TestCatalogIsCachedPerKubeletReservations(t *testing.T) {
	kubelet, err := ParseKubelet(&v1alpha1.KubeletConfiguration{
		SystemReserved: map[string]v1alpha1.KubeletValue{"memory": "1Gi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := NewProvider(nil, availability.New(nil))
	plain := Options{BootSizeBytes: 50 * GiB}
	reserved := Options{BootSizeBytes: 50 * GiB, Kubelet: kubelet}

	first := provider.InstanceTypes(plain)[0]
	second := provider.InstanceTypes(reserved)[0]
	again := provider.InstanceTypes(plain)[0]

	if !first.Overhead.SystemReserved.Memory().IsZero() ||
		!again.Overhead.SystemReserved.Memory().IsZero() {
		t.Fatalf("the plain catalog reserves %v for the system", again.Overhead.SystemReserved)
	}
	if got := second.Overhead.SystemReserved.Memory().Value(); got != GiB {
		t.Fatalf("the reserved catalog reserves %d for the system, want %d", got, GiB)
	}
	if first != again {
		t.Fatal("the plain catalog was rebuilt instead of served from cache")
	}
}

func TestEphemeralStorageHonorsTheNodeClassReservations(t *testing.T) {
	const bootSizeBytes = 10 * GiB
	if _, ok := EphemeralStorageFor(bootSizeBytes, Reserved{}); !ok {
		t.Fatal("a 10GiB boot disk should carry ephemeral storage by default")
	}
	kubelet, err := ParseKubelet(&v1alpha1.KubeletConfiguration{
		SystemReserved: map[string]v1alpha1.KubeletValue{"ephemeral-storage": "9Gi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := EphemeralStorageFor(bootSizeBytes, kubelet.Reserved); ok {
		t.Fatal("a 10GiB boot disk cannot cover 9Gi system-reserved on top of the defaults")
	}

	provider := NewProvider(nil, availability.New(nil))
	it, ok := provider.Get("4-16", Options{BootSizeBytes: bootSizeBytes, Kubelet: kubelet})
	if !ok {
		t.Fatal("4-16 not offered")
	}
	if _, advertised := it.Capacity[corev1.ResourceEphemeralStorage]; advertised {
		t.Fatal("a boot disk the reservations swallow must not advertise ephemeral storage")
	}
	if _, reserved := it.Overhead.SystemReserved[corev1.ResourceEphemeralStorage]; reserved {
		t.Fatal("overhead names ephemeral storage the shape does not advertise")
	}
}

func TestKubeletPodsMatchesKubelet(t *testing.T) {
	int32p := func(v int32) *int32 { return &v }
	for _, tc := range []struct {
		name    string
		kubelet Kubelet
		ncpus   int
		want    int64
	}{
		{name: "defaults", ncpus: 4, want: DefaultMaxPods},
		{name: "max-pods alone", kubelet: Kubelet{MaxPods: int32p(50)}, ncpus: 4, want: 50},
		{name: "pods-per-core below max-pods", kubelet: Kubelet{PodsPerCore: int32p(10)}, ncpus: 4, want: 40},
		{name: "pods-per-core above max-pods", kubelet: Kubelet{PodsPerCore: int32p(100)}, ncpus: 4, want: DefaultMaxPods},
		{name: "pods-per-core of zero is no cap", kubelet: Kubelet{PodsPerCore: int32p(0)}, ncpus: 1, want: DefaultMaxPods},
		{name: "both", kubelet: Kubelet{MaxPods: int32p(30), PodsPerCore: int32p(8)}, ncpus: 2, want: 16},
	} {
		if got := tc.kubelet.Pods(tc.ncpus); got != tc.want {
			t.Errorf("%s: Pods(%d) = %d, want %d", tc.name, tc.ncpus, got, tc.want)
		}
	}
}

func TestCatalogAdvertisesTheNodeClassPodCap(t *testing.T) {
	podsPerCore := int32(10)
	kubelet, err := ParseKubelet(&v1alpha1.KubeletConfiguration{PodsPerCore: &podsPerCore})
	if err != nil {
		t.Fatal(err)
	}
	provider := NewProvider(nil, availability.New(nil))
	plain, _ := provider.Get("4-16", Options{})
	capped, _ := provider.Get("4-16", Options{Kubelet: kubelet})
	if got := plain.Capacity[corev1.ResourcePods]; got.Value() != DefaultMaxPods {
		t.Errorf("default pods = %s", &got)
	}
	if got := capped.Capacity[corev1.ResourcePods]; got.Value() != 40 {
		t.Errorf("capped pods = %s, want 40", &got)
	}
}

func TestParseKubeletRendersTheFlagsKubeletActsOn(t *testing.T) {
	int32p := func(v int32) *int32 { return &v }
	quota := false
	kubelet, err := ParseKubelet(&v1alpha1.KubeletConfiguration{
		ClusterDNS:  []string{"10.96.0.10", "fd00::10"},
		MaxPods:     int32p(60),
		PodsPerCore: int32p(8),
		EvictionSoft: map[string]v1alpha1.KubeletValue{
			"imagefs.available": "15%", "memory.available": "500Mi",
		},
		EvictionSoftGracePeriod: map[string]metav1.Duration{
			"imagefs.available": {Duration: 2 * time.Minute},
			"memory.available":  {Duration: 90 * time.Second},
		},
		EvictionMaxPodGracePeriod:   int32p(30),
		ImageGCHighThresholdPercent: int32p(85),
		ImageGCLowThresholdPercent:  int32p(80),
		CPUCFSQuota:                 &quota,
		SystemReserved:              map[string]v1alpha1.KubeletValue{"memory": "1Gi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cluster-dns=10.96.0.10,fd00::10",
		"max-pods=60",
		"pods-per-core=8",
		"eviction-soft=memory.available<500Mi,imagefs.available<15%",
		"eviction-soft-grace-period=memory.available=1m30s,imagefs.available=2m0s",
		"eviction-max-pod-grace-period=30",
		"image-gc-high-threshold=85",
		"image-gc-low-threshold=80",
		"cpu-cfs-quota=false",
	}
	if strings.Join(kubelet.Args, "|") != strings.Join(want, "|") {
		t.Errorf("args = %v, want %v", kubelet.Args, want)
	}
	// Reservations are rendered per shape by bootstrap, never in Args.
	for _, arg := range kubelet.Args {
		if strings.HasPrefix(arg, "system-reserved") {
			t.Errorf("Args carries a reservation: %s", arg)
		}
	}
}
