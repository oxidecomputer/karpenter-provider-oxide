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

// fakeInventory is a rack reading a test can set, standing in for the sled provider.
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

// Nothing publishes this object but the controller, so the first reconcile has to create it rather
// than wait for one to exist.
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

// The two counts answer different questions and an operator reading the object needs both. One small
// refused shape is a single boundary and simultaneously hundreds of instance types the scheduler cannot
// pick, and a view reporting only the boundary reads as "almost nothing is wrong".
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
	// Every shape with at least 2 CPUs and 4 GiB, which is nearly the whole list.
	if got := view.Status.InstanceTypes.UnavailableCount; got <= 1 {
		t.Errorf("unavailable instance types = %d, want every shape the boundary covers", got)
	}
	if view.Status.InstanceTypes.UnavailableCount > view.Status.InstanceTypes.Count {
		t.Errorf("unavailable instance types = %d exceeds the total of %d",
			view.Status.InstanceTypes.UnavailableCount, view.Status.InstanceTypes.Count)
	}
}

// The point of the whole object: a refused shape and when it will be tried again, readable without
// going anywhere near a log.
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

	// Smallest first, so the first entry is the boundary of what is refused.
	smallest := view.Status.InstanceTypes.UnavailableRanges[0]
	if smallest.RefusedInstanceType != "8-16" {
		t.Errorf("first entry refused %q, want the smallest refused shape 8-16",
			smallest.RefusedInstanceType)
	}
	// A single refusal does not start a search, so the range begins exactly where the refusal did.
	if smallest.FromNcpus != 8 || smallest.FromMemoryGiB != 16 {
		t.Errorf("range starts at %d cpus and %d GiB, want 8 and 16",
			smallest.FromNcpus, smallest.FromMemoryGiB)
	}
	// The conjunction is the whole meaning, and is what naming a single instance type hid.
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

// The question this field exists to answer: nothing is scaling up, every shape is available, so what
// is wrong. A storage refusal has no shape to hang off, so without a place of its own in the view the
// only evidence of it is in the logs.
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
	// The whole point of keeping this off the shape list. A shape marked unavailable here would be one
	// the scheduler stops offering over a limit it never hit.
	if got := view.Status.InstanceTypes.UnavailableRangeCount; got != 0 {
		t.Errorf("%d shapes were taken out of consideration by a storage refusal, want 0", got)
	}
}

// A launch that succeeds created a boot disk, which is direct evidence that storage is available. A
// view still reporting the old refusal would send an operator after a limit that has already cleared.
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

// A rack reading has to arrive per sled, since that is what makes the numbers mean anything, and a
// sled that accepts nothing has to report no headroom rather than headroom nobody can use.
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
	// Sorted by ID, so the object does not churn on the order the rack was listed in.
	if got := view.Status.Inventory.Sleds[0].ID; got != "sled-a" {
		t.Errorf("first sled is %q, want sled-a", got)
	}
	// The largest shape comes from the emptiest single sled, never from the rack's total.
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

// The gap this closes. A rack with no room for most of the catalog rules out hundreds of instance
// types without refusing anything, and before these ranges the count was the only trace of it: an
// operator saw 656 of 1104 unavailable, an empty list of ranges, and nothing joining the two.
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
	// One sled, one entry. Splitting it into a CPU edge and a memory edge said the same thing in two
	// rows that each read as a constraint that had to be met on its own.
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
	// Every one of them has to be explained by the ranges, which is the whole point of publishing them.
	if view.Status.InstanceTypes.UnavailableCount == 0 {
		t.Error("no instance types are unavailable on a rack with one small sled")
	}

	headroom := ranges[0]
	if headroom.MaxNcpus != 8 || headroom.MaxMemoryGiB != 16 {
		t.Errorf("headroom = %d cpus, %d GiB, want the sled's free space",
			headroom.MaxNcpus, headroom.MaxMemoryGiB)
	}
	// An "or", because outgrowing the sled in either dimension is already the whole condition.
	if got := headroom.Range; got != ">8 vCPUs or >16 GiB" {
		t.Errorf("range = %q, want both dimensions in one sentence joined by or", got)
	}
	// The corner belongs to a refusal, and zeroes there would read as a threshold of zero.
	if headroom.FromNcpus != 0 || headroom.FromMemoryGiB != 0 {
		t.Errorf("an inventory range carries a refusal corner: %+v", headroom)
	}

	for _, unavailable := range ranges {
		if unavailable.Reason != v1alpha1.RangeReasonNoSledWithRoom {
			t.Errorf("reason = %q, want %q for a range no launch was refused from",
				unavailable.Reason, v1alpha1.RangeReasonNoSledWithRoom)
		}
		// Nothing was attempted, so the fields that only a refusal can fill have to be absent rather
		// than zeroed: a shape name and a state would both be inventions.
		if unavailable.RefusedInstanceType != "" || unavailable.State != "" ||
			unavailable.ConsecutiveRefusals != 0 || unavailable.RetryAfter != nil {
			t.Errorf("a range read off inventory carries refusal evidence: %+v", unavailable)
		}
		// The other end of the boundary, which is what makes the range actionable.
		if got := unavailable.LargestLaunchable; got != "8-16" {
			t.Errorf("largest launchable = %q, want the shape the rack does have room for", got)
		}
	}
}

// A stale reading rules nothing out, so it must not produce ranges either. Publishing them would
// describe a constraint the provider is not applying, which is worse than saying nothing.
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

// A stale reading is ignored when shapes are judged, and everything read off it is reported under
// inventory next to the state saying so. Nothing about the reading is published anywhere else, since a
// frontier at the top level would read as a constraint the provider is applying.
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
	// Reported, but only under the state that says it is not being acted on. The per-sled numbers stay
	// too, since they are the evidence for the reading being stale.
	if got := view.Status.Inventory.LargestFeasibleInstanceType; got != "8-16" {
		t.Errorf("largest feasible = %q, want the stale reading reported beside its state", got)
	}
	if len(view.Status.Inventory.Sleds) == 0 {
		t.Error("the stale reading was dropped entirely, leaving nothing to explain the state")
	}
}

// Inventory being on but unread is its own state, and almost always means a token without fleet
// access. Reporting it as disabled would send an operator looking at the wrong setting.
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

// A rack refusing launches steadily must not turn into a stream of writes to etcd. Nothing has
// changed between these reconciles, so the object must be left alone.
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

	// A change does have to land, or the view is worse than useless.
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

// The timestamp is the only thing that distinguishes a quiet cluster from a controller that has
// stopped publishing, so it cannot be allowed to age indefinitely just because nothing else changed.
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

// The object is a view, not a lock. A user who deletes it gets it back rather than losing the view
// until the controller restarts.
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

// The expected state between upgrading the controller and upgrading the CRD chart. Nothing launches
// nodes off this object, so a missing CRD has to degrade to metrics only rather than fail a reconcile
// or push the controller into backoff.
func TestAMissingCRDDoesNotFailTheReconcile(t *testing.T) {
	// An uninstalled CRD is a kind the cluster has no mapping for, which is what the apiserver tells a
	// client that asks for one. installed is flipped later to let the same client succeed.
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

	// And it recovers on its own once the CRD arrives, without a restart.
	installed = true
	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v after the CRD was installed", err)
	}
	if c.crdMissingLogged {
		t.Error("the controller still believes the CRD is missing after publishing to it")
	}
	published(t, kubeClient)
}

// A quota that has never been read is not a quota of zero, and publishing it as one would read as a
// silo entitled to nothing.
func TestQuotaIsOmittedWhenUnknown(t *testing.T) {
	c, kubeClient := newController(t, nil)

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}

	if quota := published(t, kubeClient).Status.Quota; quota != nil {
		t.Errorf("quota = %+v, want it omitted when the quota is unknown", quota)
	}
}

// Guards the fake client behaving as expected: the view is a real object, so a plain Get by name is
// how a user reaches it.
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

// Once a walk down through sizes is established the provider stops offering shapes well below the
// last one refused, rather than testing every size on the way down. The published range has to say
// so: reporting only the refused shape would claim the scheduler still has everything between them,
// when in fact none of it is being offered.
func TestReconcilePublishesTheSkippedRangeNotJustTheRefusal(t *testing.T) {
	c, kubeClient := newController(t, nil)

	// Two refusals in a row, each smaller than the last, which is what a walk looks like.
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
	// The corner is where the scheduler actually stops being offered anything, which is below the
	// refusal because the second refusal established the walk and the search skipped ahead.
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
	// Nothing has launched yet, so there is no lower end to the search to report.
	if unavailable.LargestLaunchable != "" {
		t.Errorf("largest launchable is %q, want it absent until something launches",
			unavailable.LargestLaunchable)
	}
}
