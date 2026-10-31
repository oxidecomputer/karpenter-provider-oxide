package bootdisk

import (
	"context"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestTarget(t *testing.T) {
	spec := v1alpha1.OxideBootDiskPoolSpec{}
	for _, test := range []struct {
		nodes int64
		want  int32
	}{
		{nodes: 0, want: 2},
		{nodes: 20, want: 2},
		{nodes: 21, want: 3},
		{nodes: 50, want: 5},
		{nodes: 200, want: 20},
		{nodes: 500, want: 20},
	} {
		if got := target(spec, test.nodes); got != test.want {
			t.Errorf("target(%d) = %d, want %d", test.nodes, got, test.want)
		}
	}
}

func TestRetirementDoesNotOverrideAClaim(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	stored := &v1alpha1.OxideBootDisk{
		Name: "disk",
		Spec: v1alpha1.OxideBootDiskSpec{
			ClaimRef: &v1alpha1.BootDiskOwnerRef{Name: "claim", UID: types.UID("claim-uid")},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored).Build()
	controller := &Controller{kubeClient: kubeClient}
	stale := stored.DeepCopy()
	stale.Spec.ClaimRef = nil
	stale.Spec.Retire = true

	if err := controller.reconcileRecord(context.Background(), stale, nil); err != nil {
		t.Fatal(err)
	}
	current := &v1alpha1.OxideBootDisk{}
	if err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: stored.Name},
		current,
	); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Retire {
		t.Fatal("a stale retirement update overrode a launch claim")
	}
}

func TestClaimedRecordSurvivesAStaleNodeClaimCache(t *testing.T) {
	record := &v1alpha1.OxideBootDisk{
		Name: "disk",
		Spec: v1alpha1.OxideBootDiskSpec{
			DiskName: "pool-disk",
			ClaimRef: &v1alpha1.BootDiskOwnerRef{Name: "claim", UID: types.UID("claim-uid")},
		},
	}
	claim := &karpv1.NodeClaim{
		Name: "claim", UID: types.UID("claim-uid"),
	}

	for _, tc := range []struct {
		name      string
		claimedAt *metav1.Time
		objects   []client.Object
	}{
		{
			name:      "within the claim grace window",
			claimedAt: &metav1.Time{Time: time.Now()},
			objects:   []client.Object{record},
		},
		{
			name:    "confirmed live against the API server",
			objects: []client.Object{record, claim},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := record.DeepCopy()
			stale.Spec.ClaimedAt = tc.claimedAt
			kubeClient := fake.NewClientBuilder().
				WithScheme(kubescheme.Scheme).
				WithObjects(tc.objects...).
				Build()
			// A nil pool panics on any Oxide call, so reaching one fails loudly.
			controller := &Controller{kubeClient: kubeClient, reader: kubeClient}

			if err := controller.reconcileRecord(
				context.Background(),
				stale,
				map[types.UID]struct{}{},
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadStateSizesEachPoolFromItsNodeClass(t *testing.T) {
	ready := &v1alpha1.OxideNodeClass{
		Name: "ready", UID: types.UID("ready-uid"),
		Spec: v1alpha1.OxideNodeClassSpec{Storage: v1alpha1.Storage{
			Boot: v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
		}},
		Status: v1alpha1.OxideNodeClassStatus{Images: []v1alpha1.OxideImage{{ID: "image"}}},
	}
	unresolved := ready.DeepCopy()
	unresolved.Name, unresolved.UID = "unresolved", types.UID("unresolved-uid")
	unresolved.Status.Images = nil
	nodePool := &karpv1.NodePool{
		Name: "workers",
		Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{
			Spec: karpv1.NodeClaimTemplateSpec{NodeClassRef: &karpv1.NodeClassReference{
				Group: "karpenter.oxide.computer", Kind: "OxideNodeClass", Name: ready.Name,
			}},
		}},
	}
	minReady := int32(5)
	pool := func(name string, class string, age time.Duration) *v1alpha1.OxideBootDiskPool {
		return &v1alpha1.OxideBootDiskPool{
			Name:              name,
			UID:               types.UID(name + "-uid"),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			Spec: v1alpha1.OxideBootDiskPoolSpec{
				NodeClassRef: v1alpha1.NodeClassReference{Name: class},
				MinReady:     &minReady,
			},
		}
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(kubescheme.Scheme).
		WithObjects(
			ready, unresolved, nodePool,
			pool("ready-pool", ready.Name, time.Hour),
			pool("duplicate-pool", ready.Name, time.Minute),
			pool("unresolved-pool", unresolved.Name, time.Hour),
			pool("orphan-pool", "missing", time.Hour),
		).
		Build()

	state, err := (&Controller{kubeClient: kubeClient}).readState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := state.desiredByUID["ready-pool-uid"]; got != minReady {
		t.Errorf("desired for the resolved pool = %d, want %d", got, minReady)
	}
	if len(state.desiredByUID) != 1 {
		t.Errorf("desired = %v, want only the resolved pool", state.desiredByUID)
	}
	for name, reason := range map[string]string{
		"duplicate-pool":  v1alpha1.ConditionReasonNodeClassClaimed,
		"unresolved-pool": v1alpha1.ConditionReasonNodeClassNotReady,
		"orphan-pool":     v1alpha1.ConditionReasonNodeClassNotFound,
	} {
		if got := state.unresolved[types.UID(name+"-uid")].reason; got != reason {
			t.Errorf("%s reason = %q, want %q", name, got, reason)
		}
	}
}
