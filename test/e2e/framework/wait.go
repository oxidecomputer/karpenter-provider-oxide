package framework

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	"github.com/onsi/gomega"
	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// EventuallyNodeReady is EventuallyNodesReady for a pool that should hold one node.
func (e *Env) EventuallyNodeReady(
	ctx context.Context,
	g gomega.Gomega,
	pool *karpv1.NodePool,
) *corev1.Node {
	return e.EventuallyNodesReady(ctx, g, pool, 1, 1)[0]
}

// EventuallyNodesReady returns n ready nodes with ready, non-deleting NodeClaims
// once all non-deleting pool pods are running on them.
// More than n non-deleting NodeClaims fails immediately.
func (e *Env) EventuallyNodesReady(
	ctx context.Context, g gomega.Gomega, pool *karpv1.NodePool, n, podCount int,
) []*corev1.Node {
	ctx, cancel := context.WithTimeout(ctx, e.ReadyTimeout)
	defer cancel()
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

		g.Expect(e.poolPodsReady(ctx, pool, podCount, byName)).To(gomega.Succeed())
		nodes = found
	}).WithContext(ctx).WithTimeout(e.ReadyTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
	return nodes
}

func (e *Env) poolPodsReady(
	ctx context.Context, pool *karpv1.NodePool, want int, nodes map[string]bool,
) error {
	var pods corev1.PodList
	if err := e.Kube.List(ctx, &pods, client.InNamespace(e.Namespace), client.MatchingLabels{
		LabelPool: pool.Name,
	}); err != nil {
		return err
	}
	count := 0
	var errs []error
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		count++
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if pod.Status.Phase != corev1.PodRunning || !ready {
			errs = append(errs, fmt.Errorf(
				"pod %s is not ready: phase=%s node=%q: %s %s",
				pod.Name,
				pod.Status.Phase,
				pod.Spec.NodeName,
				pod.Status.Reason,
				pod.Status.Message,
			))
		}
		if !nodes[pod.Spec.NodeName] {
			errs = append(errs, fmt.Errorf("pod %s runs on %q, not on an expected node",
				pod.Name, pod.Spec.NodeName))
		}
	}
	if count != want {
		errs = append(errs, fmt.Errorf("nodepool %s has %d non-terminating pods, want %d",
			pool.Name, count, want))
	}
	return errors.Join(errs...)
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
func (e *Env) EventuallyNodeClaimDeleting(ctx context.Context, g gomega.Gomega, name string) {
	ctx, cancel := context.WithTimeout(ctx, e.DeleteTimeout)
	defer cancel()
	g.Eventually(func(g gomega.Gomega) {
		var claim karpv1.NodeClaim
		err := e.Kube.Get(ctx, types.NamespacedName{Name: name}, &claim)
		if apierrors.IsNotFound(err) {
			return
		}
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(claim.DeletionTimestamp).NotTo(gomega.BeNil(),
			"nodeclaim %s is not deleting: %s", name, describe(claim.StatusConditions()))
	}).WithContext(ctx).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
}

// EventuallyNoNodeClaims waits until the pool has no nodeclaims at all.
func (e *Env) EventuallyNoNodeClaims(ctx context.Context, g gomega.Gomega, pool *karpv1.NodePool) {
	ctx, cancel := context.WithTimeout(ctx, e.DeleteTimeout)
	defer cancel()
	g.Eventually(func(g gomega.Gomega) {
		claims, err := e.NodeClaims(ctx, pool)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(claims).To(gomega.BeEmpty(), "nodeclaims of %s still exist: %s",
			pool.Name, describeClaims(claims))
	}).WithContext(ctx).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).Should(gomega.Succeed())
}

// EventuallyNodeGone waits until the node object is deleted.
func (e *Env) EventuallyNodeGone(ctx context.Context, g gomega.Gomega, name string) {
	ctx, cancel := context.WithTimeout(ctx, e.DeleteTimeout)
	defer cancel()
	g.Eventually(func() error {
		return e.Kube.Get(ctx, types.NamespacedName{Name: name}, &corev1.Node{})
	}).WithContext(ctx).WithTimeout(e.DeleteTimeout).WithPolling(e.Poll).
		Should(gomega.MatchError(apierrors.IsNotFound, "node not found"), "node %s", name)
}

// EventuallyLaunchGone waits until the launch's instance and disks are gone from Oxide.
func (e *Env) EventuallyLaunchGone(ctx context.Context, g gomega.Gomega, l Launch) {
	g.Expect(e.waitForCleanup(ctx, nil, []Launch{l})).To(gomega.Succeed())
}

func (e *Env) launchGone(ctx context.Context, l Launch) error {
	var errs []error
	if l.InstanceID != "" {
		_, err := e.Oxide.InstanceView(ctx, oxide.InstanceViewParams{
			Instance: oxide.NameOrId(l.InstanceID),
		})
		if !errors.Is(err, oxide.ErrObjectNotFound) {
			errs = append(
				errs,
				fmt.Errorf("instance %s of nodeclaim %s still exists or cannot be read: %v",
					l.InstanceID, l.NodeClaim, err),
			)
		}
	}
	for _, id := range l.DiskIDs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		_, err := e.Oxide.DiskView(ctx, oxide.DiskViewParams{Disk: oxide.NameOrId(id)})
		if !errors.Is(err, oxide.ErrObjectNotFound) {
			errs = append(
				errs,
				fmt.Errorf("disk %s of nodeclaim %s still exists or cannot be read: %v",
					id, l.NodeClaim, err),
			)
		}
	}
	if len(l.DiskIDs) == 0 && l.DiskPrefix != "" {
		disks, err := e.DisksWithPrefix(ctx, l.DiskPrefix)
		if err != nil {
			errs = append(errs, err)
		} else if len(disks) != 0 {
			errs = append(
				errs,
				fmt.Errorf("disks of nodeclaim %s still exist: %s", l.NodeClaim, disks),
			)
		}
	}
	return errors.Join(errs...)
}

func (e *Env) waitForCleanup(ctx context.Context, pool *karpv1.NodePool, launches []Launch) error {
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, e.Poll, e.DeleteTimeout, true,
		func(ctx context.Context) (bool, error) {
			var errs []error
			if pool != nil {
				claims, err := e.NodeClaims(ctx, pool)
				if err != nil {
					errs = append(errs, err)
				} else if len(claims) != 0 {
					errs = append(errs, fmt.Errorf("nodeclaims of %s still exist: %s",
						pool.Name, describeClaims(claims)))
				}
			}
			for _, l := range launches {
				if err := ctx.Err(); err != nil {
					errs = append(errs, err)
					break
				}
				errs = append(errs, e.launchGone(ctx, l))
			}
			lastErr = errors.Join(errs...)
			return lastErr == nil, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for cleanup: %w", errors.Join(err, lastErr))
	}
	return nil
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
