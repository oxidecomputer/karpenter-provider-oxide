package cloudprovider

import (
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestInstanceToNodeClaimAddsInstanceSchedulingLabels(t *testing.T) {
	provider := &OxideCloudProvider{
		instanceTypeProvider: instancetype.NewProvider(nil, availability.New(nil)),
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

func TestInstanceToNodeClaimUsesEndpointTopology(t *testing.T) {
	for _, hostname := range []string{
		"prod.sys.rack-a.example.com",
		"prod.sys.rack-b.example.com",
		strings.Repeat("a", 63) + ".example.com",
	} {
		t.Run(hostname, func(t *testing.T) {
			catalog := instancetype.NewProvider(nil, availability.New(nil))
			topology, err := instancetype.SiloTopology("https://"+hostname, "prod", "silo-id")
			if err != nil {
				t.Fatal(err)
			}
			catalog.SetTopology(topology)
			provider := &OxideCloudProvider{instanceTypeProvider: catalog}
			claim := &karpv1.NodeClaim{}
			claim.Annotations = map[string]string{
				"example.com/custom": "retained", v1alpha1.AnnotationSiloHostname: "incorrect",
			}
			got, err := provider.InstanceToNodeClaim(&oxide.Instance{
				Hostname: "karpenter-node",
				Ncpus:    4,
				Memory:   oxide.ByteCount(16 * instancetype.GiB),
			}, nil, claim)
			if err != nil {
				t.Fatal(err)
			}
			it, ok := catalog.Get("4-16", instancetype.Options{})
			if !ok {
				t.Fatal("instance type missing from catalog")
			}
			for _, key := range []string{corev1.LabelTopologyZone, corev1.LabelTopologyRegion} {
				if value := got.Labels[key]; value != topology.Zone ||
					!it.Requirements.Get(key).Has(value) {
					t.Errorf("%s = %q, want %q in catalog and claim", key, value, topology.Zone)
				}
			}
			for _, offering := range it.Offerings {
				if !offering.Requirements.Get(corev1.LabelTopologyZone).
					Has(got.Labels[corev1.LabelTopologyZone]) {
					t.Error("offering zone differs from NodeClaim")
				}
			}
			for key, want := range map[string]string{
				v1alpha1.AnnotationSiloHostname: hostname,
				v1alpha1.AnnotationSiloName:     "prod", v1alpha1.AnnotationSiloID: "silo-id",
				"example.com/custom": "retained",
			} {
				if got.Annotations[key] != want {
					t.Errorf("annotation %s = %q, want %q", key, got.Annotations[key], want)
				}
			}
			// Conversion must not silently migrate an existing NodeClaim's domains.
			claim.Labels[corev1.LabelTopologyRegion] = "prod"
			claim.Labels[corev1.LabelTopologyZone] = "prod"
			got, err = provider.InstanceToNodeClaim(&oxide.Instance{
				Hostname: "karpenter-node",
				Ncpus:    4,
				Memory:   oxide.ByteCount(16 * instancetype.GiB),
			}, nil, claim)
			if err != nil {
				t.Fatal(err)
			}
			if got.Labels[corev1.LabelTopologyZone] != "prod" ||
				got.Labels[corev1.LabelTopologyRegion] != "prod" {
				t.Error("existing topology labels were overwritten")
			}
		})
	}
}
