package framework

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/awslabs/operatorpkg/status"
	"github.com/onsi/gomega"
	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// EventuallyNodeReady is EventuallyNodesReady for a pool that should hold one node.
func (e *Env) EventuallyNodeReady(g gomega.Gomega, pool *karpv1.NodePool) *corev1.Node {
	return e.EventuallyNodesReady(g, pool, 1)[0]
}

// EventuallyNodesReady returns n ready nodes with ready, non-deleting NodeClaims
// once all non-deleting pool pods are running on them.
// More than n non-deleting NodeClaims fails immediately.
func (e *Env) EventuallyNodesReady(
	g gomega.Gomega, pool *karpv1.NodePool, n int,
) []*corev1.Node {
	ctx := context.Background()
	var nodes []*corev1.Node
	g.Eventually(func(g gomega.Gomega) {
		claims, err := e.liveNodeClaims(ctx, pool)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		if len(claims) > n {
			gomega.StopTrying(fmt.Sprintf("nodepool %s has %d nodeclaims, want %d: %s",
				pool.Name, len(claims), n, describeClaims(claims))).Now()
		}
		g.Expect(claims).To(gomega.HaveLen(n), "nodeclaims of nodepool %s: %s",
			pool.Name, describeClaims(claims))

		found := make([]*corev1.Node, 0, n)
		byName := map[string]bool{}
		for i := range claims {
			claim := &claims[i]
			g.Expect(conditionTrue(claim.StatusConditions().Root())).To(gomega.BeTrue(),
				"nodeclaim %s: %s", claim.Name, describe(claim.StatusConditions()))
			g.Expect(claim.Status.NodeName).
				NotTo(gomega.BeEmpty(), "nodeclaim %s has no node", claim.Name)
			node := &corev1.Node{}
			g.Expect(e.Kube.Get(ctx, types.NamespacedName{Name: claim.Status.NodeName}, node)).
				To(gomega.Succeed())
			g.Expect(nodeReady(node)).
				To(gomega.BeTrue(), "node %s: %s", node.Name, nodeConditions(node))
			found = append(found, node)
			byName[node.Name] = true
		}

		var pods corev1.PodList
		g.Expect(e.Kube.List(ctx, &pods, client.InNamespace(e.Namespace), client.MatchingLabels{
			LabelPool: pool.Name,
		})).To(gomega.Succeed())
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.DeletionTimestamp != nil {
				continue
			}
			g.Expect(pod.Status.Phase).To(gomega.Equal(corev1.PodRunning),
				"pod %s is %s on %q: %s %s", pod.Name, pod.Status.Phase, pod.Spec.NodeName,
				pod.Status.Reason, pod.Status.Message)
			g.Expect(byName).To(gomega.HaveKey(pod.Spec.NodeName),
				"pod %s runs on %s, not on a node of the pool", pod.Name, pod.Spec.NodeName)
		}
		nodes = found
	}).WithTimeout(e.ReadyTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
	return nodes
}

// NodeClaims lists every nodeclaim of the pool, deleting ones included.
func (e *Env) NodeClaims(ctx context.Context, pool *karpv1.NodePool) ([]karpv1.NodeClaim, error) {
	var claims karpv1.NodeClaimList
	if err := e.Kube.List(ctx, &claims, client.MatchingLabels{
		karpv1.NodePoolLabelKey: pool.Name,
	}); err != nil {
		return nil, err
	}
	return claims.Items, nil
}

func (e *Env) liveNodeClaims(
	ctx context.Context, pool *karpv1.NodePool,
) ([]karpv1.NodeClaim, error) {
	claims, err := e.NodeClaims(ctx, pool)
	if err != nil {
		return nil, err
	}
	live := claims[:0]
	for _, c := range claims {
		if c.DeletionTimestamp == nil {
			live = append(live, c)
		}
	}
	return live, nil
}

// NodeClaim is the pool's single nodeclaim that is not being deleted, or nil before
// Karpenter created one.
func (e *Env) NodeClaim(ctx context.Context, pool *karpv1.NodePool) (*karpv1.NodeClaim, error) {
	claims, err := e.liveNodeClaims(ctx, pool)
	if err != nil {
		return nil, err
	}
	switch len(claims) {
	case 0:
		return nil, nil
	case 1:
		return &claims[0], nil
	default:
		names := make([]string, 0, len(claims))
		for _, c := range claims {
			names = append(names, c.Name)
		}
		return nil, fmt.Errorf("nodepool %s has %d nodeclaims: %s", pool.Name, len(names), names)
	}
}

// EventuallyNodeClaimDeleting waits until the nodeclaim is being deleted, or is gone.
func (e *Env) EventuallyNodeClaimDeleting(g gomega.Gomega, name string) {
	ctx := context.Background()
	g.Eventually(func(g gomega.Gomega) {
		var claim karpv1.NodeClaim
		err := e.Kube.Get(ctx, types.NamespacedName{Name: name}, &claim)
		if apierrors.IsNotFound(err) {
			return
		}
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(claim.DeletionTimestamp).NotTo(gomega.BeNil(),
			"nodeclaim %s is not deleting: %s", name, describe(claim.StatusConditions()))
	}).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
}

// EventuallyNoNodeClaims waits until the pool has no nodeclaims at all.
func (e *Env) EventuallyNoNodeClaims(g gomega.Gomega, pool *karpv1.NodePool) {
	ctx := context.Background()
	g.Eventually(func(g gomega.Gomega) {
		claims, err := e.NodeClaims(ctx, pool)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(claims).To(gomega.BeEmpty(), "nodeclaims of %s still exist: %s",
			pool.Name, describeClaims(claims))
	}).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
}

// EventuallyNodeGone waits until the node object is deleted.
func (e *Env) EventuallyNodeGone(g gomega.Gomega, name string) {
	ctx := context.Background()
	g.Eventually(func() error {
		return e.Kube.Get(ctx, types.NamespacedName{Name: name}, &corev1.Node{})
	}).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).
		Should(gomega.MatchError(apierrors.IsNotFound, "node not found"), "node %s", name)
}

// EventuallyLaunchGone waits until the launch's instance and disks are gone from Oxide.
func (e *Env) EventuallyLaunchGone(g gomega.Gomega, l Launch) {
	ctx := context.Background()
	if l.InstanceID != "" {
		g.Eventually(func(g gomega.Gomega) {
			_, err := e.Oxide.InstanceView(ctx, oxide.InstanceViewParams{
				Instance: oxide.NameOrId(l.InstanceID),
			})
			g.Expect(errors.Is(err, oxide.ErrObjectNotFound)).To(gomega.BeTrue(),
				"instance %s of nodeclaim %s still exists (%v)", l.InstanceID, l.NodeClaim, err)
		}).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
	}
	if l.DiskPrefix != "" {
		g.Eventually(func(g gomega.Gomega) {
			disks, err := e.DisksWithPrefix(ctx, l.DiskPrefix)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(disks).To(gomega.BeEmpty(),
				"disks of nodeclaim %s still exist: %s", l.NodeClaim, disks)
		}).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
	}
}

// EventuallyGone is the cleanup's wait: the pool's nodeclaims, then each launch's Oxide
// instance and disks, must disappear. Failures are reported but do not stop the
// remaining checks.
func (e *Env) EventuallyGone(
	ctx context.Context,
	t *testing.T,
	pool *karpv1.NodePool,
	launches []Launch,
) {
	t.Helper()
	g := gomega.NewGomega(func(message string, _ ...int) {
		t.Helper()
		t.Errorf("%s", message)
	})
	if pool != nil {
		g.Eventually(func(g gomega.Gomega) {
			claims, err := e.NodeClaims(ctx, pool)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			for _, c := range claims {
				g.Expect(c.DeletionTimestamp).NotTo(gomega.BeNil(),
					"nodeclaim %s of deleted nodepool %s is not deleting", c.Name, pool.Name)
			}
			g.Expect(claims).To(gomega.BeEmpty(), "nodeclaims of %s still exist: %s",
				pool.Name, describeClaims(claims))
		}).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
	}
	for _, l := range launches {
		e.EventuallyLaunchGone(g, l)
	}
}

// DisksWithPrefix names the project's disks whose name starts with prefix.
func (e *Env) DisksWithPrefix(ctx context.Context, prefix string) ([]string, error) {
	disks, err := e.Oxide.DiskListAllPages(ctx, oxide.DiskListParams{
		Project: oxide.NameOrId(e.Project),
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range disks {
		if strings.HasPrefix(string(d.Name), prefix) {
			names = append(names, string(d.Name))
		}
	}
	return names, nil
}

func conditionTrue(c *status.Condition) bool {
	return c != nil && c.Status == "True"
}

// describe renders a condition set as one line, the root first, for a wait's message.
func describe(set status.ConditionSet) string {
	parts := []string{}
	for _, c := range set.List() {
		if c.Status == "True" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s(%s: %s)", c.Type, c.Status, c.Reason, c.Message))
	}
	if len(parts) == 0 {
		return "no conditions yet"
	}
	return strings.Join(parts, "; ")
}

func describeClaims(claims []karpv1.NodeClaim) string {
	parts := make([]string, 0, len(claims))
	for _, c := range claims {
		parts = append(parts, c.Name+": "+describe(c.StatusConditions()))
	}
	return strings.Join(parts, "; ")
}

func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeConditions(node *corev1.Node) string {
	parts := []string{}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady || c.Status != corev1.ConditionFalse {
			parts = append(
				parts,
				fmt.Sprintf("%s=%s(%s: %s)", c.Type, c.Status, c.Reason, c.Message),
			)
		}
	}
	return strings.Join(parts, "; ")
}
