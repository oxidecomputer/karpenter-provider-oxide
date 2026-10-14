package instancetype

import (
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	corev1 "k8s.io/api/core/v1"
)

func TestEphemeralStorageIsAdvertisedFromTheBootDisk(t *testing.T) {
	const bootSizeBytes = 50 * GiB
	provider := NewProvider(nil, availability.New(nil))
	it, ok := provider.Get("4-16", Options{BootSizeBytes: bootSizeBytes})
	if !ok {
		t.Fatal("4-16 not offered")
	}

	capacity := it.Capacity[corev1.ResourceEphemeralStorage]
	if capacity.IsZero() {
		t.Fatal("ephemeral storage is not advertised")
	}
	if capacity.Value() >= bootSizeBytes {
		t.Fatalf(
			"ephemeral storage %d must be less than the boot disk %d",
			capacity.Value(),
			bootSizeBytes,
		)
	}

	allocatable := it.Allocatable()[corev1.ResourceEphemeralStorage]
	if allocatable.Value() <= 0 || allocatable.Value() >= capacity.Value() {
		t.Fatalf(
			"allocatable ephemeral storage %d must be in (0, %d)",
			allocatable.Value(),
			capacity.Value(),
		)
	}
}

// A pod requesting ephemeral storage must fit a shape whose only constraint is
// the boot disk; before ephemeral storage was modeled, a missing key read as
// zero and every shape was rejected.
func TestEphemeralStorageScalesWithTheBootDisk(t *testing.T) {
	provider := NewProvider(nil, availability.New(nil))
	small, ok := provider.Get("4-16", Options{BootSizeBytes: 20 * GiB})
	if !ok {
		t.Fatal("4-16 not offered")
	}
	large, ok := provider.Get("4-16", Options{BootSizeBytes: 200 * GiB})
	if !ok {
		t.Fatal("4-16 not offered")
	}

	smallCapacity := small.Capacity[corev1.ResourceEphemeralStorage]
	largeCapacity := large.Capacity[corev1.ResourceEphemeralStorage]
	if smallCapacity.Value() >= largeCapacity.Value() {
		t.Fatalf(
			"ephemeral storage did not grow with the boot disk: %d then %d",
			smallCapacity.Value(),
			largeCapacity.Value(),
		)
	}
	if smallMem, largeMem := small.Capacity.Memory(), large.Capacity.Memory(); !smallMem.Equal(
		*largeMem,
	) {
		t.Fatalf("the boot disk changed memory: %s then %s", smallMem, largeMem)
	}
}

func TestEphemeralStorageOmittedWhenTheBootDiskCannotCoverIt(t *testing.T) {
	for _, bootSizeBytes := range []int64{0, -1, GiB, GiB + MiB} {
		if _, ok := EphemeralStorageFor(bootSizeBytes, Reserved{}); ok {
			t.Errorf("boot disk of %d bytes should carry no ephemeral storage", bootSizeBytes)
		}
	}

	provider := NewProvider(nil, availability.New(nil))
	it, ok := provider.Get("4-16", Options{})
	if !ok {
		t.Fatal("4-16 not offered")
	}
	if _, advertised := it.Capacity[corev1.ResourceEphemeralStorage]; advertised {
		t.Fatal("an unresolved boot disk must not advertise ephemeral storage")
	}
	if _, reserved := it.Overhead.KubeReserved[corev1.ResourceEphemeralStorage]; reserved {
		t.Fatal("an unresolved boot disk must not reserve ephemeral storage")
	}
}

// Two nodeclasses with different boot disks share one catalog build; neither
// may see the other's storage.
func TestEphemeralStorageIsCachedPerBootDiskSize(t *testing.T) {
	provider := NewProvider(nil, availability.New(nil))
	first := provider.InstanceTypes(Options{BootSizeBytes: 50 * GiB})[0]
	second := provider.InstanceTypes(Options{BootSizeBytes: 100 * GiB})[0]
	again := provider.InstanceTypes(Options{BootSizeBytes: 50 * GiB})[0]

	firstCapacity := first.Capacity[corev1.ResourceEphemeralStorage]
	secondCapacity := second.Capacity[corev1.ResourceEphemeralStorage]
	againCapacity := again.Capacity[corev1.ResourceEphemeralStorage]
	if firstCapacity.Equal(secondCapacity) {
		t.Fatal("both boot disk sizes produced the same ephemeral storage")
	}
	if !firstCapacity.Equal(againCapacity) {
		t.Fatalf("cached catalog changed: %s then %s", &firstCapacity, &againCapacity)
	}
}
