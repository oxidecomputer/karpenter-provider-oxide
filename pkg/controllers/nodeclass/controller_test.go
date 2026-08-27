package nodeclass

import (
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/subnet"
)

func TestSetSubnetCondition(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{Spec: v1alpha1.OxideNodeClassSpec{
		SubnetSelector: []v1alpha1.SubnetSelector{{VPCName: "prod"}},
	}}

	setSubnetCondition(nodeClass, subnet.Result{})
	condition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeSubnetsResolved)
	if !condition.IsFalse() || condition.Reason != "SubnetsNotFound" {
		t.Fatalf("empty result condition = %#v, want SubnetsNotFound=False", condition)
	}
	if !strings.Contains(condition.Message, "vpcName=prod") {
		t.Errorf("condition message = %q, want selector description", condition.Message)
	}

	setSubnetCondition(nodeClass, subnet.Result{Resolved: []v1alpha1.OxideSubnet{{ID: "subnet"}}})
	if condition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeSubnetsResolved); !condition.IsTrue() {
		t.Errorf("resolved result condition = %#v, want True", condition)
	}

	setSubnetCondition(nodeClass, subnet.Result{})
	if condition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeSubnetsResolved); !condition.IsFalse() {
		t.Errorf("disappeared subnet condition = %#v, want False", condition)
	}
}
