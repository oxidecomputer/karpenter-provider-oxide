package capacity

import (
	"context"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/storagerefusal"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconcileReplacesThePreviousStatusSchema(t *testing.T) {
	c, kubeClient, _ := newController(t, nil)
	previous := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "OxideCapacity",
		"metadata": map[string]any{"name": v1alpha1.OxideCapacityName},
		"status": map[string]any{
			"observedAt": time.Now().UTC().Format(time.RFC3339),
			"instanceTypes": map[string]any{
				"count":                 int64(1),
				"unavailableRangeCount": int64(1),
			},
			"storage":           map[string]any{"reason": "old failure"},
			"placementRefusals": []any{},
		},
	}}
	if err := kubeClient.Create(t.Context(), previous); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	current := published(t, kubeClient)
	if current.Status.GlobalInstanceTypes.Count == 0 ||
		current.Status.Quota.State != v1alpha1.QuotaUnknown {
		t.Fatalf("previous status was not replaced: %+v", current.Status)
	}
	if current.Status.History != nil {
		t.Fatal("stale persisted failures were treated as current observations")
	}
}

func TestRenderIncludesPlacementHistoryAndQuotaFreshness(t *testing.T) {
	c, _, manager := newController(t, nil)
	manager.RecordPlacementRefusal(
		t.Context(),
		availability.NewPlacement(
			availability.Constraint{Kind: v1alpha1.AffinityGroupTypeAntiAffinity, ID: "group"},
		),
		availability.Shape{Ncpus: 8, MemInGiB: 16},
		"placement denied",
	)
	view := c.observe()
	observed := time.Now().Add(-time.Minute)
	view.Availability = view.Availability.WithQuota(
		utilization.Snapshot{Known: true, TakenAt: observed, Stale: true},
	)
	status := c.render(view)
	if status.History == nil || len(status.History.Placement) != 1 ||
		status.History.Placement[0].Message != "placement denied" {
		t.Fatalf("placement history was not rendered: %+v", status.History)
	}
	if status.Quota.State != v1alpha1.QuotaStale || status.Quota.Remaining == nil ||
		!status.Quota.ObservedAt.Equal(timestamp(observed)) {
		t.Fatalf("quota freshness was not rendered: %+v", status.Quota)
	}
}

type fakeInventory struct {
	snapshot capacity.Snapshot
}

func (f *fakeInventory) Capacity() capacity.Snapshot {
	return f.snapshot
}

func newController(
	t *testing.T,
	inventory availability.InventoryProvider,
) (*Controller, client.WithWatch, *availability.Manager) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("unable to build a scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("unable to add the provider types to the scheme: %v", err)
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.OxideCapacity{}).
		Build()

	manager := availability.New(inventory)
	instanceTypes, err := instancetype.NewProviderWithShapeConfig(
		nil,
		manager,
		instancetype.DefaultShapeConfig(),
	)
	if err != nil {
		t.Fatalf("unable to build instance types: %v", err)
	}
	return NewController(
		kubeClient,
		instanceTypes,
		storagerefusal.NewTracker(),
		time.Second,
	), kubeClient, manager
}

func published(t *testing.T, kubeClient client.Reader) *v1alpha1.OxideCapacity {
	t.Helper()

	object := &v1alpha1.OxideCapacity{}
	key := client.ObjectKey{Name: v1alpha1.OxideCapacityName}
	if err := kubeClient.Get(t.Context(), key, object); err != nil {
		t.Fatalf("unable to read the published view: %v", err)
	}
	assertValidStatus(t, object.Status)
	return object
}

func sled(id string, freeCPUs, freeMemGiB int, provisionable bool) capacity.Sled {
	return capacity.Sled{
		ID:            id,
		Capacity:      capacity.Resources{CPUs: freeCPUs, Memory: uint64(freeMemGiB) * GiB},
		Provisionable: provisionable,
	}
}

func TestReconcileCreatesTheViewIfItIsMissing(t *testing.T) {
	c, kubeClient, _ := newController(t, nil)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if view.Status.ObservedAt == nil {
		t.Error("the view has no observation time, so nothing can tell whether it is live")
	}
	if view.Status.Inventory.State != v1alpha1.InventoryDisabled {
		t.Errorf("inventory = %q, want %q with no inventory provider",
			view.Status.Inventory.State, v1alpha1.InventoryDisabled)
	}
	if len(view.Status.LaunchBackoffs) != 0 {
		t.Errorf("unavailable shapes = %d, want none before anything is refused",
			len(view.Status.LaunchBackoffs))
	}
}

func TestReconcileCountsInstanceTypesAsWellAsBoundaries(t *testing.T) {
	c, kubeClient, manager := newController(t, nil)
	manager.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 2, MemInGiB: 4}, availability.Unpinned,

		availability.ReasonCapacity)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if got := len(view.Status.LaunchBackoffs); got != 1 {
		t.Errorf("unavailable shapes = %d, want the one boundary", got)
	}
	if view.Status.GlobalInstanceTypes.Count == 0 {
		t.Fatal("the total instance type count is zero, so the view has no denominator")
	}

	if got := view.Status.GlobalInstanceTypes.UnavailableCount; got <= 1 {
		t.Errorf("unavailable instance types = %d, want every shape the boundary covers", got)
	}
	if view.Status.GlobalInstanceTypes.UnavailableCount > view.Status.GlobalInstanceTypes.Count {
		t.Errorf("unavailable instance types = %d exceeds the total of %d",
			view.Status.GlobalInstanceTypes.UnavailableCount, view.Status.GlobalInstanceTypes.Count)
	}
}

func TestReconcilePublishesRefusedShapes(t *testing.T) {
	c, kubeClient, manager := newController(t, nil)
	manager.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 8, MemInGiB: 16}, availability.Unpinned,

		availability.ReasonCapacity)

	manager.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 32, MemInGiB: 64}, availability.Unpinned,

		availability.ReasonQuota)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if got := len(view.Status.LaunchBackoffs); got != 2 {
		t.Fatalf("unavailable shapes = %d, want 2", got)
	}
	smallest := view.Status.LaunchBackoffs[0]
	if smallest.LastRefusedShape != (v1alpha1.CapacityShape{CPUs: 8, MemoryGiB: 16}) {
		t.Errorf("first entry refused %+v, want the smallest refused shape 8-16",
			smallest.LastRefusedShape)
	}

	if smallest.MinimumShape.CPUs != 8 || smallest.MinimumShape.MemoryGiB != 16 {
		t.Errorf("range starts at %d cpus and %d GiB, want 8 and 16",
			smallest.MinimumShape.CPUs, smallest.MinimumShape.MemoryGiB)
	}

	if smallest.Reason != string(availability.ReasonCapacity) {
		t.Errorf("reason = %q, want %q", smallest.Reason, availability.ReasonCapacity)
	}
	if smallest.State != v1alpha1.BackoffWaiting {
		t.Errorf("state = %q, want %q", smallest.State, v1alpha1.BackoffWaiting)
	}
	if smallest.ConsecutiveRefusals != 1 {
		t.Errorf("consecutive refusals = %d, want 1", smallest.ConsecutiveRefusals)
	}
	if smallest.RetryAfter == nil {
		t.Error("no retry time was published, which is the question the object exists to answer")
	}
	if smallest.LastRefusal == nil {
		t.Error("no refusal time was published")
	}
	if quota := view.Status.LaunchBackoffs[1]; quota.Reason != string(
		availability.ReasonQuota,
	) {
		t.Errorf("the quota-refused shape reports reason %q, want %q",
			quota.Reason, availability.ReasonQuota)
	}
}

func TestReconcilePublishesAStorageRefusal(t *testing.T) {
	c, kubeClient, _ := newController(t, nil)
	c.storage.Record(
		t.Context(),
		availability.ReasonCapacity,
		"the rack has nowhere to put the boot disk",
	)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if view.Status.History == nil || view.Status.History.Storage == nil {
		t.Fatal("no storage refusal was published")
	}
	if got := view.Status.History.Storage.Reason; got != string(availability.ReasonCapacity) {
		t.Errorf("reason = %q, want %q", got, availability.ReasonCapacity)
	}
	if got := view.Status.History.Storage.ConsecutiveRefusals; got != 1 {
		t.Errorf("consecutive refusals = %d, want 1", got)
	}
	if view.Status.History.Storage.Message == "" {
		t.Error("no diagnosis was published, so the view names the limit without explaining it")
	}
	if view.Status.History.Storage.LastRefusal == nil {
		t.Error("no refusal time was published")
	}

	if got := len(view.Status.LaunchBackoffs); got != 0 {
		t.Errorf("%d shapes were taken out of consideration by a storage refusal, want 0", got)
	}
}

func TestAClearedStorageRefusalIsUnpublished(t *testing.T) {
	c, kubeClient, _ := newController(t, nil)
	c.storage.Record(t.Context(), availability.ReasonQuota, "no storage quota")

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if published(t, kubeClient).Status.History == nil {
		t.Fatal("the refusal was never published, so the test proves nothing")
	}

	c.storage.Clear(t.Context())
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if got := published(t, kubeClient).Status.History; got != nil {
		t.Errorf(
			"storage refusal %+v is still published after it was cleared",
			got,
		)
	}
}

func TestReconcilePublishesRackCapacity(t *testing.T) {
	inventory := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{
			sled("sled-b", 8, 16, true),
			sled("sled-a", 4, 8, true),
			sled("sled-c", 64, 128, false),
		},
		time.Now(),
		time.Hour,
		1,
	)}
	c, kubeClient, _ := newController(t, inventory)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if view.Status.Inventory.State != v1alpha1.InventoryFresh {
		t.Fatalf("inventory = %q, want %q", view.Status.Inventory.State, v1alpha1.InventoryFresh)
	}
	if len(view.Status.Inventory.Sleds) == 0 {
		t.Fatal("no rack capacity was published")
	}
	if got := len(view.Status.Inventory.Sleds); got != 3 {
		t.Fatalf("%d sleds published, want 3", got)
	}

	if got := view.Status.Inventory.Sleds[0].ID; got != "sled-a" {
		t.Errorf("first sled is %q, want sled-a", got)
	}

	if got := view.Status.Inventory.LargestFeasibleInstanceType; got != "8-16" {
		t.Errorf("largest feasible = %q, want 8-16 from the emptiest provisionable sled", got)
	}
	drained := view.Status.Inventory.Sleds[2]
	if drained.Provisionable {
		t.Fatalf("sled %s was published as provisionable", drained.ID)
	}
	if drained.FreeCPUs != 0 || drained.FreeMemoryGiB != 0 {
		t.Errorf("a sled accepting no placements published %d cpus and %d GiB of headroom",
			drained.FreeCPUs, drained.FreeMemoryGiB)
	}
}

func TestInventoryLimitsTheGlobalCatalogWithoutCreatingBackoffs(t *testing.T) {
	inventory := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 8, 16, true)}, time.Now(), time.Hour, 1,
	)}
	c, kubeClient, _ := newController(t, inventory)
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := published(t, kubeClient).Status
	if len(status.LaunchBackoffs) != 0 {
		t.Fatalf("inventory created launch backoffs: %+v", status.LaunchBackoffs)
	}
	if status.GlobalInstanceTypes.UnavailableCount == 0 {
		t.Fatal("small inventory did not limit the catalog")
	}
	if status.History != nil {
		t.Fatal("inventory created failure history")
	}
}

func TestStaleInventoryDoesNotBlockTheGlobalCatalog(t *testing.T) {
	inventory := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 8, 16, true)},
		time.Now().Add(-time.Hour),
		time.Minute,
		1,
	)}
	c, kubeClient, _ := newController(t, inventory)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if len(view.Status.LaunchBackoffs) != 0 ||
		view.Status.GlobalInstanceTypes.UnavailableCount != 0 {
		t.Fatalf("stale inventory blocked launches: %+v", view.Status)
	}
}

func TestStaleInventoryIsReportedWithoutAFrontier(t *testing.T) {
	inventory := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 8, 16, true)},
		time.Now().Add(-time.Hour),
		time.Minute,
		1,
	)}
	c, kubeClient, _ := newController(t, inventory)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if view.Status.Inventory.State != v1alpha1.InventoryStale {
		t.Errorf("inventory = %q, want %q", view.Status.Inventory.State, v1alpha1.InventoryStale)
	}

	if got := view.Status.Inventory.LargestFeasibleInstanceType; got != "8-16" {
		t.Errorf("largest feasible = %q, want the stale reading reported beside its state", got)
	}
	if len(view.Status.Inventory.Sleds) == 0 {
		t.Error("the stale reading was dropped entirely, leaving nothing to explain the state")
	}
}

func TestUnreadInventoryIsDistinctFromDisabled(t *testing.T) {
	c, kubeClient, _ := newController(t, &fakeInventory{})

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if view.Status.Inventory.State != v1alpha1.InventoryUnknown {
		t.Errorf("inventory = %q, want %q", view.Status.Inventory.State, v1alpha1.InventoryUnknown)
	}
	if len(view.Status.Inventory.Sleds) != 0 || view.Status.Inventory.ObservedAt != nil {
		t.Error("rack capacity was published from a reading that never happened")
	}
}

func TestUnchangedViewIsNotRewritten(t *testing.T) {
	c, kubeClient, manager := newController(t, nil)
	manager.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 8, MemInGiB: 16}, availability.Unpinned,

		availability.ReasonCapacity)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	first := published(t, kubeClient).ResourceVersion

	for range 5 {
		if _, err := c.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile returned %v", err)
		}
	}
	if got := published(t, kubeClient).ResourceVersion; got != first {
		t.Errorf("the view was rewritten (%s to %s) with nothing changed", first, got)
	}

	manager.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 4, MemInGiB: 8}, availability.Unpinned,

		availability.ReasonCapacity)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if got := published(t, kubeClient).ResourceVersion; got == first {
		t.Error("a newly refused shape did not reach the view")
	}
}

func TestObservedAtIsRefreshedOnAHeartbeat(t *testing.T) {
	c, kubeClient, _ := newController(t, nil)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	stale := published(t, kubeClient)
	backdated := metav1.NewTime(time.Now().Add(-2 * heartbeat))
	stale.Status.ObservedAt = &backdated
	if err := kubeClient.Status().Update(t.Context(), stale); err != nil {
		t.Fatalf("unable to backdate the published view: %v", err)
	}

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	if got := published(t, kubeClient).Status.ObservedAt; !got.After(backdated.Time) {
		t.Errorf("observedAt = %v, want it refreshed past the heartbeat", got)
	}
}

func TestADeletedViewIsRepublished(t *testing.T) {
	c, kubeClient, _ := newController(t, nil)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if err := kubeClient.Delete(t.Context(), published(t, kubeClient)); err != nil {
		t.Fatalf("unable to delete the published view: %v", err)
	}
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	published(t, kubeClient)
}

func TestAMissingCRDDoesNotFailTheReconcile(t *testing.T) {

	installed := false
	uninstalled := &meta.NoKindMatchError{
		GroupKind: v1alpha1.GroupVersion.WithKind("OxideCapacity").GroupKind(),
	}
	c, kubeClient, _ := newController(t, nil)
	c.kubeClient = interceptor.NewClient(kubeClient, interceptor.Funcs{
		Get: func(
			ctx context.Context,
			inner client.WithWatch,
			key client.ObjectKey,
			obj client.Object,
			opts ...client.GetOption,
		) error {
			if !installed {
				return uninstalled
			}
			return inner.Get(ctx, key, obj, opts...)
		},
	})

	result, err := c.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile returned %v, want a missing CRD to be tolerated", err)
	}
	if result.RequeueAfter != time.Second {
		t.Errorf("RequeueAfter = %v, want the controller to keep trying", result.RequeueAfter)
	}
	if !c.crdMissingLogged {
		t.Error("the missing CRD was not reported, so nobody is told to upgrade the CRD chart")
	}

	installed = true
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v after the CRD was installed", err)
	}
	if c.crdMissingLogged {
		t.Error("the controller still believes the CRD is missing after publishing to it")
	}
	published(t, kubeClient)
}

func TestQuotaIsExplicitlyUnknownBeforeAnObservation(t *testing.T) {
	c, kubeClient, _ := newController(t, nil)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	if quota := published(
		t,
		kubeClient,
	).Status.Quota; quota.State != v1alpha1.QuotaUnknown || quota.Remaining != nil ||
		quota.ObservedAt != nil {
		t.Errorf("quota = %+v, want Unknown with no observation", quota)
	}
}

func TestViewIsAddressableByName(t *testing.T) {
	c, kubeClient, _ := newController(t, nil)
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	err := kubeClient.Get(
		t.Context(),
		client.ObjectKey{Name: "not-the-singleton"},
		&v1alpha1.OxideCapacity{},
	)
	if !kerrors.IsNotFound(err) {
		t.Errorf("Get of another name returned %v, want NotFound", err)
	}
}

func TestReconcilePublishesTheSkippedRangeNotJustTheRefusal(t *testing.T) {
	c, kubeClient, manager := newController(t, nil)

	manager.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 32, MemInGiB: 64}, availability.Unpinned,

		availability.ReasonCapacity)

	manager.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 31, MemInGiB: 62}, availability.Unpinned,

		availability.ReasonCapacity)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if got := len(view.Status.LaunchBackoffs); got != 1 {
		t.Fatalf("published %d ranges, want the walk collapsed into 1", got)
	}
	unavailable := view.Status.LaunchBackoffs[0]

	if unavailable.LastRefusedShape != (v1alpha1.CapacityShape{CPUs: 31, MemoryGiB: 62}) {
		t.Errorf("refused shape is %+v, want 31-62", unavailable.LastRefusedShape)
	}

	if unavailable.MinimumShape.CPUs >= 31 {
		t.Errorf("range starts at %d cpus, want it below the refused 31 to reflect the skip",
			unavailable.MinimumShape.CPUs)
	}
	if unavailable.KnownLaunchableShape != nil {
		t.Errorf("known launchable shape is %+v, want it absent until something launches",
			unavailable.KnownLaunchableShape)
	}
}
