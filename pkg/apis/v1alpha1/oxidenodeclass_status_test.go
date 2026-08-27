package v1alpha1

import (
	"testing"

	"github.com/awslabs/operatorpkg/status"
)

func TestReadyIsDerivedFromImagesAndSubnetsResolved(t *testing.T) {
	for _, tt := range []struct {
		name      string
		nodeClass *OxideNodeClass
	}{

		{"no bootstrap", &OxideNodeClass{}},
		{"rke2 without a token ref", &OxideNodeClass{Spec: OxideNodeClassSpec{
			Bootstrap: &Bootstrap{RKE2: &RKE2Bootstrap{}},
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nodeClass := tt.nodeClass
			nodeClass.StatusConditions().SetTrue(ConditionTypeSubnetsResolved)
			nodeClass.StatusConditions().SetTrue(ConditionTypeStorageResolved)

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

func TestReadyRemainsFalseWithoutResolvedSubnets(t *testing.T) {
	nodeClass := &OxideNodeClass{}
	nodeClass.StatusConditions().SetTrue(ConditionTypeImagesResolved)
	nodeClass.StatusConditions().SetTrue(ConditionTypeStorageResolved)
	nodeClass.StatusConditions().SetFalse(
		ConditionTypeSubnetsResolved, "SubnetsNotFound", "no subnets matched {vpcName=prod}",
	)
	if cond := nodeClass.StatusConditions().Get(status.ConditionReady); !cond.IsFalse() {
		t.Errorf("Ready = %s with SubnetsResolved false, want False", cond.Status)
	}
}

func TestReadyIsFalseWhenABootstrapConditionIsFalse(t *testing.T) {
	nodeClass := &OxideNodeClass{Spec: OxideNodeClassSpec{
		Bootstrap: &Bootstrap{Kubeadm: &KubeadmBootstrap{}},
	}}

	nodeClass.StatusConditions().SetTrue(ConditionTypeImagesResolved)
	nodeClass.StatusConditions().SetTrue(ConditionTypeSubnetsResolved)
	nodeClass.StatusConditions().SetTrue(ConditionTypeStorageResolved)
	nodeClass.StatusConditions().
		SetFalse(ConditionTypeClusterInfoAvailable, "ClusterInfoUnavailable", "boom")
	if cond := nodeClass.StatusConditions().Get(status.ConditionReady); !cond.IsFalse() {
		t.Errorf("Ready = %s with ClusterInfoAvailable false, want False", cond.Status)
	}
}
