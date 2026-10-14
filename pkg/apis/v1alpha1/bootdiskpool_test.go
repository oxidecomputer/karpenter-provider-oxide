package v1alpha1

import "testing"

func TestBootDiskPoolDefaults(t *testing.T) {
	spec := OxideBootDiskPoolSpec{}
	if spec.EffectiveMinReady() != 2 || spec.EffectivePercentage() != 10 ||
		spec.EffectiveMaxReady() != 20 {
		t.Fatalf(
			"defaults = %d/%d/%d, want 2/10/20",
			spec.EffectiveMinReady(),
			spec.EffectivePercentage(),
			spec.EffectiveMaxReady(),
		)
	}
}
