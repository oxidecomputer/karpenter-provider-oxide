package bootdisk

import (
	"context"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
