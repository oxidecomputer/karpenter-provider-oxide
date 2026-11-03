//go:build e2e

package e2e

import (
	"testing"

	"github.com/onsi/gomega"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// TestKubeletConfiguration shows a nodeclass kubelet field is verifiable entirely
// through the API: the live kubelet config and the node's reported capacity.
func TestKubeletConfiguration(t *testing.T) {
	e := setup(t)
	g := gomega.NewWithT(t)
	ctx := t.Context()

	nc := e.NodeClass("kubelet", func(nc *v1alpha1.OxideNodeClass) {
		nc.Spec.Kubelet = &v1alpha1.KubeletConfiguration{
			MaxPods:      ptr.To[int32](50),
			KubeReserved: map[string]v1alpha1.KubeletValue{"memory": "512Mi"},
		}
	})
	pool := e.NodePool(nc)
	// Reserving 512Mi on the smallest 1 GiB shape leaves too little for the node's own
	// daemons, so ask for enough memory to land on a bigger one.
	pod := e.Pod(pool, "cpu=1,memory=1Gi")
	e.Create(t, nc, pool, pod)
	node := e.EventuallyNodeReady(g, pool)

	config, err := e.Host(t, node).KubeletConfig(ctx)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(config.MaxPods).To(gomega.BeEquivalentTo(50))
	g.Expect(config.KubeReserved).To(gomega.HaveKeyWithValue("memory", "512Mi"))

	pods := node.Status.Capacity[corev1.ResourcePods]
	g.Expect(pods.Value()).To(gomega.BeEquivalentTo(50))
}
