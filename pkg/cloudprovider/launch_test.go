package cloudprovider

import (
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/oxide.go/oxide"
)

// The regression this whole shape exists for: the number of external addresses a launch asks for is
// the number of entries the nodeclass declares, and nothing else. Deriving it from the pools the silo
// happens to contain produced a launch that Nexus refuses outright, since an instance may have at
// most one ephemeral address per IP version.
func TestExternalIPsIsOneAttachmentPerEntry(t *testing.T) {
	tests := []struct {
		name         string
		ephemeralIPs []v1alpha1.OxideEphemeralIP
	}{
		{
			name: "a nodeclass declaring none asks for none",
		},
		{
			name:         "one entry is one attachment",
			ephemeralIPs: []v1alpha1.OxideEphemeralIP{{IPVersion: "v4", PoolName: "public-v4"}},
		},
		{
			name: "and a dual-stack pair is two",
			ephemeralIPs: []v1alpha1.OxideEphemeralIP{
				{IPVersion: "v4"},
				{IPVersion: "v6", PoolName: "public-v6"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := externalIPs(tt.ephemeralIPs); len(got) != len(tt.ephemeralIPs) {
				t.Errorf(
					"externalIPs() asked for %d attachments, want one per declared entry (%d)",
					len(got),
					len(tt.ephemeralIPs),
				)
			}
		})
	}
}

// The two ways an entry names its pool are the two variants of Oxide's PoolSelector. Auto is the one
// the previous shape had no way to express at all: it is what "the silo's default for this version"
// is spelled as, and Nexus needs the version on it to choose between two default pools.
func TestExternalIPsSelectsThePoolTheEntryNamed(t *testing.T) {
	tests := []struct {
		name        string
		ephemeralIP v1alpha1.OxideEphemeralIP
		want        oxide.PoolSelector
	}{
		{
			name:        "an entry with a pool selects it explicitly",
			ephemeralIP: v1alpha1.OxideEphemeralIP{IPVersion: "v4", PoolName: "public-v4"},
			want: oxide.PoolSelector{
				Value: &oxide.PoolSelectorExplicit{Pool: oxide.NameOrId("public-v4")},
			},
		},
		{
			name:        "an entry with no pool takes the silo default for its version",
			ephemeralIP: v1alpha1.OxideEphemeralIP{IPVersion: "v6"},
			want: oxide.PoolSelector{
				Value: &oxide.PoolSelectorAuto{IpVersion: oxide.IpVersionV6},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := externalIPs([]v1alpha1.OxideEphemeralIP{tt.ephemeralIP})
			if len(got) != 1 {
				t.Fatalf("externalIPs() = %v, want one attachment", got)
			}
			ephemeral, ok := got[0].Value.(*oxide.ExternalIpCreateEphemeral)
			if !ok {
				// Floating IPs are deliberately not templatable: a floating address has one parent, so
				// a nodeclass carrying one would deadlock its own drift the moment Karpenter launched
				// a replacement before terminating the node it replaces.
				t.Fatalf("externalIPs() asked for %T, want an ephemeral attachment", got[0].Value)
			}
			if ephemeral.PoolSelector.Type() != tt.want.Type() {
				t.Fatalf(
					"externalIPs() selected the pool by %q, want %q",
					ephemeral.PoolSelector.Type(),
					tt.want.Type(),
				)
			}
			switch want := tt.want.Value.(type) {
			case *oxide.PoolSelectorExplicit:
				got := ephemeral.PoolSelector.Value.(*oxide.PoolSelectorExplicit)
				if got.Pool != want.Pool {
					t.Errorf("externalIPs() named pool %q, want %q", got.Pool, want.Pool)
				}
			case *oxide.PoolSelectorAuto:
				got := ephemeral.PoolSelector.Value.(*oxide.PoolSelectorAuto)
				if got.IpVersion != want.IpVersion {
					t.Errorf(
						"externalIPs() asked the default pool for %q, want %q",
						got.IpVersion,
						want.IpVersion,
					)
				}
			}
		})
	}
}
