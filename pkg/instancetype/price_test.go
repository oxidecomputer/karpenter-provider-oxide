package instancetype

import (
	"context"
	"testing"
)

func TestPriceIsMonotonicInBothDimensions(t *testing.T) {
	p := NewProvider(nil, nil)
	prices := map[Specs]float64{}
	for _, it := range p.InstanceTypes() {
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

func TestPriceIsIndependentOfAvailability(t *testing.T) {
	p := NewProvider(nil, nil)

	before, ok := p.Get("8-16")
	if !ok {
		t.Fatal("instance type 8-16 does not exist")
	}
	want := before.Offerings[0].Price

	p.RecordICE(context.Background(), Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	after, ok := p.Get("8-16")
	if !ok {
		t.Fatal("instance type 8-16 does not exist")
	}
	if after.Offerings[0].Available {
		t.Fatal("instance type 8-16 is still available after being marked")
	}
	if got := after.Offerings[0].Price; got != want {
		t.Errorf("price changed with availability: got %v, want %v", got, want)
	}
}

func TestPriceOrdersSmallerShapesCheaper(t *testing.T) {
	p := NewProvider(nil, nil)
	for _, tc := range []struct{ cheaper, dearer string }{
		{"2-4", "4-8"},
		{"4-8", "4-16"},
		{"1-4", "254-757"},
		{"16-30", "16-64"},
	} {
		a, ok := p.Get(tc.cheaper)
		if !ok {
			t.Fatalf("instance type %s does not exist", tc.cheaper)
		}
		b, ok := p.Get(tc.dearer)
		if !ok {
			t.Fatalf("instance type %s does not exist", tc.dearer)
		}
		if a.Offerings[0].Price >= b.Offerings[0].Price {
			t.Errorf("%s price %v is not cheaper than %s price %v",
				tc.cheaper, a.Offerings[0].Price, tc.dearer, b.Offerings[0].Price)
		}
	}
}
