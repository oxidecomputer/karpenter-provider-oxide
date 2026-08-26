package cloudprovider

import (
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/oxide.go/oxide"
)

func TestInstanceToNodeClaimAddsInstanceSchedulingLabels(t *testing.T) {
	provider := &OxideCloudProvider{
		instanceTypeProvider: instancetype.NewProvider(nil, nil),
	}
	nodeClaim, err := provider.InstanceToNodeClaim(&oxide.Instance{
		Hostname: "karpenter-node",
		Ncpus:    4,
		Memory:   oxide.ByteCount(16 * instancetype.GiB),
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]string{
		v1alpha1.LabelInstanceCPU:            "4",
		v1alpha1.LabelInstanceMemory:         "16384",
		v1alpha1.LabelInstanceCPUMemoryRatio: "1-4",
	}
	for label, want := range tests {
		if got := nodeClaim.Labels[label]; got != want {
			t.Errorf("%s = %q, want %q", label, got, want)
		}
	}
}
