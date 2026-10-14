package instancetype

import (
	"slices"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
)

func TestPriceIsMonotonicInBothDimensions(t *testing.T) {
	p := NewProvider(nil, availability.New(nil))
	prices := map[Specs]float64{}
	for _, it := range p.InstanceTypes(Options{}) {
		specs, err := InstanceTypeToSpecs(it.Name)
		if err != nil {
			t.Fatalf("instance type %s: %v", it.Name, err)
		}
		if len(it.Offerings) != 1 {
			t.Fatalf("instance type %s has %d offerings, want 1", it.Name, len(it.Offerings))
		}
		price := it.Offerings[0].Price
		if price <= 0 {
			t.Errorf("instance type %s price = %v, want > 0", it.Name, price)
		}
		prices[specs] = price
	}
	if len(prices) == 0 {
		t.Fatal("no instance types were built")
	}

	for specs, price := range prices {

		for other, otherPrice := range prices {
			if other.MemInGiB != specs.MemInGiB || other.Ncpus <= specs.Ncpus {
				continue
			}
			if otherPrice <= price {
				t.Errorf(
					"%d-%d price %v is not greater than %d-%d price %v (more CPU, same memory)",
					other.Ncpus,
					other.MemInGiB,
					otherPrice,
					specs.Ncpus,
					specs.MemInGiB,
					price,
				)
			}
		}

		for other, otherPrice := range prices {
			if other.Ncpus != specs.Ncpus || other.MemInGiB <= specs.MemInGiB {
				continue
			}
			if otherPrice <= price {
				t.Errorf(
					"%d-%d price %v is not greater than %d-%d price %v (same CPU, more memory)",
					other.Ncpus,
					other.MemInGiB,
					otherPrice,
					specs.Ncpus,
					specs.MemInGiB,
					price,
				)
			}
		}
	}
}

func TestPriceOrdersSmallerShapesCheaper(t *testing.T) {
	p := NewProvider(nil, availability.New(nil))
	for _, tc := range []struct{ cheaper, dearer string }{
		{"2-4", "4-8"},
		{"4-8", "4-16"},
		{"1-4", "384-1536"},
		{"16-31", "16-67"},
	} {
		a, ok := p.Get(tc.cheaper, Options{})
		if !ok {
			t.Fatalf("instance type %s does not exist", tc.cheaper)
		}
		b, ok := p.Get(tc.dearer, Options{})
		if !ok {
			t.Fatalf("instance type %s does not exist", tc.dearer)
		}
		if a.Offerings[0].Price >= b.Offerings[0].Price {
			t.Errorf("%s price %v is not cheaper than %s price %v",
				tc.cheaper, a.Offerings[0].Price, tc.dearer, b.Offerings[0].Price)
		}
	}
}

func TestComparePriceMatchesSchedulerOrdering(t *testing.T) {
	p := NewProvider(nil, availability.New(nil))
	for _, tc := range []struct{ first, second string }{
		{"2-4", "4-8"},
		{"1-12", "2-10"},
		{"1-12", "2-4"},
		{"1-4", "1-8"},
	} {
		a, _ := InstanceTypeToSpecs(tc.first)
		b, _ := InstanceTypeToSpecs(tc.second)
		if ComparePrice(a, b) >= 0 {
			t.Errorf("ComparePrice(%s, %s) >= 0, want %s first", tc.first, tc.second, tc.first)
		}
	}
	shapes := make([]Specs, 0)
	for _, it := range p.InstanceTypes(Options{}) {
		specs, _ := InstanceTypeToSpecs(it.Name)
		shapes = append(shapes, specs)
	}
	slices.SortFunc(shapes, ComparePrice)
	for i := 1; i < len(shapes); i++ {
		if price(shapes[i-1]) > price(shapes[i]) {
			t.Fatalf("%v sorted before dearer %v", shapes[i-1], shapes[i])
		}
	}
}
