package nodeclass

import (
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
)

func TestDescribeImageSelectors(t *testing.T) {
	tests := []struct {
		name      string
		selectors []v1alpha1.ImageSelector
		want      string
	}{
		{
			name: "no selectors",
			want: "no image selectors are configured",
		},
		{
			name:      "one selector reports only the terms that were set",
			selectors: []v1alpha1.ImageSelector{{OS: "ubuntu", Version: "1.2.*", Project: "prod"}},
			want:      "no images matched {os=ubuntu,version=1.2.*,project=prod}",
		},
		{
			name: "selectors are OR'd, and read that way",
			selectors: []v1alpha1.ImageSelector{
				{Name: "rke2-*"},
				{ID: "444f4178-6bee-45f7-a654-c6ecafd4bb9e"},
			},
			want: "no images matched {name=rke2-*} or {id=444f4178-6bee-45f7-a654-c6ecafd4bb9e}",
		},
		{
			name:      "an empty selector still renders",
			selectors: []v1alpha1.ImageSelector{{}},
			want:      "no images matched {}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describeImageSelectors(tt.selectors); got != tt.want {
				t.Errorf("describeImageSelectors() = %q, want %q", got, tt.want)
			}
		})
	}
}
