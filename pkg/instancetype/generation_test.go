package instancetype

import (
	"math"
	"slices"
	"testing"
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
