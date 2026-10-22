package instancetype

import (
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
)

func TestInstanceTypesOfferEveryCPUPlatform(t *testing.T) {
	manager := availability.New(nil)
	provider := providerUsing(t, nil, manager)

	it, ok := provider.Get("8-16", Options{})
	if !ok {
		t.Fatal("instance type 8-16 is missing from the catalog")
	}
	requirement := it.Requirements.Get(v1alpha1.LabelInstanceCPUPlatform)
	if requirement.Len() != len(v1alpha1.CPUPlatforms) {
		t.Errorf("platform requirement is %v, want every platform", requirement.Values())
	}
	if got := len(it.Offerings); got != len(v1alpha1.CPUPlatforms) {
		t.Fatalf("8-16 has %d offerings, want one per platform", got)
	}
	for _, offering := range it.Offerings {
		platform := offering.Requirements.Get(v1alpha1.LabelInstanceCPUPlatform)
		if platform.Len() != 1 {
			t.Errorf("offering platform is %v, want exactly one", platform.Values())
		}
		if !offering.Available {
			t.Errorf("offering %v is unavailable before anything is refused", offering.Requirements)
		}
	}

	manager.RecordICE(
		t.Context(),
		Specs{Ncpus: 8, MemInGiB: 16},
		availability.Platform(v1alpha1.CPUPlatformAmdTurinV2),
		availability.ReasonCapacity,
	)

	for _, tc := range []struct {
		name     string
		platform string
		want     bool
	}{
		{"8-16", v1alpha1.CPUPlatformAmdTurinV2, false},
		{"8-16", v1alpha1.CPUPlatformAmdTurin, false},
		{"16-31", v1alpha1.CPUPlatformAmdTurin, false},
		{"8-16", v1alpha1.CPUPlatformAmdMilan, true},
		{"4-8", v1alpha1.CPUPlatformAmdTurinV2, true},
	} {
		it, ok := provider.Get(tc.name, Options{})
		if !ok {
			t.Fatalf("instance type %s does not exist", tc.name)
		}
		for _, offering := range it.Offerings {
			if !offering.Requirements.Get(v1alpha1.LabelInstanceCPUPlatform).Has(tc.platform) {
				continue
			}
			if offering.Available != tc.want {
				t.Errorf("%s under %s available = %v, want %v",
					tc.name, tc.platform, offering.Available, tc.want)
			}
		}
	}
}
