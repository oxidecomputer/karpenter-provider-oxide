package instancetype

import (
	"context"
	"testing"
)

func TestUnavailableOfferingsCoversLargerShapes(t *testing.T) {
	u := NewUnavailableOfferings()
	u.Mark(context.Background(), Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	for _, tc := range []struct {
		specs Specs
		want  bool
	}{
		{Specs{Ncpus: 8, MemInGiB: 16}, true},  // the marked shape itself
		{Specs{Ncpus: 16, MemInGiB: 32}, true}, // larger in both
		{Specs{Ncpus: 8, MemInGiB: 64}, true},  // larger in memory only
		{Specs{Ncpus: 4, MemInGiB: 16}, false}, // smaller in cpus
		{Specs{Ncpus: 16, MemInGiB: 8}, false}, // more cpus but less memory
		{Specs{Ncpus: 1, MemInGiB: 2}, false},  // smaller in both
	} {
		if got, _ := u.IsUnavailable(tc.specs); got != tc.want {
			t.Errorf("IsUnavailable(%d cpus, %d GiB) = %v, want %v",
				tc.specs.Ncpus, tc.specs.MemInGiB, got, tc.want)
		}
	}
}

func TestUnavailableOfferingsDropsSubsumedEntries(t *testing.T) {
	u := NewUnavailableOfferings()
	u.Mark(context.Background(), Specs{Ncpus: 16, MemInGiB: 32}, ReasonCapacity)
	u.Mark(context.Background(), Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	if got := len(u.Snapshot()); got != 1 {
		t.Fatalf("snapshot has %d entries, want 1", got)
	}
	if unavailable, _ := u.IsUnavailable(Specs{Ncpus: 16, MemInGiB: 32}); !unavailable {
		t.Error("the subsumed shape is no longer covered")
	}
}

func TestInstanceTypesMarkOfferingsUnavailable(t *testing.T) {
	p := NewProvider(nil)

	before, ok := p.Get("8-16")
	if !ok {
		t.Fatal("instance type 8-16 does not exist")
	}
	if !before.Offerings[0].Available {
		t.Fatal("instance type 8-16 is unavailable before anything is marked")
	}

	p.MarkUnavailable(context.Background(), Specs{Ncpus: 8, MemInGiB: 16}, ReasonCapacity)

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"8-16", false},
		{"16-32", false},
		{"4-8", true},
	} {
		it, ok := p.Get(tc.name)
		if !ok {
			t.Fatalf("instance type %s does not exist", tc.name)
		}
		if got := it.Offerings[0].Available; got != tc.want {
			t.Errorf("instance type %s available = %v, want %v", tc.name, got, tc.want)
		}
	}
}
