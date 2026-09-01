package bootstrap_test

import (
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
)

func TestKubeletArgsCarriesShapeReservations(t *testing.T) {
	args := bootstrap.KubeletArgs(
		instancetype.Specs{Ncpus: 9, MemInGiB: 5},
		[]string{"cloud-provider=external"},
	)
	want := []string{
		"kube-reserved=cpu=92m,memory=1288490188",
		"eviction-hard=memory.available<100Mi",
		"cloud-provider=external",
	}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", args, want)
	}
}

func TestKubeletArgsNodeClassOverridesReservations(t *testing.T) {
	args := bootstrap.KubeletArgs(
		instancetype.Specs{Ncpus: 9, MemInGiB: 5},
		[]string{"kube-reserved=cpu=1,memory=1Gi"},
	)
	if len(args) != 2 {
		t.Fatalf("expected the override to replace in place, got %v", args)
	}
	if args[0] != "kube-reserved=cpu=1,memory=1Gi" {
		t.Fatalf("nodeclass argument did not win: %v", args)
	}
}

func TestKubeletArgsWithoutShapeReservesNothing(t *testing.T) {
	args := bootstrap.KubeletArgs(instancetype.Specs{}, []string{"cloud-provider=external"})
	if len(args) != 1 || args[0] != "cloud-provider=external" {
		t.Fatalf("got %v", args)
	}
}

// The scheduler may never over-promise allocatable relative to what kubelet will
// compute on the node, or pods land on a node that cannot hold them.
func TestReservationsMatchInstanceTypeOverhead(t *testing.T) {
	specs := instancetype.Specs{Ncpus: 9, MemInGiB: 5}
	reserved := instancetype.ReservedFor(specs)

	provider := instancetype.NewProvider(nil, availability.New(nil))
	it, ok := provider.Get("9-5")
	if !ok {
		t.Fatal("9-5 not offered")
	}
	if got := it.Overhead.KubeReserved.Cpu().MilliValue(); got != reserved.CPU {
		t.Fatalf("overhead cpu %dm != reserved %dm", got, reserved.CPU)
	}
	if got := it.Overhead.KubeReserved.Memory().Value(); got != reserved.Memory {
		t.Fatalf("overhead memory %d != reserved %d", got, reserved.Memory)
	}
	if got := it.Overhead.EvictionThreshold.Memory().Value(); got != reserved.EvictionMemory {
		t.Fatalf("eviction %d != reserved %d", got, reserved.EvictionMemory)
	}
}
