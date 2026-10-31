package affinitygroup

import (
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

const defaultNodeClass = "default"

const testUID = "444f4178-6bee-45f7-a654-c6ecafd4bb9e"

const testUIDSuffix = "444f4178"

func TestManagedGroupName(t *testing.T) {
	tests := []struct {
		name          string
		nodeClassName string
		uid           string
		managed       v1alpha1.ManagedAffinityGroup
		want          string
		wantErr       bool
	}{
		{
			name:          "an allow-policy anti-affinity group",
			nodeClassName: defaultNodeClass,
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity", Policy: "allow"},
			want:          "default-aa-a-" + testUIDSuffix,
		},
		{
			name:          "the policy is encoded, so a change to it names a different group",
			nodeClassName: defaultNodeClass,
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity", Policy: "fail"},
			want:          "default-aa-f-" + testUIDSuffix,
		},
		{
			name:          "the type is encoded for the same reason",
			nodeClassName: "gpu-nodes",
			managed:       v1alpha1.ManagedAffinityGroup{Type: "Affinity", Policy: "fail"},
			want:          "gpu-nodes-af-f-" + testUIDSuffix,
		},
		{
			name:          "an unset policy names the group the API would have defaulted to",
			nodeClassName: defaultNodeClass,
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity"},
			want:          "default-aa-a-" + testUIDSuffix,
		},
		{
			name:          "the UID distinguishes two nodeclasses sharing a name and a project",
			nodeClassName: defaultNodeClass,
			uid:           "9a1b2c3d-0000-0000-0000-000000000000",
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity"},
			want:          "default-aa-a-9a1b2c3d",
		},
		{
			name:          "49 characters is the longest nodeclass name the suffix leaves room for",
			nodeClassName: "a" + strings.Repeat("b", 48),
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity"},
			want:          "a" + strings.Repeat("b", 48) + "-aa-a-" + testUIDSuffix,
		},
		{
			name:          "a longer nodeclass name is truncated, since the UID still separates it",
			nodeClassName: "a" + strings.Repeat("b", 60),
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity"},
			want:          "a" + strings.Repeat("b", 48) + "-aa-a-" + testUIDSuffix,
		},
		{
			name:          "truncation does not leave a trailing hyphen before the suffix",
			nodeClassName: "a" + strings.Repeat("b", 47) + "--nodes",
			managed:       v1alpha1.ManagedAffinityGroup{Type: "AntiAffinity"},
			want:          "a" + strings.Repeat("b", 47) + "-aa-a-" + testUIDSuffix,
		},
		{
			name:          "a nodeclass without a UID cannot name a group",
			nodeClassName: defaultNodeClass,
			uid:           "-",
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
			uid := tt.uid
			if uid == "" {
				uid = testUID
			}
			nodeClass := &v1alpha1.OxideNodeClass{
				Name: tt.nodeClassName,
				UID:  types.UID(uid),
				Spec: v1alpha1.OxideNodeClassSpec{
					Affinity: &v1alpha1.Affinity{Managed: &tt.managed},
				},
			}
			got, err := managedGroupName(nodeClass)
			if (err != nil) != tt.wantErr {
				t.Fatalf("managedGroupName() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("managedGroupName() = %q, want %q", got, tt.want)
			}
			if len(got) > maxOxideNameLength {
				t.Errorf("managedGroupName() is %d characters, over the limit", len(got))
			}
		})
	}
}

func TestManagedGroupDescription(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{
		Name: defaultNodeClass,
		UID:  testUID,
	}
	want := "karpenter.oxide.computer managed group for oxidenodeclass/default (444f4178-6bee-45f7-a654-c6ecafd4bb9e)"
	if got := managedGroupDescription(nodeClass); got != want {
		t.Errorf("managedGroupDescription() = %q, want %q", got, want)
	}
}
