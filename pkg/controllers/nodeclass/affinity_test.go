package nodeclass

import (
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// defaultNodeClass is the nodeclass name most of these cases share, so that what varies between
// them is only the managed group configuration.
const defaultNodeClass = "default"

func TestManagedGroupName(t *testing.T) {
	tests := []struct {
		name          string
		nodeClassName string
		managed       v1alpha1.ManagedAffinityGroup
		want          string
		wantErr       bool
	}{
		{
			name:          "an allow-policy anti-affinity group",
			nodeClassName: defaultNodeClass,
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity", Policy: "allow"},
			want:          "default-aa-a",
		},
		{
			name:          "the policy is encoded, so a change to it names a different group",
			nodeClassName: defaultNodeClass,
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity", Policy: "fail"},
			want:          "default-aa-f",
		},
		{
			name:          "the type is encoded for the same reason",
			nodeClassName: "gpu-nodes",
			managed:       v1alpha1.ManagedAffinityGroup{Type: "Affinity", Policy: "fail"},
			want:          "gpu-nodes-af-f",
		},
		{
			name:          "an unset policy names the group the API would have defaulted to",
			nodeClassName: defaultNodeClass,
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity"},
			want:          "default-aa-a",
		},
		{
			name:          "58 characters is the longest nodeclass name the suffix leaves room for",
			nodeClassName: "a" + strings.Repeat("b", 57),
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity"},
			want:          "a" + strings.Repeat("b", 57) + "-aa-a",
		},
		{
			name:          "59 characters is reported rather than truncated",
			nodeClassName: "a" + strings.Repeat("b", 58),
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity"},
			wantErr:       true,
		},
		{
			name:          "a dot is legal in a Kubernetes name and not in an Oxide one",
			nodeClassName: "team.default",
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity"},
			wantErr:       true,
		},
		{
			name:          "a name has to begin with a lowercase ASCII letter",
			nodeClassName: "9lives",
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity"},
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClass := &v1alpha1.OxideNodeClass{
				ObjectMeta: metav1.ObjectMeta{Name: tt.nodeClassName},
				Spec: v1alpha1.OxideNodeClassSpec{
					Affinity: &v1alpha1.Affinity{Managed: &tt.managed},
				},
			}
			got, err := managedGroupName(nodeClass)
			if (err != nil) != tt.wantErr {
				t.Fatalf("managedGroupName() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("managedGroupName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestManagedGroupDescription(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: defaultNodeClass,
			UID:  "444f4178-6bee-45f7-a654-c6ecafd4bb9e",
		},
	}
	want := "karpenter.oxide.computer managed group for oxidenodeclass/default (444f4178-6bee-45f7-a654-c6ecafd4bb9e)"
	if got := managedGroupDescription(nodeClass); got != want {
		t.Errorf("managedGroupDescription() = %q, want %q", got, want)
	}
}

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
