package bootstrap_test

import (
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	corev1 "k8s.io/api/core/v1"
)

func TestKubeletArgsCarriesShapeReservations(t *testing.T) {
	args := bootstrap.KubeletArgs(instancetype.Specs{Ncpus: 9, MemInGiB: 5},
		instancetype.Reserved{},
		[]string{"cloud-provider=external"},
	)
	want := []string{
		"kube-reserved=cpu=92m,memory=1288490188,ephemeral-storage=1Gi",
		"eviction-hard=memory.available<100Mi,nodefs.available<10%",
		"cloud-provider=external",
	}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", args, want)
	}
}

func TestKubeletArgsCarriesNodeClassReservations(t *testing.T) {
	kubelet, err := instancetype.ParseKubelet(&v1alpha1.KubeletConfiguration{
		SystemReserved: map[string]string{"cpu": "500m", "memory": "1Gi", "pid": "4096"},
		KubeReserved:   map[string]string{"memory": "2Gi"},
		EvictionHard:   map[string]string{"memory.available": "5%", "imagefs.available": "15%"},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := bootstrap.KubeletArgs(instancetype.Specs{Ncpus: 9, MemInGiB: 5},
		kubelet.Reserved,
		[]string{"cloud-provider=external"},
	)
	want := []string{
		"kube-reserved=cpu=92m,memory=2Gi,ephemeral-storage=1Gi",
		"system-reserved=cpu=500m,memory=1Gi,pid=4096",
		"eviction-hard=memory.available<5%,nodefs.available<10%,imagefs.available<15%",
		"cloud-provider=external",
	}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", args, want)
	}
}

func TestKubeletArgsWithoutShapeReservesOnlyWhatTheNodeClassAsks(t *testing.T) {
	args := bootstrap.KubeletArgs(instancetype.Specs{},
		instancetype.Reserved{},
		[]string{"cloud-provider=external"},
	)
	if len(args) != 1 || args[0] != "cloud-provider=external" {
		t.Fatalf("got %v", args)
	}

	kubelet, err := instancetype.ParseKubelet(&v1alpha1.KubeletConfiguration{
		SystemReserved: map[string]string{"memory": "1Gi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	args = bootstrap.KubeletArgs(instancetype.Specs{}, kubelet.Reserved, nil)
	if len(args) != 1 || args[0] != "system-reserved=memory=1Gi" {
		t.Fatalf("got %v", args)
	}
}

// The scheduler may never over-promise allocatable relative to what kubelet will
// compute on the node, or pods land on a node that cannot hold them.
func TestReservationsMatchInstanceTypeOverhead(t *testing.T) {
	const bootSizeBytes = 100 << 30
	specs := instancetype.Specs{Ncpus: 9, MemInGiB: 5}

	for name, kubelet := range map[string]*v1alpha1.KubeletConfiguration{
		"defaults": nil,
		"overrides": {
			SystemReserved: map[string]string{"cpu": "250m", "memory": "512Mi", "ephemeral-storage": "2Gi"},
			KubeReserved:   map[string]string{"cpu": "1", "ephemeral-storage": "3Gi"},
			EvictionHard:   map[string]string{"memory.available": "10%", "nodefs.available": "4Gi"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			overrides, err := instancetype.ParseKubelet(kubelet)
			if err != nil {
				t.Fatal(err)
			}
			reserved := instancetype.ReservedFor(specs, overrides.Reserved)
			ephemeral, hasEphemeral := instancetype.EphemeralStorageFor(
				bootSizeBytes,
				overrides.Reserved,
			)
			if !hasEphemeral {
				t.Fatal("a 100GiB boot disk should carry ephemeral storage")
			}

			provider := instancetype.NewProvider(nil, availability.New(nil))
			it, ok := provider.Get("9-5", instancetype.Options{
				BootSizeBytes: bootSizeBytes,
				Kubelet:       overrides,
			})
			if !ok {
				t.Fatal("9-5 not offered")
			}
			for _, name := range []corev1.ResourceName{
				corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage,
			} {
				if got, want := it.Overhead.KubeReserved[name], reserved.KubeReserved[name]; !got.Equal(
					want,
				) {
					t.Errorf("kube-reserved %s: overhead %s != reserved %s", name, &got, &want)
				}
				if got, want := it.Overhead.SystemReserved[name], reserved.SystemReserved[name]; !got.Equal(
					want,
				) {
					t.Errorf("system-reserved %s: overhead %s != reserved %s", name, &got, &want)
				}
			}
			memory := it.Capacity[corev1.ResourceMemory]
			if got, want := it.Overhead.EvictionThreshold.Memory().Value(),
				reserved.EvictionHard[instancetype.SignalMemoryAvailable].Of(
					memory.Value(),
				); got != want {
				t.Errorf("eviction memory %d != %d", got, want)
			}
			if got, want := it.Overhead.EvictionThreshold.StorageEphemeral().Value(),
				reserved.EvictionHard[instancetype.SignalNodeFSAvailable].Of(
					ephemeral.Capacity,
				); got != want {
				t.Errorf("eviction nodefs %d != %d", got, want)
			}
		})
	}
}
