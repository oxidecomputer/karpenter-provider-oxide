package v1alpha1

import (
	"testing"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestInstanceSchedulingLabelsAreWellKnown(t *testing.T) {
	for _, label := range []string{
		LabelInstanceCPU,
		LabelInstanceMemory,
		LabelInstanceCPUMemoryRatio,
	} {
		if !karpv1.WellKnownLabels.Has(label) {
			t.Errorf("%s is not registered as a well-known label", label)
		}
	}
}
