//go:build e2e

package lifecycle

import (
	"context"
	"testing"

	"github.com/onsi/gomega"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/test/e2e/framework"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestNodeClassDriftReplacesNode(t *testing.T) {
	e := framework.Setup(t)
	g := gomega.NewWithT(t)
	ctx := t.Context()

	nc := e.NodeClass("drift", func(nc *v1alpha1.OxideNodeClass) {
		nc.Spec.Kubelet = &v1alpha1.KubeletConfiguration{MaxPods: ptr.To[int32](50)}
	})
	pool := e.NodePool(nc, func(pool *karpv1.NodePool) {
		pool.Spec.Disruption.Budgets = []karpv1.Budget{{Nodes: "0"}}
	})
	deployment := e.Deployment(pool, 1, resources)
	e.Create(t, nc, pool, deployment)
	old := e.EventuallyNodeReady(ctx, g, pool)
	oldClaim, err := e.NodeClaim(ctx, pool)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(oldClaim).NotTo(gomega.BeNil())
	launch := framework.LaunchOf(oldClaim)
	config, err := e.Host(t, old).KubeletConfig(ctx)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(config.MaxPods).To(gomega.BeEquivalentTo(50))

	g.Expect(e.Kube.Get(ctx, client.ObjectKeyFromObject(nc), nc)).To(gomega.Succeed())
	before := nc.DeepCopy()
	nc.Spec.Kubelet.MaxPods = ptr.To[int32](60)
	g.Expect(e.Kube.Patch(ctx, nc, client.MergeFrom(before))).To(gomega.Succeed())

	driftCtx, cancel := context.WithTimeout(ctx, e.ReadyTimeout)
	defer cancel()
	g.Eventually(func(g gomega.Gomega) {
		var current karpv1.NodeClaim
		g.Expect(e.Kube.Get(driftCtx, client.ObjectKeyFromObject(oldClaim), &current)).
			To(gomega.Succeed())
		condition := current.StatusConditions().Get(karpv1.ConditionTypeDrifted)
		g.Expect(condition).NotTo(gomega.BeNil())
		g.Expect(condition.Status).To(gomega.BeEquivalentTo("True"))
		g.Expect(condition.Reason).To(gomega.Equal(string(providers.DriftReasonNodeClass)))
	}).WithContext(driftCtx).WithTimeout(e.ReadyTimeout).WithPolling(e.Poll).Should(gomega.Succeed())

	g.Expect(e.Kube.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(gomega.Succeed())
	previousPool := pool.DeepCopy()
	pool.Spec.Disruption.Budgets = []karpv1.Budget{{Nodes: "1"}}
	g.Expect(e.Kube.Patch(ctx, pool, client.MergeFrom(previousPool))).To(gomega.Succeed())
	e.EventuallyNodeClaimDeleting(ctx, g, oldClaim.Name)
	node := e.EventuallyNodeReady(ctx, g, pool)
	g.Expect(node.Name).NotTo(gomega.Equal(old.Name))
	claim, err := e.NodeClaim(ctx, pool)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(claim).NotTo(gomega.BeNil())
	g.Expect(claim.Name).NotTo(gomega.Equal(oldClaim.Name))
	g.Expect(claim.Status.ProviderID).NotTo(gomega.Equal(oldClaim.Status.ProviderID))
	config, err = e.Host(t, node).KubeletConfig(ctx)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(config.MaxPods).To(gomega.BeEquivalentTo(60))

	e.EventuallyNodeGone(ctx, g, old.Name)
	e.EventuallyLaunchGone(ctx, g, launch)
	settleCtx, settleCancel := context.WithTimeout(ctx, e.DeleteTimeout)
	defer settleCancel()
	g.Eventually(func() ([]karpv1.NodeClaim, error) {
		return e.NodeClaims(settleCtx, pool)
	}).WithContext(settleCtx).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).Should(gomega.HaveLen(1))
}
