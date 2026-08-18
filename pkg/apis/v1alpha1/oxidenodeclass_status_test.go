package v1alpha1

import (
	"testing"

	"github.com/awslabs/operatorpkg/status"
)

// The nodeclass controller no longer asserts Ready; it only sets the dependent conditions and lets
// the condition set recompute the root. That only works if at least one dependent is always
// registered, which is the contract these tests pin.
func TestReadyIsDerivedFromImagesResolved(t *testing.T) {
	for _, tt := range []struct {
		name      string
		nodeClass *OxideNodeClass
	}{
		// The bootstrap provider decides which other dependents are registered, including the rke2
		// case that registers none of its own.
		{"no bootstrap", &OxideNodeClass{}},
		{"rke2 without a token ref", &OxideNodeClass{Spec: OxideNodeClassSpec{
			Bootstrap: &Bootstrap{RKE2: &RKE2Bootstrap{}},
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nodeClass := tt.nodeClass

			nodeClass.StatusConditions().
				SetFalse(ConditionTypeImagesResolved, "ImagesNotFound", "no images matched")
			if cond := nodeClass.StatusConditions().Get(status.ConditionReady); !cond.IsFalse() {
				t.Errorf("Ready = %s with ImagesResolved false, want False", cond.Status)
			}

			nodeClass.StatusConditions().SetTrue(ConditionTypeImagesResolved)
			if cond := nodeClass.StatusConditions().Get(status.ConditionReady); !cond.IsTrue() {
				t.Errorf("Ready = %s with ImagesResolved true, want True", cond.Status)
			}
		})
	}
}

// A provider condition going false must hold the nodeclass un-Ready even though images resolved.
func TestReadyIsFalseWhenABootstrapConditionIsFalse(t *testing.T) {
	nodeClass := &OxideNodeClass{Spec: OxideNodeClassSpec{
		Bootstrap: &Bootstrap{Kubeadm: &KubeadmBootstrap{}},
	}}

	nodeClass.StatusConditions().SetTrue(ConditionTypeImagesResolved)
	nodeClass.StatusConditions().
		SetFalse(ConditionTypeClusterInfoAvailable, "ClusterInfoUnavailable", "boom")
	if cond := nodeClass.StatusConditions().Get(status.ConditionReady); !cond.IsFalse() {
		t.Errorf("Ready = %s with ClusterInfoAvailable false, want False", cond.Status)
	}
}
