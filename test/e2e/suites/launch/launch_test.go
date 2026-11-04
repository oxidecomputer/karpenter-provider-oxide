//go:build e2e

package launch

import (
	"testing"

	"github.com/onsi/gomega"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/test/e2e/framework"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// TestLaunch boots a node from a boot-disk-only nodeclass and checks what the APIs
// report about it, without touching the host.
func TestLaunch(t *testing.T) {
	e := framework.Setup(t)
	g := gomega.NewWithT(t)
	ctx := t.Context()

	nc := e.NodeClass("launch")
	pool := e.NodePool(nc)
	pod := e.Pod(pool, "cpu=1,memory=256Mi")
	e.Create(t, nc, pool, pod)
	node := e.EventuallyNodeReady(g, pool)

	g.Expect(node.Labels).To(gomega.HaveKeyWithValue(karpv1.NodePoolLabelKey, pool.Name))
	g.Expect(node.Labels).To(gomega.HaveKeyWithValue(v1alpha1.LabelNodeClass, nc.Name))

	claim, err := e.NodeClaim(ctx, pool)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(claim).NotTo(gomega.BeNil())
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourcePods} {
		want, got := claim.Status.Allocatable[name], node.Status.Allocatable[name]
		g.Expect(got.Cmp(want)).To(gomega.BeZero(),
			"allocatable %s: node %s, nodeclaim %s", name, got.String(), want.String())
	}
	// Memory allocatable is modeled from a survey of the guest; the node must report at
	// least what the scheduler was told it would have.
	wantMem, gotMem := claim.Status.Allocatable[corev1.ResourceMemory],
		node.Status.Allocatable[corev1.ResourceMemory]
	g.Expect(gotMem.Cmp(wantMem)).To(gomega.BeNumerically(">=", 0),
		"allocatable memory: node %s, nodeclaim %s", gotMem.String(), wantMem.String())

	h := e.Host(t, node)
	disks, err := h.Disks(ctx)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(disks).To(gomega.HaveLen(1))
	g.Expect(int64(disks[0].Size)).To(gomega.Equal(nc.Spec.Storage.Boot.Size.Value()),
		"boot disk %s", disks[0].Name)
	g.Expect(string(disks[0].Name)).To(gomega.HavePrefix(h.DiskPrefix()))
}
