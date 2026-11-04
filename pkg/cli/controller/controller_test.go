package controller

import (
	"context"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

// Only RepairPolicies is called; the embedded nil interface panics on anything else.
type policyOnlyCloudProvider struct {
	karpcloudprovider.CloudProvider
	policies []karpcloudprovider.RepairPolicy
}

func (p policyOnlyCloudProvider) RepairPolicies() []karpcloudprovider.RepairPolicy {
	return p.policies
}

func repairMatcher(t *testing.T) *health.RepairPolicyMatcher {
	t.Helper()
	ctx := options.ToContext(context.Background(), &options.Options{
		FeatureGates: options.FeatureGates{NodeRepair: true},
	})
	matcher, err := health.NewRepairPolicyMatcher(
		ctx,
		policyOnlyCloudProvider{policies: repairPolicies()},
	)
	if err != nil {
		t.Fatalf("core rejected the repair policies: %v", err)
	}
	return matcher
}

func unhealthyNode(condition corev1.NodeCondition, since time.Time) *corev1.Node {
	condition.LastTransitionTime = metav1.NewTime(since)
	node := &corev1.Node{}
	node.CreationTimestamp = metav1.NewTime(since.Add(-time.Hour))
	node.Status.Conditions = []corev1.NodeCondition{condition}
	return node
}

func TestRepairPoliciesRebootANotReadyNodeAfterTheToleration(t *testing.T) {
	matcher := repairMatcher(t)
	since := time.Now()

	for _, status := range []corev1.ConditionStatus{corev1.ConditionFalse, corev1.ConditionUnknown} {
		node := unhealthyNode(corev1.NodeCondition{
			Type: corev1.NodeReady, Status: status, Reason: "KubeletNotReady",
		}, since)

		tooEarly := health.Resolve(
			matcher.Match(node),
			since.Add(readyRepairTolerationDuration-time.Minute),
			since,
		)
		if tooEarly.Action != "" {
			t.Errorf(
				"Ready=%s was repaired with %q before the toleration elapsed",
				status,
				tooEarly.Action,
			)
		}

		result := health.Resolve(
			matcher.Match(node),
			since.Add(readyRepairTolerationDuration+time.Minute),
			since,
		)
		if result.Action != karpcloudprovider.RebootNode {
			t.Errorf(
				"Ready=%s resolved to %q, want %q",
				status,
				result.Action,
				karpcloudprovider.RebootNode,
			)
		}
		if result.TerminationGracePeriod == nil || *result.TerminationGracePeriod != 0 {
			t.Errorf(
				"Ready=%s drain bound is %v, want a forceful 0",
				status,
				result.TerminationGracePeriod,
			)
		}
	}
}

func TestRepairPoliciesReplaceANodeOnAnUnhealthySled(t *testing.T) {
	matcher := repairMatcher(t)
	since := time.Now()
	node := unhealthyNode(corev1.NodeCondition{
		Type: v1alpha1.ConditionTypeSledUnhealthy, Status: corev1.ConditionTrue, Reason: "Expunged",
	}, since)

	result := health.Resolve(
		matcher.Match(node),
		since.Add(inventoryRepairTolerationDuration+time.Minute),
		since,
	)
	if result.Action != karpcloudprovider.ReplaceNode {
		t.Errorf(
			"SledUnhealthy resolved to %q, want %q",
			result.Action,
			karpcloudprovider.ReplaceNode,
		)
	}
}
