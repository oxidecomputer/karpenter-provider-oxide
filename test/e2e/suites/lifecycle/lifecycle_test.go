//go:build e2e

package lifecycle

import (
	"testing"

	"github.com/onsi/gomega"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/test/e2e/framework"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const resources = "cpu=1,memory=256Mi"

// TestScaleDownWhenEmpty checks that consolidation removes an empty node and its
// cloud resources while retaining the NodePool and NodeClass.
// A probe pod would prevent the node from being empty.
func TestScaleDownWhenEmpty(t *testing.T) {
	e := framework.Setup(t)
	g := gomega.NewWithT(t)
	ctx := t.Context()

	nc := e.NodeClass("scaledown")
	pool := e.NodePool(nc, func(pool *karpv1.NodePool) {
		pool.Spec.Disruption.ConsolidationPolicy = karpv1.ConsolidationPolicyWhenEmpty
		pool.Spec.Disruption.ConsolidateAfter = karpv1.MustParseNillableDuration("30s")
	})
	pod := e.Pod(pool, resources)
	e.Create(t, nc, pool, pod)
	node := e.EventuallyNodeReady(ctx, g, pool)
	claim, err := e.NodeClaim(ctx, pool)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	launch := framework.LaunchOf(claim)

	g.Expect(e.Kube.Delete(ctx, pod)).To(gomega.Succeed())

	e.EventuallyNoNodeClaims(ctx, g, pool)
	e.EventuallyNodeGone(ctx, g, node.Name)
	e.EventuallyLaunchGone(ctx, g, launch)

	var currentPool karpv1.NodePool
	g.Expect(e.Kube.Get(ctx, client.ObjectKeyFromObject(pool), &currentPool)).To(gomega.Succeed())
	g.Expect(currentPool.DeletionTimestamp).To(gomega.BeNil())
	var currentClass v1alpha1.OxideNodeClass
	g.Expect(e.Kube.Get(ctx, client.ObjectKeyFromObject(nc), &currentClass)).To(gomega.Succeed())
	g.Expect(currentClass.DeletionTimestamp).To(gomega.BeNil())
}

// TestNodeClaimDeletionReplacesNode checks that deletion produces exactly one
// replacement and cleans up the old instance and disks.
func TestNodeClaimDeletionReplacesNode(t *testing.T) {
	e := framework.Setup(t)
	g := gomega.NewWithT(t)
	ctx := t.Context()

	nc := e.NodeClass("replace")
	pool := e.NodePool(nc)
	deployment := e.Deployment(pool, 1, resources)
	e.Create(t, nc, pool, deployment)
	old := e.EventuallyNodeReady(ctx, g, pool)
	oldClaim, err := e.NodeClaim(ctx, pool)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	launch := framework.LaunchOf(oldClaim)

	g.Expect(e.Kube.Delete(ctx, oldClaim)).To(gomega.Succeed())

	node := e.EventuallyNodeReady(ctx, g, pool)
	g.Expect(node.Name).NotTo(gomega.Equal(old.Name))
	claim, err := e.NodeClaim(ctx, pool)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(claim.Name).NotTo(gomega.Equal(oldClaim.Name))
	replacement := framework.LaunchOf(claim)
	g.Expect(replacement.InstanceID).NotTo(gomega.Equal(launch.InstanceID))
	g.Expect(replacement.DiskPrefix).NotTo(gomega.Equal(launch.DiskPrefix))

	e.EventuallyNodeGone(ctx, g, old.Name)
	e.EventuallyLaunchGone(ctx, g, launch)
	claims, err := e.NodeClaims(ctx, pool)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(claims).To(gomega.HaveLen(1))
}

// TestInstanceDeletedOutOfBand checks that deleting an instance through the Oxide
// API triggers replacement and cleanup of its detached boot and data disks.
func TestInstanceDeletedOutOfBand(t *testing.T) {
	e := framework.Setup(t)
	g := gomega.NewWithT(t)
	ctx := t.Context()

	nc := e.NodeClass("outofband", func(nc *v1alpha1.OxideNodeClass) {
		nc.Spec.Storage.DiskSets = []v1alpha1.DiskSet{{
			Name: "data", Type: v1alpha1.DiskSetTypeDistributed,
			Size: resource.MustParse("10Gi"), MountPath: "/mnt/data",
		}}
	})
	pool := e.NodePool(nc)
	deployment := e.Deployment(pool, 1, resources)
	e.Create(t, nc, pool, deployment)
	old := e.EventuallyNodeReady(ctx, g, pool)
	oldClaim, err := e.NodeClaim(ctx, pool)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	launch := framework.LaunchOf(oldClaim)

	disks, err := e.Host(t, old).Disks(ctx)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(disks).To(gomega.HaveLen(2), "disks attached to instance %s", launch.InstanceID)
	launch.DiskIDs = nil
	for _, disk := range disks {
		launch.DiskIDs = append(launch.DiskIDs, disk.Id)
	}

	g.Expect(e.DeleteInstance(ctx, launch.InstanceID)).To(gomega.Succeed())

	e.EventuallyNodeClaimDeleting(ctx, g, oldClaim.Name)
	node := e.EventuallyNodeReady(ctx, g, pool)
	g.Expect(node.Name).NotTo(gomega.Equal(old.Name))

	e.EventuallyNodeGone(ctx, g, old.Name)
	e.EventuallyLaunchGone(ctx, g, launch)
}
