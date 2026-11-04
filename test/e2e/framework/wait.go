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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// EventuallyNodeReady waits for the pool's nodeclaim to be Ready, then its node to be
// Ready and the pool's pod to be Running, and returns the node. Waits assert on
// conditions so a timeout prints the last reason and message.
func (e *Env) EventuallyNodeReady(g gomega.Gomega, pool *karpv1.NodePool) *corev1.Node {
	ctx := context.Background()
	var claim karpv1.NodeClaim
	g.Eventually(func(g gomega.Gomega) {
		found, err := e.NodeClaim(ctx, pool)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(found).NotTo(gomega.BeNil(), "no nodeclaim for nodepool %s yet", pool.Name)
		claim = *found
		g.Expect(conditionTrue(claim.StatusConditions().Root())).
			To(gomega.BeTrue(), "nodeclaim %s: %s", claim.Name, describe(claim.StatusConditions()))
	}).WithTimeout(e.ReadyTimeout).WithPolling(e.Poll).Should(gomega.Succeed())

	var node corev1.Node
	g.Eventually(func(g gomega.Gomega) {
		g.Expect(claim.Status.NodeName).
			NotTo(gomega.BeEmpty(), "nodeclaim %s has no node", claim.Name)
		g.Expect(e.Kube.Get(ctx, types.NamespacedName{Name: claim.Status.NodeName}, &node)).
			To(gomega.Succeed())
		g.Expect(nodeReady(&node)).
			To(gomega.BeTrue(), "node %s: %s", node.Name, nodeConditions(&node))
	}).WithTimeout(e.ReadyTimeout).WithPolling(e.Poll).Should(gomega.Succeed())

	g.Eventually(func(g gomega.Gomega) {
		var pods corev1.PodList
		g.Expect(e.Kube.List(ctx, &pods, client.InNamespace(e.Namespace), client.MatchingLabels{
			LabelPool: pool.Name,
		})).To(gomega.Succeed())
		for i := range pods.Items {
			pod := &pods.Items[i]
			g.Expect(pod.Status.Phase).To(gomega.Equal(corev1.PodRunning),
				"pod %s is %s on %q: %s %s", pod.Name, pod.Status.Phase, pod.Spec.NodeName,
				pod.Status.Reason, pod.Status.Message)
			g.Expect(pod.Spec.NodeName).To(gomega.Equal(node.Name))
		}
	}).WithTimeout(e.ReadyTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
	return &node
}

// NodeClaim is the pool's single nodeclaim, or nil before Karpenter created it.
func (e *Env) NodeClaim(ctx context.Context, pool *karpv1.NodePool) (*karpv1.NodeClaim, error) {
	var claims karpv1.NodeClaimList
	if err := e.Kube.List(ctx, &claims, client.MatchingLabels{
		karpv1.NodePoolLabelKey: pool.Name,
	}); err != nil {
		return nil, err
	}
	switch len(claims.Items) {
	case 0:
		return nil, nil
	case 1:
		return &claims.Items[0], nil
	default:
		names := make([]string, 0, len(claims.Items))
		for _, c := range claims.Items {
			names = append(names, c.Name)
		}
		return nil, fmt.Errorf("nodepool %s has %d nodeclaims: %s", pool.Name, len(names), names)
	}
}

// EventuallyGone waits for the pool's nodeclaims, then each launch's Oxide instance and
// disks, to disappear. Failures are reported but do not stop the remaining checks.
func (e *Env) EventuallyGone(
	ctx context.Context,
	t *testing.T,
	pool *karpv1.NodePool,
	launches []launch,
) {
	t.Helper()
	g := gomega.NewGomega(func(message string, _ ...int) {
		t.Helper()
		t.Errorf("%s", message)
	})
	if pool != nil {
		g.Eventually(func(g gomega.Gomega) {
			var claims karpv1.NodeClaimList
			g.Expect(e.Kube.List(ctx, &claims, client.MatchingLabels{
				karpv1.NodePoolLabelKey: pool.Name,
			})).To(gomega.Succeed())
			for _, c := range claims.Items {
				g.Expect(c.DeletionTimestamp).NotTo(gomega.BeNil(),
					"nodeclaim %s of deleted nodepool %s is not deleting", c.Name, pool.Name)
			}
			g.Expect(claims.Items).To(gomega.BeEmpty(), "nodeclaims of %s still exist: %s",
				pool.Name, describeClaims(claims.Items))
		}).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
	}
	for _, l := range launches {
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
				disks, err := e.disksWithPrefix(ctx, l.DiskPrefix)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(disks).To(gomega.BeEmpty(),
					"disks of nodeclaim %s still exist: %s", l.NodeClaim, disks)
			}).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
		}
	}
}

func (e *Env) disksWithPrefix(ctx context.Context, prefix string) ([]string, error) {
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
