package instancetype

import (
	"context"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
)

func providerUsing(
	t *testing.T,
	quotas QuotaProvider,
	manager *availability.Manager,
) *Provider {
	t.Helper()
	provider, err := NewProviderWithShapeConfig(quotas, manager, DefaultShapeConfig())
	if err != nil {
		t.Fatalf("building instance type provider: %v", err)
	}
	return provider
}

func TestInstanceTypesReflectSharedAvailability(t *testing.T) {
	manager := availability.New(nil)
	provider := providerUsing(t, nil, manager)

	before, ok := provider.Get("8-16")
	if !ok || !before.Offerings[0].Available {
		t.Fatal("instance type 8-16 is unavailable before anything is refused")
	}

	manager.RecordICE(t.Context(), Specs{Ncpus: 8, MemInGiB: 16}, availability.ReasonCapacity)

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"8-16", false},
		{"16-30", false},
		{"4-8", true},
	} {
		instanceType, ok := provider.Get(tc.name)
		if !ok {
			t.Fatalf("instance type %s does not exist", tc.name)
		}
		if got := instanceType.Offerings[0].Available; got != tc.want {
			t.Errorf("instance type %s available = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type catalogInventory struct {
	snapshot capacity.Snapshot
}

func (f *catalogInventory) Capacity() capacity.Snapshot { return f.snapshot }

func TestInstanceTypesReflectSharedInventory(t *testing.T) {
	inventory := &catalogInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{{
			ID: "sled",
			Capacity: capacity.Resources{
				CPUs:   8,
				Memory: 16 * availability.GiB,
			},
			Provisionable: true,
		}},
		time.Now(),
		time.Hour,
		1,
	)}
	manager := availability.New(inventory)
	provider := providerUsing(t, nil, manager)

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"8-16", true},
		{"4-8", true},
		{"16-30", false},
		{"8-64", false},
	} {
		instanceType, ok := provider.Get(tc.name)
		if !ok {
			t.Fatalf("instance type %s does not exist", tc.name)
		}
		if got := instanceType.Offerings[0].Available; got != tc.want {
			t.Errorf("instance type %s available = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAvailabilityDoesNotChangePrice(t *testing.T) {
	manager := availability.New(nil)
	provider := providerUsing(t, nil, manager)
	before, ok := provider.Get("8-16")
	if !ok {
		t.Fatal("instance type 8-16 does not exist")
	}
	want := before.Offerings[0].Price

	manager.RecordICE(
		context.Background(),
		Specs{Ncpus: 8, MemInGiB: 16},
		availability.ReasonCapacity,
	)

	after, _ := provider.Get("8-16")
	if after.Offerings[0].Available {
		t.Fatal("instance type 8-16 is still available after being marked")
	}
	if got := after.Offerings[0].Price; got != want {
		t.Errorf("price changed with availability: got %v, want %v", got, want)
	}
}
