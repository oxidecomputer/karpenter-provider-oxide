package framework

import (
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/test/e2e/framework/host"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Host returns the typed view of a node, with its nodeclaim's instance and disk prefix.
func (e *Env) Host(t *testing.T, node *corev1.Node) *host.Host {
	t.Helper()
	var claims karpv1.NodeClaimList
	if err := e.Kube.List(t.Context(), &claims); err != nil {
		t.Fatalf("listing nodeclaims: %v", err)
	}
	var claim *karpv1.NodeClaim
	for i := range claims.Items {
		if claims.Items[i].Status.NodeName == node.Name {
			claim = &claims.Items[i]
		}
	}
	if claim == nil {
		t.Fatalf("no nodeclaim for node %s", node.Name)
	}
	return host.Node(t, e.hostEnv(t), node,
		instanceID(claim.Status.ProviderID),
		claim.Annotations[v1alpha1.AnnotationDiskPrefix],
	)
}

func (e *Env) hostEnv(t *testing.T) host.Env {
	return host.Env{
		Config:    e.Config,
		Clientset: e.Clientset,
		Oxide:     e.Oxide,
		Project:   e.Project,
		Namespace: e.Namespace,
		Labels:    e.Labels(),
		ProbeName: e.Name("probe-" + strings.ToLower(strings.TrimPrefix(t.Name(), "Test"))),
	}
}
