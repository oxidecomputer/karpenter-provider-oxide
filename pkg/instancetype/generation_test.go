package instancetype

import (
	"math"
	"slices"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/api/validate/content"
)

func TestGeometricValues(t *testing.T) {
	wantCPUs := []int{1, 2, 4, 8, 16, 32, 64, 128, 256, 384}
	if got := geometricValues(1, 384, 100); !slices.Equal(got, wantCPUs) {
		t.Fatalf("CPU values = %v, want %v", got, wantCPUs)
	}

	wantMemory := []int{4, 8, 16, 32, 64, 128, 256, 512, 1024, 1536}
	if got := geometricValues(4, 1536, 100); !slices.Equal(got, wantMemory) {
		t.Fatalf("memory values = %v, want %v", got, wantMemory)
	}
}

func TestGeneratedShapesHonorMemorySanityBound(t *testing.T) {
	generated, err := generateShapes(DefaultShapeIncrementPercent)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) == 0 {
		t.Fatal("no shapes generated")
	}

	for _, shape := range generated {
		if shape.MemInGiB < minInstanceMemoryGiB || shape.MemInGiB*2 < shape.Ncpus {
			t.Errorf(
				"generated implausible shape %s",
				SpecsToInstanceTypeName(shape.Ncpus, shape.MemInGiB),
			)
		}
	}
	if got := generated[len(generated)-1]; got != (Specs{Ncpus: maxSledCPUs, MemInGiB: maxSledMemGiB}) {
		t.Errorf("last shape = %v, want both catalog maxima", got)
	}
}

func TestShapeIncrementIsConfigurable(t *testing.T) {
	provider, err := NewProviderWithShapeConfig(nil, nil, ShapeConfig{IncrementPercent: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.Get("4-8"); !ok {
		t.Error("a shape on both 100% progressions is missing")
	}
	if _, ok := provider.Get("3-8"); ok {
		t.Error("a CPU value outside the configured progression was generated")
	}
	if _, ok := provider.Get("4-12"); ok {
		t.Error("a memory value outside the configured progression was generated")
	}
}

func TestShapeIncrementValidation(t *testing.T) {
	for _, increment := range []float64{0, -1, 101, math.NaN(), math.Inf(1)} {
		if _, err := generateShapes(increment); err == nil {
			t.Errorf("generateShapes(%v) succeeded", increment)
		}
	}
}

func TestInstanceTypesHaveSchedulingLabels(t *testing.T) {
	provider, err := NewProviderWithShapeConfig(nil, nil, ShapeConfig{IncrementPercent: 100})
	if err != nil {
		t.Fatal(err)
	}
	instanceType, ok := provider.Get("4-16")
	if !ok {
		t.Fatal("instance type 4-16 does not exist")
	}

	tests := map[string]string{
		v1alpha1.LabelInstanceCPU:            "4",
		v1alpha1.LabelInstanceMemory:         "16384",
		v1alpha1.LabelInstanceCPUMemoryRatio: "1-4",
	}
	for label, want := range tests {
		requirement, ok := instanceType.Requirements[label]
		if !ok {
			t.Errorf("instance type has no %s requirement", label)
			continue
		}
		values := requirement.Values()
		if len(values) != 1 || values[0] != want {
			t.Errorf("%s values = %v, want [%s]", label, values, want)
		}
	}
}

func TestReducedCPUMemoryRatio(t *testing.T) {
	tests := []struct {
		cpus, memGiB int
		want         string
	}{
		{cpus: 8, memGiB: 16, want: "1-2"},
		{cpus: 4, memGiB: 16, want: "1-4"},
		{cpus: 2, memGiB: 16, want: "1-8"},
		{cpus: 1, memGiB: 16, want: "1-16"},
		{cpus: 1, memGiB: 32, want: "1-32"},
		{cpus: 6, memGiB: 15, want: "2-5"},
	}
	for _, tt := range tests {
		got := reducedCPUMemoryRatio(tt.cpus, tt.memGiB)
		if got != tt.want {
			t.Errorf("reducedCPUMemoryRatio(%d, %d) = %q, want %q", tt.cpus, tt.memGiB, got, tt.want)
		}
		if errs := content.IsLabelValue(got); len(errs) != 0 {
			t.Errorf("reducedCPUMemoryRatio(%d, %d) is not a valid label: %v", tt.cpus, tt.memGiB, errs)
		}
	}
}
