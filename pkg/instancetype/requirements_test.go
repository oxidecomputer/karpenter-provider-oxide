package instancetype

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
)

// Upstream builds the topology-spread domain universe from instance type requirements,
// so zone and capacity type have to be there and agree with the offering.
func TestRequirementsCoverTopologyLabels(t *testing.T) {
	provider := providerUsing(t, nil, availability.New(nil))

	it, ok := provider.Get("8-16", Options{})
	if !ok {
		t.Fatal("instance type 8-16 is missing from the catalog")
	}

	for label, want := range map[string]string{
		corev1.LabelTopologyZone:    ZoneName,
		karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeOnDemand,
	} {
		got := it.Requirements.Get(label)
		if !got.Has(want) || got.Len() != 1 {
			t.Errorf("%s requirement is %v, want only %q", label, got.Values(), want)
		}
		if offered := it.Offerings[0].Requirements.Get(label); !offered.Has(want) {
			t.Errorf("offering %s is %v, want %q", label, offered.Values(), want)
		}
	}
}
