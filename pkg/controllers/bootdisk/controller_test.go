package bootdisk

import (
	"context"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestTarget(t *testing.T) {
	boot := v1alpha1.BootDisk{}
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
		if got := target(boot, test.nodes); got != test.want {
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
		ObjectMeta: metav1.ObjectMeta{Name: "disk"},
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
		ObjectMeta: metav1.ObjectMeta{Name: "disk"},
		Spec: v1alpha1.OxideBootDiskSpec{
			DiskName: "pool-disk",
			ClaimRef: &v1alpha1.BootDiskOwnerRef{Name: "claim", UID: types.UID("claim-uid")},
		},
	}
	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", UID: types.UID("claim-uid")},
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
