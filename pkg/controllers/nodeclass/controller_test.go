package nodeclass

import (
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/subnet"
)

func TestSetCondition(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{Spec: v1alpha1.OxideNodeClassSpec{
		SubnetSelector: []v1alpha1.SubnetSelector{{VPCName: "prod"}},
	}}
	unresolved := subnet.Result{
		Reason:  v1alpha1.ConditionReasonSubnetsNotFound,
		Message: subnet.DescribeSelectors(nodeClass.Spec.SubnetSelector),
	}

	setCondition(
		nodeClass,
		v1alpha1.ConditionTypeSubnetsResolved,
		unresolved.Reason,
		unresolved.Message,
	)
	condition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeSubnetsResolved)
	if !condition.IsFalse() || condition.Reason != v1alpha1.ConditionReasonSubnetsNotFound {
		t.Fatalf("empty result condition = %#v, want SubnetsNotFound=False", condition)
	}
	if !strings.Contains(condition.Message, "vpcName=prod") {
		t.Errorf("condition message = %q, want selector description", condition.Message)
	}

	resolved := subnet.Result{Resolved: []v1alpha1.OxideSubnet{{ID: "subnet"}}}
	setCondition(
		nodeClass,
		v1alpha1.ConditionTypeSubnetsResolved,
		resolved.Reason,
		resolved.Message,
	)
	if condition := nodeClass.StatusConditions().
		Get(v1alpha1.ConditionTypeSubnetsResolved); !condition.IsTrue() {
		t.Errorf("resolved result condition = %#v, want True", condition)
	}

	setCondition(
		nodeClass,
		v1alpha1.ConditionTypeSubnetsResolved,
		unresolved.Reason,
		unresolved.Message,
	)
	if condition := nodeClass.StatusConditions().
		Get(v1alpha1.ConditionTypeSubnetsResolved); !condition.IsFalse() {
		t.Errorf("disappeared subnet condition = %#v, want False", condition)
	}
}

func TestKubeletArgsDefaultIsOverridable(t *testing.T) {
	args := bootstrap.MergeKubeletArgs(kubeletArgsDefault, []string{"cloud-provider=", "v=2"})
	want := []string{"cloud-provider=", "v=2"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", args, want)
	}
}

func TestKubeletArgsDefaultAppliesOnItsOwn(t *testing.T) {
	args := bootstrap.MergeKubeletArgs(kubeletArgsDefault, nil)
	if len(args) != 1 || args[0] != "cloud-provider=external" {
		t.Fatalf("got %v", args)
	}
}
