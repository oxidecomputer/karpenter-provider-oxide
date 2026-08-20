package nodeclass

import (
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
)

func TestDescribeAffinityGroupSelectors(t *testing.T) {
	tests := []struct {
		name      string
		selectors []v1alpha1.AffinityGroupSelector
		want      string
	}{
		{
			name:      "a name selector reports the name it looked for",
			selectors: []v1alpha1.AffinityGroupSelector{{Name: "rack-*"}},
			want:      "no groups matched {name=rack-*}",
		},
		{
			name: "selectors are OR'd, and read that way",
			selectors: []v1alpha1.AffinityGroupSelector{
				{ID: "444f4178-6bee-45f7-a654-c6ecafd4bb9e"},
				{Name: "control-plane-spread"},
			},
			want: "no groups matched {id=444f4178-6bee-45f7-a654-c6ecafd4bb9e} or {name=control-plane-spread}",
		},
		{
			name:      "an empty selector still renders",
			selectors: []v1alpha1.AffinityGroupSelector{{}},
			want:      "no groups matched {}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describeAffinityGroupSelectors(tt.selectors); got != tt.want {
				t.Errorf("describeAffinityGroupSelectors() = %q, want %q", got, tt.want)
			}
		})
	}
}
