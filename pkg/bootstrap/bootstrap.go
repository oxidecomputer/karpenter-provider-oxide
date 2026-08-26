// Package bootstrap configures how nodes join the cluster.
package bootstrap

import (
	"context"
	"fmt"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	corev1 "k8s.io/api/core/v1"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Cluster is the provider-agnostic view of the control plane that the nodeclass controller discovers once.
type Cluster struct {
	// Host is the kube-apiserver address.
	Host string
	// Port is the kube-apiserver port from the same source.
	Port int32
	// ServerOverride is the verbatim spec.server value when the user set one.
	ServerOverride string
}

// Request is everything a Bootstrapper needs to render user-data for one specific node.
type Request struct {
	// NodeClaim is the claim being fulfilled.
	NodeClaim *karpcorev1.NodeClaim
	// NodeTaints are the taints the node must register with, including Karpenter's unregistered taint.
	NodeTaints []corev1.Taint
	// Hostname is the hostname the instance will be created with.
	Hostname string
	// Storage is the resolved disk layout for this node: the Oxide disk names the launch is creating.
	Storage *storage.Plan
}

// Bootstrapper joins nodes to the cluster for a single bootstrap provider.
type Bootstrapper interface {
	// Name is the bootstrap kind this implementation handles, matching a v1alpha1.BootstrapKind*.
	Name() string
	// Resolve fills in the provider-specific parts of nodeClass.Status and sets the conditions the provider owns.
	Resolve(ctx context.Context, nodeClass *v1alpha1.OxideNodeClass, cluster Cluster) error
	// UserData renders the user-data used to join a single node to the cluster.
	UserData(ctx context.Context, nodeClass *v1alpha1.OxideNodeClass, req Request) (string, error)
}

// Bootstrappers is the set of available implementations keyed by kind.
type Bootstrappers map[string]Bootstrapper

// New builds a registry from the given implementations.
func New(bootstrappers ...Bootstrapper) Bootstrappers {
	registry := Bootstrappers{}
	for _, b := range bootstrappers {
		registry[b.Name()] = b
	}
	return registry
}

// For returns the Bootstrapper matching the nodeclass's configured provider.
func (b Bootstrappers) For(nodeClass *v1alpha1.OxideNodeClass) (Bootstrapper, error) {
	kind := nodeClass.Spec.Bootstrap.Kind()
	if kind == "" {
		return nil, fmt.Errorf(
			"no bootstrap provider configured on OxideNodeClass %q",
			nodeClass.Name,
		)
	}
	bootstrapper, ok := b[kind]
	if !ok {
		return nil, fmt.Errorf(
			"unsupported bootstrap provider %q on OxideNodeClass %q",
			kind,
			nodeClass.Name,
		)
	}
	return bootstrapper, nil
}
