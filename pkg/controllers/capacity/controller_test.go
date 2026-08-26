package capacity

import (
	"context"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type fakeInventory struct {
	snapshot capacity.Snapshot
}

func (f *fakeInventory) Capacity() capacity.Snapshot {
	return f.snapshot
}

func newController(
	t *testing.T,
	inventory instancetype.InventoryProvider,
) (*Controller, client.WithWatch) {
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

	return NewController(
		kubeClient,
		instancetype.NewProvider(nil, inventory),
		time.Second,
	), kubeClient
}

func published(t *testing.T, kubeClient client.Reader) *v1alpha1.OxideCapacity {
	t.Helper()

	object := &v1alpha1.OxideCapacity{}
	key := client.ObjectKey{Name: v1alpha1.OxideCapacityName}
	if err := kubeClient.Get(t.Context(), key, object); err != nil {
		t.Fatalf("unable to read the published view: %v", err)
	}
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
	c, kubeClient := newController(t, nil)

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
	if view.Status.InstanceTypes.UnavailableRangeCount != 0 {
		t.Errorf("unavailable shapes = %d, want none before anything is refused",
			view.Status.InstanceTypes.UnavailableRangeCount)
	}
}

func TestReconcileCountsInstanceTypesAsWellAsBoundaries(t *testing.T) {
	c, kubeClient := newController(t, nil)
	c.instanceTypes.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 2, MemInGiB: 4},
		instancetype.ReasonCapacity,
	)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if got := view.Status.InstanceTypes.UnavailableRangeCount; got != 1 {
		t.Errorf("unavailable shapes = %d, want the one boundary", got)
	}
	if view.Status.InstanceTypes.Count == 0 {
		t.Fatal("the total instance type count is zero, so the view has no denominator")
	}

	if got := view.Status.InstanceTypes.UnavailableCount; got <= 1 {
		t.Errorf("unavailable instance types = %d, want every shape the boundary covers", got)
	}
	if view.Status.InstanceTypes.UnavailableCount > view.Status.InstanceTypes.Count {
		t.Errorf("unavailable instance types = %d exceeds the total of %d",
			view.Status.InstanceTypes.UnavailableCount, view.Status.InstanceTypes.Count)
	}
}

func TestReconcilePublishesRefusedShapes(t *testing.T) {
	c, kubeClient := newController(t, nil)
	c.instanceTypes.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 8, MemInGiB: 16},
		instancetype.ReasonCapacity,
	)
	c.instanceTypes.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 32, MemInGiB: 64},
		instancetype.ReasonQuota,
	)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if got := view.Status.InstanceTypes.UnavailableRangeCount; got != 2 {
		t.Fatalf("unavailable shapes = %d, want 2", got)
	}
	if got := len(
		view.Status.InstanceTypes.UnavailableRanges,
	); got != view.Status.InstanceTypes.UnavailableRangeCount {
		t.Fatalf(
			"the count says %d and the list holds %d",
			view.Status.InstanceTypes.UnavailableRangeCount,
			got,
		)
	}

	smallest := view.Status.InstanceTypes.UnavailableRanges[0]
	if smallest.RefusedInstanceType != "8-16" {
		t.Errorf("first entry refused %q, want the smallest refused shape 8-16",
			smallest.RefusedInstanceType)
	}

	if smallest.FromNcpus != 8 || smallest.FromMemoryGiB != 16 {
		t.Errorf("range starts at %d cpus and %d GiB, want 8 and 16",
			smallest.FromNcpus, smallest.FromMemoryGiB)
	}

	if smallest.Range != ">=8 vCPUs and >=16 GiB" {
		t.Errorf("range reads %q, want it to spell out both bounds", smallest.Range)
	}
	if smallest.Reason != string(instancetype.ReasonCapacity) {
		t.Errorf("reason = %q, want %q", smallest.Reason, instancetype.ReasonCapacity)
	}
	if smallest.State != v1alpha1.ShapeStateBackoff {
		t.Errorf("state = %q, want %q", smallest.State, v1alpha1.ShapeStateBackoff)
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
	if quota := view.Status.InstanceTypes.UnavailableRanges[1]; quota.Reason != string(
		instancetype.ReasonQuota,
	) {
		t.Errorf("the quota-refused shape reports reason %q, want %q",
			quota.Reason, instancetype.ReasonQuota)
	}
}

func TestReconcilePublishesAStorageRefusal(t *testing.T) {
	c, kubeClient := newController(t, nil)
	c.instanceTypes.RecordStorageRefusal(
		t.Context(),
		instancetype.ReasonCapacity,
		"the rack has nowhere to put the boot disk",
	)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if view.Status.Storage == nil {
		t.Fatal("no storage refusal was published")
	}
	if got := view.Status.Storage.Reason; got != string(instancetype.ReasonCapacity) {
		t.Errorf("reason = %q, want %q", got, instancetype.ReasonCapacity)
	}
	if got := view.Status.Storage.ConsecutiveRefusals; got != 1 {
		t.Errorf("consecutive refusals = %d, want 1", got)
	}
	if view.Status.Storage.Message == "" {
		t.Error("no diagnosis was published, so the view names the limit without explaining it")
	}
	if view.Status.Storage.LastRefusal == nil {
		t.Error("no refusal time was published")
	}

	if got := view.Status.InstanceTypes.UnavailableRangeCount; got != 0 {
		t.Errorf("%d shapes were taken out of consideration by a storage refusal, want 0", got)
	}
}

func TestASuccessfulLaunchClearsThePublishedStorageRefusal(t *testing.T) {
	c, kubeClient := newController(t, nil)
	c.instanceTypes.RecordStorageRefusal(t.Context(), instancetype.ReasonQuota, "no storage quota")

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if published(t, kubeClient).Status.Storage == nil {
		t.Fatal("the refusal was never published, so the test proves nothing")
	}

	c.instanceTypes.RecordSuccess(t.Context(), instancetype.Specs{Ncpus: 8, MemInGiB: 16})
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if got := published(t, kubeClient).Status.Storage; got != nil {
		t.Errorf(
			"storage refusal %+v is still published after a launch provisioned a boot disk",
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
	c, kubeClient := newController(t, inventory)

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

func TestReconcilePublishesWhatNoSledHasRoomFor(t *testing.T) {
	inventory := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 8, 16, true)},
		time.Now(),
		time.Hour,
		1,
	)}
	c, kubeClient := newController(t, inventory)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	ranges := view.Status.InstanceTypes.UnavailableRanges

	if got := len(ranges); got != 1 {
		t.Fatalf(
			"%d ranges published, want one covering both edges of an 8 CPU, 16 GiB sled: %+v",
			got,
			ranges,
		)
	}
	if got := view.Status.InstanceTypes.UnavailableRangeCount; got != len(ranges) {
		t.Errorf("the count says %d and the list holds %d", got, len(ranges))
	}

	if view.Status.InstanceTypes.UnavailableCount == 0 {
		t.Error("no instance types are unavailable on a rack with one small sled")
	}

	headroom := ranges[0]
	if headroom.MaxNcpus != 8 || headroom.MaxMemoryGiB != 16 {
		t.Errorf("headroom = %d cpus, %d GiB, want the sled's free space",
			headroom.MaxNcpus, headroom.MaxMemoryGiB)
	}

	if got := headroom.Range; got != ">8 vCPUs or >16 GiB" {
		t.Errorf("range = %q, want both dimensions in one sentence joined by or", got)
	}

	if headroom.FromNcpus != 0 || headroom.FromMemoryGiB != 0 {
		t.Errorf("an inventory range carries a refusal corner: %+v", headroom)
	}

	for _, unavailable := range ranges {
		if unavailable.Reason != v1alpha1.RangeReasonNoSledWithRoom {
			t.Errorf("reason = %q, want %q for a range no launch was refused from",
				unavailable.Reason, v1alpha1.RangeReasonNoSledWithRoom)
		}

		if unavailable.RefusedInstanceType != "" || unavailable.State != "" ||
			unavailable.ConsecutiveRefusals != 0 || unavailable.RetryAfter != nil {
			t.Errorf("a range read off inventory carries refusal evidence: %+v", unavailable)
		}

		if got := unavailable.LargestLaunchable; got != "8-16" {
			t.Errorf("largest launchable = %q, want the shape the rack does have room for", got)
		}
	}
}

func TestStaleInventoryPublishesNoRanges(t *testing.T) {
	inventory := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 8, 16, true)},
		time.Now().Add(-time.Hour),
		time.Minute,
		1,
	)}
	c, kubeClient := newController(t, inventory)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if got := view.Status.InstanceTypes.UnavailableRanges; len(got) != 0 {
		t.Errorf("a stale reading published %d ranges: %+v", len(got), got)
	}
}

func TestStaleInventoryIsReportedWithoutAFrontier(t *testing.T) {
	inventory := &fakeInventory{snapshot: capacity.NewSnapshot(
		[]capacity.Sled{sled("sled-a", 8, 16, true)},
		time.Now().Add(-time.Hour),
		time.Minute,
		1,
	)}
	c, kubeClient := newController(t, inventory)

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
	c, kubeClient := newController(t, &fakeInventory{})

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
	c, kubeClient := newController(t, nil)
	c.instanceTypes.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 8, MemInGiB: 16},
		instancetype.ReasonCapacity,
	)

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

	c.instanceTypes.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 4, MemInGiB: 8},
		instancetype.ReasonCapacity,
	)
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
	if got := published(t, kubeClient).ResourceVersion; got == first {
		t.Error("a newly refused shape did not reach the view")
	}
}

func TestObservedAtIsRefreshedOnAHeartbeat(t *testing.T) {
	c, kubeClient := newController(t, nil)

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
	c, kubeClient := newController(t, nil)

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
	c, kubeClient := newController(t, nil)
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

func TestQuotaIsOmittedWhenUnknown(t *testing.T) {
	c, kubeClient := newController(t, nil)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	if quota := published(t, kubeClient).Status.Quota; quota != nil {
		t.Errorf("quota = %+v, want it omitted when the quota is unknown", quota)
	}
}

func TestViewIsAddressableByName(t *testing.T) {
	c, kubeClient := newController(t, nil)
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
	c, kubeClient := newController(t, nil)

	c.instanceTypes.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 32, MemInGiB: 64},
		instancetype.ReasonCapacity,
	)
	c.instanceTypes.RecordICE(
		t.Context(),
		instancetype.Specs{Ncpus: 31, MemInGiB: 62},
		instancetype.ReasonCapacity,
	)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	view := published(t, kubeClient)
	if got := len(view.Status.InstanceTypes.UnavailableRanges); got != 1 {
		t.Fatalf("published %d ranges, want the walk collapsed into 1", got)
	}
	unavailable := view.Status.InstanceTypes.UnavailableRanges[0]

	if unavailable.RefusedInstanceType != "31-62" {
		t.Errorf("refused shape is %q, want 31-62", unavailable.RefusedInstanceType)
	}

	if unavailable.FromNcpus >= 31 {
		t.Errorf("range starts at %d cpus, want it below the refused 31 to reflect the skip",
			unavailable.FromNcpus)
	}
	if unavailable.Range != rangeDescription(instancetype.Specs{
		Ncpus:    unavailable.FromNcpus,
		MemInGiB: unavailable.FromMemoryGiB,
	}) {
		t.Errorf("range text %q does not match its own bounds", unavailable.Range)
	}

	if unavailable.LargestLaunchable != "" {
		t.Errorf("largest launchable is %q, want it absent until something launches",
			unavailable.LargestLaunchable)
	}
}
