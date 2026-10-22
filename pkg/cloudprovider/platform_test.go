package cloudprovider

import (
	"slices"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestCPUPlatformsForFollowsTheNodeClaimRequirement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator corev1.NodeSelectorOperator
		values   []string
		want     []availability.Platform
		wantErr  bool
	}{
		{name: "silence takes the default pin", want: []availability.Platform{"amd_milan"}},
		{
			name:     "one value pins it",
			operator: corev1.NodeSelectorOpIn,
			values:   []string{"amd_milan"},
			want:     []availability.Platform{"amd_milan"},
		},
		{
			name:     "several values keep the provider's order",
			operator: corev1.NodeSelectorOpIn,
			values:   []string{"amd_milan", "amd_turin_v2"},
			want:     []availability.Platform{"amd_turin_v2", "amd_milan"},
		},
		{
			name:     "NotIn leaves the rest",
			operator: corev1.NodeSelectorOpNotIn,
			values:   []string{"amd_milan"},
			want:     []availability.Platform{"amd_turin_v2", "amd_turin"},
		},
		{
			name:     "Exists allows every pin",
			operator: corev1.NodeSelectorOpExists,
			want:     []availability.Platform{"amd_turin_v2", "amd_turin", "amd_milan"},
		},
		{
			name:     "a platform the provider lacks is an error",
			operator: corev1.NodeSelectorOpIn,
			values:   []string{"amd_venice"},
			wantErr:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeClaim := &karpcorev1.NodeClaim{}
			if tc.operator != "" {
				nodeClaim.Spec.Requirements = []karpcorev1.NodeSelectorRequirementWithMinValues{{
					Key:      v1alpha1.LabelInstanceCPUPlatform,
					Operator: tc.operator,
					Values:   tc.values,
				}}
			}
			got, err := cpuPlatformsFor(nodeClaim)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("platforms = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInstanceToNodeClaimLabelsOnlyAPinnedPlatform(t *testing.T) {
	provider := &OxideCloudProvider{
		instanceTypeProvider: instancetype.NewProvider(nil, availability.New(nil)),
	}
	for _, tc := range []struct {
		pin  oxide.InstanceCpuPlatform
		want string
	}{
		{oxide.InstanceCpuPlatformAmdTurinV2, "amd_turin_v2"},
		{"", ""},
	} {
		nodeClaim, err := provider.InstanceToNodeClaim(&oxide.Instance{
			Hostname:    "karpenter-node",
			Ncpus:       4,
			Memory:      oxide.ByteCount(16 * instancetype.GiB),
			CpuPlatform: tc.pin,
		}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, present := nodeClaim.Labels[v1alpha1.LabelInstanceCPUPlatform]
		if got != tc.want || present != (tc.want != "") {
			t.Errorf("pin %q: label = %q (present %v), want %q", tc.pin, got, present, tc.want)
		}
		if zone := nodeClaim.Labels[corev1.LabelTopologyZone]; zone != instancetype.ZoneName {
			t.Errorf("pin %q: zone label = %q, want %q", tc.pin, zone, instancetype.ZoneName)
		}
	}
}
