package availability

import (
	"context"
	"testing"
	"time"
)

const (
	milan   Platform = "amd_milan"
	turin   Platform = "amd_turin"
	turinV2 Platform = "amd_turin_v2"
)

func TestPlatformCovers(t *testing.T) {
	for _, tc := range []struct {
		said, asked Platform
		want        bool
	}{
		{Unpinned, Unpinned, true},
		{Unpinned, milan, true},
		{Unpinned, turinV2, true},
		{milan, Unpinned, false},
		{milan, milan, true},
		{milan, turin, true},
		{milan, turinV2, true},
		{turin, milan, false},
		{turin, turinV2, true},
		{turinV2, turin, true},
		{turinV2, Unpinned, false},
		{Unpinned, "amd_venice", true},
		{milan, "amd_venice", false},
		{"amd_venice", "amd_venice", true},
		{"amd_venice", turin, false},
	} {
		if got := tc.said.Covers(tc.asked); got != tc.want {
			t.Errorf("%q.Covers(%q) = %v, want %v", tc.said, tc.asked, got, tc.want)
		}
	}
}

func pinnedUnavailable(c *Manager, shape Shape, platform Platform) bool {
	return !c.Check(Request{Shape: shape, Platform: platform}).Available
}

func TestARefusalUnderAPinLeavesBroaderPinsOnOffer(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	c.RecordICE(context.Background(), Shape{Ncpus: 8, MemInGiB: 16}, turinV2, ReasonCapacity)

	for _, tc := range []struct {
		shape    Shape
		platform Platform
		want     bool
	}{
		{Shape{Ncpus: 8, MemInGiB: 16}, turinV2, true},
		{Shape{Ncpus: 8, MemInGiB: 16}, turin, true},
		{Shape{Ncpus: 16, MemInGiB: 32}, turin, true},
		{Shape{Ncpus: 4, MemInGiB: 8}, turinV2, false},
		{Shape{Ncpus: 8, MemInGiB: 16}, milan, false},
		{Shape{Ncpus: 8, MemInGiB: 16}, Unpinned, false},
	} {
		if got := pinnedUnavailable(c, tc.shape, tc.platform); got != tc.want {
			t.Errorf("%s under %q unavailable = %v, want %v",
				shapeKey(tc.shape), tc.platform, got, tc.want)
		}
	}
}

func TestAnUnpinnedRefusalCoversEveryPin(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	c.RecordICE(context.Background(), Shape{Ncpus: 8, MemInGiB: 16}, Unpinned, ReasonCapacity)

	for _, platform := range []Platform{Unpinned, milan, turin, turinV2} {
		if !pinnedUnavailable(c, Shape{Ncpus: 8, MemInGiB: 16}, platform) {
			t.Errorf("8-16 under %q is still on offer", platform)
		}
	}
}

func TestASuccessUnderANarrowPinClearsBroaderRefusals(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()
	shape := Shape{Ncpus: 8, MemInGiB: 16}
	c.RecordICE(ctx, shape, Unpinned, ReasonCapacity)
	c.RecordICE(ctx, shape, milan, ReasonCapacity)
	c.RecordICE(ctx, shape, turinV2, ReasonCapacity)

	c.RecordSuccess(ctx, shape, turin)

	for _, platform := range []Platform{Unpinned, milan, turin, turinV2} {
		if pinnedUnavailable(c, shape, platform) {
			t.Errorf("8-16 under %q is still refused after a Turin launch", platform)
		}
	}
	if got := len(c.Snapshot().Entries()); got != 0 {
		t.Errorf("%d entries remain, want none", got)
	}
}

func TestAnUnpinnedSuccessSaysNothingAboutPins(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()
	shape := Shape{Ncpus: 8, MemInGiB: 16}
	c.RecordICE(ctx, shape, Unpinned, ReasonCapacity)
	c.RecordICE(ctx, shape, turinV2, ReasonCapacity)

	c.RecordSuccess(ctx, shape, Unpinned)

	if pinnedUnavailable(c, shape, Unpinned) {
		t.Error("the unpinned refusal was not cleared")
	}
	if !pinnedUnavailable(c, shape, turinV2) {
		t.Error("the Turin v2 refusal was cleared by an unpinned launch")
	}
}

func TestReleasedCapacityReopensThePinAndBroaderOnes(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()
	shape := Shape{Ncpus: 8, MemInGiB: 16}
	c.RecordICE(ctx, shape, Unpinned, ReasonCapacity)
	c.RecordICE(ctx, shape, milan, ReasonCapacity)
	c.RecordICE(ctx, shape, turinV2, ReasonCapacity)

	c.RecordCapacityReleased(ctx, shape, milan)

	for _, tc := range []struct {
		platform Platform
		want     bool
	}{
		{Unpinned, false},
		{milan, false},
		{turinV2, true},
	} {
		if got := pinnedUnavailable(c, shape, tc.platform); got != tc.want {
			t.Errorf("8-16 under %q unavailable = %v, want %v", tc.platform, got, tc.want)
		}
	}
}

// A running node proves its own pin and every broader one placeable, but says
// nothing about narrower pins.
func TestASeededFloorOnlyServesThePinsItProves(t *testing.T) {
	now := time.Now()
	c := newTestManager(&now)
	ctx := context.Background()
	c.SeedLaunchable(ctx, Shape{Ncpus: 8, MemInGiB: 16}, Unpinned)
	c.SeedLaunchable(ctx, Shape{Ncpus: 4, MemInGiB: 8}, turin)

	c.RecordICE(ctx, Shape{Ncpus: 32, MemInGiB: 64}, milan, ReasonCapacity)
	c.RecordICE(ctx, Shape{Ncpus: 40, MemInGiB: 80}, Unpinned, ReasonCapacity)

	want := map[Platform]Shape{
		milan:    {Ncpus: 4, MemInGiB: 8},
		Unpinned: {Ncpus: 8, MemInGiB: 16},
	}
	for _, entry := range c.Snapshot().Entries() {
		if entry.Floor != want[entry.Platform] {
			t.Errorf("entry under %q has floor %s, want %s",
				entry.Platform, floorName(entry.Floor), floorName(want[entry.Platform]))
		}
	}
}
