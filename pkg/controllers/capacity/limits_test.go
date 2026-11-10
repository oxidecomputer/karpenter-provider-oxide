package capacity

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func statusBytes(t *testing.T, status v1alpha1.OxideCapacityStatus) int {
	t.Helper()
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxStatusBytes {
		t.Fatalf("status is %d bytes, limit is %d", len(encoded), maxStatusBytes)
	}
	return len(encoded)
}

func TestPublicationBoundsInventoryWithoutChangingScheduling(t *testing.T) {
	for _, count := range []int{320, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			sleds := make([]capacity.Sled, count)
			for i := range sleds {
				sleds[i] = sled(fmt.Sprintf("66666666-0000-4000-8000-%012d", i), 4, 8, true)
			}
			last := &sleds[len(sleds)-1]
			last.Capacity = capacity.Resources{CPUs: 384, Memory: 1536 * GiB}
			inventory := &fakeInventory{
				snapshot: capacity.NewSnapshot(sleds, time.Now(), time.Hour, 1),
			}
			c, kubeClient, manager := newController(t, inventory)
			if _, err := c.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			status := published(t, kubeClient).Status
			size := statusBytes(t, status)
			t.Logf("%d sleds: %d bytes, %d retained, %d omitted", count, size,
				len(status.Inventory.Sleds), status.Inventory.OmittedSledCount)
			if len(status.Inventory.Sleds)+status.Inventory.OmittedSledCount != count {
				t.Fatal("inventory truncation lost the total sled count")
			}
			if count == 320 && status.Inventory.OmittedSledCount != 0 {
				t.Fatal("ten racks should fit without truncation")
			}
			if count == 10000 && status.Inventory.OmittedSledCount == 0 {
				t.Fatal("oversized inventory was not truncated")
			}
			if status.Inventory.LargestFeasibleInstanceType != "384-1536" ||
				status.GlobalInstanceTypes.UnavailableCount != 0 {
				t.Fatal("the summary excluded omitted sleds")
			}
			rack, _ := manager.Snapshot().Rack()
			if len(rack.Sleds()) != count || !manager.Check(availability.Request{
				Shape: availability.Shape{Ncpus: 384, MemInGiB: 1536},
			}).Available {
				t.Fatal("publication truncated the inventory used for scheduling")
			}
			if got := testutil.ToFloat64(
				metrics.SledFreeCPUs.WithLabelValues(last.ID),
			); got != 384 {
				t.Fatalf("omitted sled metric = %v, want 384", got)
			}
			if _, err := c.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !unchanged(status, published(t, kubeClient).Status) {
				t.Fatal("unchanged inventory produced a different truncated view")
			}
		})
	}
}

func TestPublicationBoundsFailuresAndEscapedMessages(t *testing.T) {
	sleds := make([]capacity.Sled, 320)
	for i := range sleds {
		sleds[i] = sled(fmt.Sprintf("66666666-0000-4000-8000-%012d", i), 384, 1536, true)
	}
	c, kubeClient, manager := newController(t, &fakeInventory{
		snapshot: capacity.NewSnapshot(sleds, time.Now(), time.Hour, 1),
	})
	const count = 1000
	message := strings.Repeat("🙂<&\n", 1000)
	var placement availability.Placement
	for i := range count {
		placement = availability.NewPlacement(availability.Constraint{
			Kind: v1alpha1.AffinityGroupTypeAntiAffinity,
			ID:   fmt.Sprintf("66666666-0000-4000-8000-%012d", i),
		})
		manager.RecordPlacementRefusal(t.Context(), placement,
			availability.Shape{Ncpus: 4, MemInGiB: 8}, message)
		manager.RecordPlacementRefusal(t.Context(), placement,
			availability.Shape{Ncpus: 8, MemInGiB: 16}, message)
	}
	c.storage.Record(t.Context(), availability.ReasonQuota, message)
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := published(t, kubeClient).Status
	statusBytes(t, status)
	if len(status.Inventory.Sleds)+status.Inventory.OmittedSledCount != len(sleds) {
		t.Fatal("inventory did not share the status budget with failures")
	}
	if status.OmittedLaunchBackoffCount == 0 ||
		len(status.LaunchBackoffs)+status.OmittedLaunchBackoffCount != count {
		t.Fatal("backoff truncation did not account for omitted entries")
	}
	if status.History == nil || status.History.OmittedPlacementCount == 0 ||
		len(status.History.Placement)+status.History.OmittedPlacementCount != count {
		t.Fatal("history truncation did not account for omitted entries")
	}
	for _, backoff := range status.LaunchBackoffs {
		if !backoff.MessageTruncated || !utf8.ValidString(backoff.Message) ||
			len(backoff.Message) > maxMessageBytes {
			t.Fatal("diagnostic message was not safely truncated")
		}
	}
	if status.History.Storage == nil || !status.History.Storage.MessageTruncated {
		t.Fatal("storage diagnosis was not retained and truncated")
	}
	reading := manager.Snapshot()
	if len(reading.Placements()) != 2*count || reading.Placements()[0].Message != message {
		t.Fatal("publication modified internal refusal history")
	}
	if reading.Evaluate(availability.Request{
		Shape: availability.Shape{Ncpus: 8, MemInGiB: 16}, Placement: placement,
	}).Available {
		t.Fatal("an omitted backoff stopped gating launches")
	}
	if storage, ok := c.storage.Last(); !ok || storage.Message != message {
		t.Fatal("publication truncated internal storage diagnostics")
	}
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !unchanged(status, published(t, kubeClient).Status) {
		t.Fatal("unchanged failures produced a different truncated view")
	}
}

func TestPublicationBoundsPlacementHistory(t *testing.T) {
	c, kubeClient, manager := newController(t, nil)
	const count = 1000
	message := strings.Repeat("🙂", 1000)
	for i := range count {
		placement := availability.NewPlacement(availability.Constraint{
			Kind: v1alpha1.AffinityGroupTypeAntiAffinity,
			ID:   fmt.Sprintf("66666666-0000-4000-8000-%012d", i),
		})
		manager.RecordPlacementRefusal(t.Context(), placement,
			availability.Shape{Ncpus: 4, MemInGiB: 8}, message)
	}
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := published(t, kubeClient).Status
	statusBytes(t, status)
	if status.History == nil || status.History.OmittedPlacementCount == 0 ||
		len(status.History.Placement) == 0 ||
		len(status.History.Placement)+status.History.OmittedPlacementCount != count {
		t.Fatal("placement history was not bounded with accurate omitted counts")
	}
	for _, failure := range status.History.Placement {
		if !failure.MessageTruncated || !utf8.ValidString(failure.Message) ||
			len(failure.Message) > maxMessageBytes {
			t.Fatal("placement history message was not safely truncated")
		}
	}
}

func TestOversizedScopesAreOmittedWithoutBroadeningThem(t *testing.T) {
	c, kubeClient, manager := newController(t, nil)
	manager.RecordICE(t.Context(), availability.Shape{Ncpus: 8, MemInGiB: 16},
		availability.Unpinned, availability.ReasonCapacity)
	status := c.render(c.observe())
	groups := make([]v1alpha1.CapacityPlacementConstraint, 10000)
	for i := range groups {
		groups[i] = v1alpha1.CapacityPlacementConstraint{
			Type: v1alpha1.AffinityGroupTypeAntiAffinity,
			ID:   fmt.Sprintf("66666666-0000-4000-8000-%012d", i),
		}
	}
	large := status.LaunchBackoffs[0]
	large.Scope = v1alpha1.CapacityScope{Type: v1alpha1.CapacityScopePlacement, Placement: groups}
	status.LaunchBackoffs = append([]v1alpha1.LaunchBackoff{large}, status.LaunchBackoffs...)
	if err := c.publish(t.Context(), status); err != nil {
		t.Fatal(err)
	}
	got := published(t, kubeClient).Status
	statusBytes(t, got)
	if got.OmittedLaunchBackoffCount != 1 || len(got.LaunchBackoffs) != 1 ||
		got.LaunchBackoffs[0].Scope.Type != v1alpha1.CapacityScopeGlobal {
		t.Fatal("oversized scope was partially published or hid a smaller entry")
	}
}

func TestBackoffOrderingPrioritizesLaunchGates(t *testing.T) {
	entries := []v1alpha1.LaunchBackoff{
		{
			State: v1alpha1.BackoffProbeEligible,
			Scope: v1alpha1.CapacityScope{Type: v1alpha1.CapacityScopeGlobal},
		},
		{
			State: v1alpha1.BackoffProbing,
			Scope: v1alpha1.CapacityScope{Type: v1alpha1.CapacityScopeGlobal},
		},
		{
			State: v1alpha1.BackoffWaiting,
			Scope: v1alpha1.CapacityScope{Type: v1alpha1.CapacityScopePlacement},
		},
	}
	slices.SortFunc(entries, compareBackoffs)
	if entries[0].State != v1alpha1.BackoffWaiting || entries[1].State != v1alpha1.BackoffProbing {
		t.Fatal("launch gates must precede probes when publication is truncated")
	}
}

func TestOversizedSummaryIsRejectedBeforeWriting(t *testing.T) {
	c, kubeClient, _ := newController(t, nil)
	status := c.render(c.observe())
	status.Inventory.State = strings.Repeat("x", maxStatusBytes)
	if err := c.publish(t.Context(), status); err == nil {
		t.Fatal("oversized summary was accepted")
	}
	if err := kubeClient.Get(t.Context(), client.ObjectKey{Name: v1alpha1.OxideCapacityName},
		&v1alpha1.OxideCapacity{}); !kerrors.IsNotFound(err) {
		t.Fatalf("oversized status reached the API: %v", err)
	}
}
