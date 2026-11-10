package availability

import (
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestObservedNodeClaimsSeedAvailabilityWithoutACapacityPublisher(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*karpv1.NodeClaim)
		wantFloor bool
	}{
		{"launched", func(*karpv1.NodeClaim) {}, true},
		{"pending", func(c *karpv1.NodeClaim) { c.StatusConditions().SetUnknown(karpv1.ConditionTypeLaunched) }, false},
		{"deleting", func(c *karpv1.NodeClaim) { now := metav1.Now(); c.DeletionTimestamp = &now }, false},
		{"other provider", func(c *karpv1.NodeClaim) { c.Spec.NodeClassRef.Group = "other.example" }, false},
		{"invalid shape", func(c *karpv1.NodeClaim) { c.Labels[corev1.LabelInstanceTypeStable] = "unknown" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := &karpv1.NodeClaim{
				Spec: karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{
					Group: apis.Group, Kind: "OxideNodeClass", Name: "default"}},
				Labels: map[string]string{
					corev1.LabelInstanceTypeStable:    "4-8",
					v1alpha1.LabelInstanceCPUPlatform: "amd_milan",
				},
			}
			claim.StatusConditions().SetTrue(karpv1.ConditionTypeLaunched)
			tc.change(claim)
			m := availability.New(nil)
			if _, err := NewController(m).Reconcile(t.Context(), claim); err != nil {
				t.Fatal(err)
			}
			m.RecordICE(
				t.Context(),
				availability.Shape{Ncpus: 16, MemInGiB: 32},
				"amd_milan",
				availability.ReasonCapacity,
			)
			floor := m.Snapshot().Entries()[0].Floor
			if got := floor == (availability.Shape{Ncpus: 4, MemInGiB: 8}); got != tc.wantFloor {
				t.Fatalf("seeded floor = %+v, want seeded = %v", floor, tc.wantFloor)
			}
		})
	}
}
