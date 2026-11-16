package instancetype

import (
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// The 1-4 a Rocky 9.8 node registered as, crashkernel included.
const observed14 = 3737792 * 1024

func observedNodeClass(
	images []string,
	observed ...v1alpha1.ObservedCapacity,
) *v1alpha1.OxideNodeClass {
	nodeClass := &v1alpha1.OxideNodeClass{}
	for _, id := range images {
		nodeClass.Status.Images = append(nodeClass.Status.Images, v1alpha1.OxideImage{ID: id})
	}
	nodeClass.Status.ObservedCapacity = observed
	return nodeClass
}

func observedEntry(instanceType, imageID string, memory int64) v1alpha1.ObservedCapacity {
	return v1alpha1.ObservedCapacity{
		InstanceType: instanceType,
		ImageID:      imageID,
		Memory:       *resource.NewQuantity(memory, resource.BinarySI),
	}
}

func TestObservedGuestMemoryReplacesTheEstimate(t *testing.T) {
	provider := NewProvider(nil, availability.New(nil))
	opts, err := OptionsFor(observedNodeClass(
		[]string{"new"},
		observedEntry("1-4", "new", observed14),
	))
	if err != nil {
		t.Fatal(err)
	}

	it, ok := provider.Get("1-4", opts)
	if !ok {
		t.Fatal("1-4 not offered")
	}
	if got := it.Capacity.Memory().Value(); got != observed14 {
		t.Fatalf("capacity memory = %d, want the observed %d", got, observed14)
	}
	// Allocatable is what the scheduler fits pods against; the bug was 2762Mi modeled, 2526Mi real.
	allocatable := it.Allocatable()
	if got := allocatable.Memory().Value(); got != 2586816*1024 {
		t.Fatalf("allocatable memory = %dKi, want 2586816Ki", got/1024)
	}

	other, ok := provider.Get("2-4", opts)
	if !ok {
		t.Fatal("2-4 not offered")
	}
	if got, want := other.Capacity.Memory().Value(), guestMemory(4); got != want {
		t.Fatalf("an unobserved shape has capacity %d, want the estimate %d", got, want)
	}
}

func TestObservedGuestMemoryOnlyAppliesToTheCurrentImage(t *testing.T) {
	provider := NewProvider(nil, availability.New(nil))
	opts, err := OptionsFor(observedNodeClass(
		[]string{"new", "old"},
		observedEntry("1-4", "old", observed14),
	))
	if err != nil {
		t.Fatal(err)
	}
	if opts.GuestMemory != nil {
		t.Fatalf("an older image's observation leaked into the options: %v", opts.GuestMemory)
	}
	it, _ := provider.Get("1-4", opts)
	if got, want := it.Capacity.Memory().Value(), guestMemory(4); got != want {
		t.Fatalf("capacity memory = %d, want the estimate %d", got, want)
	}
}

func TestCatalogIsCachedPerObservation(t *testing.T) {
	provider := NewProvider(nil, availability.New(nil))
	estimated, _ := provider.Get("1-4", Options{})
	observed, _ := provider.Get("1-4", Options{GuestMemory: map[string]int64{"1-4": observed14}})
	if estimated.Capacity.Memory().Value() == observed.Capacity.Memory().Value() {
		t.Fatal("an observation was served the catalog built without it")
	}
	again, _ := provider.Get("1-4", Options{})
	if again != estimated {
		t.Fatal("the estimated catalog was rebuilt or replaced")
	}
}

func TestObservedGuestMemoryDrivesTheMemoryEvictionThreshold(t *testing.T) {
	provider := NewProvider(nil, availability.New(nil))
	kubelet, err := ParseKubelet(&v1alpha1.KubeletConfiguration{
		EvictionHard: map[string]v1alpha1.KubeletValue{SignalMemoryAvailable: "10%"},
	})
	if err != nil {
		t.Fatal(err)
	}
	it, _ := provider.Get("1-4", Options{
		Kubelet:     kubelet,
		GuestMemory: map[string]int64{"1-4": observed14},
	})
	threshold := it.Overhead.EvictionThreshold[corev1.ResourceMemory]
	want := kubelet.Reserved.EvictionHard[SignalMemoryAvailable].Of(observed14)
	if got := threshold.Value(); got != want {
		t.Fatalf("memory eviction threshold = %d, want 10%% of the observed capacity %d", got, want)
	}
}
