// Package bootstrap defines the contract between the provider-agnostic Karpenter machinery and the
// mechanism a launched node uses to join the cluster. The nodeclass controller and the cloud
// provider only ever talk to a Bootstrapper; everything RKE2- or kubeadm-specific lives behind it
// in pkg/rke2 and pkg/kubeadm respectively.
package bootstrap

import (
	"context"
	"fmt"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	corev1 "k8s.io/api/core/v1"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Cluster is the provider-agnostic view of the control plane that the nodeclass controller
// discovers once and hands to whichever Bootstrapper is configured. Providers join on different
// ports, so the host and port are kept separate rather than pre-formatted into a URL.
type Cluster struct {
	// Host is the address of the kube-apiserver, either user-supplied or discovered from the
	// default/kubernetes EndpointSlice.
	Host string
	// Port is the kube-apiserver port from the same source. Providers that join on their own port,
	// such as RKE2's supervisor, ignore it.
	Port int32
	// ServerOverride is the verbatim spec.server value when the user set one. A provider that can
	// use it as-is should prefer it over rendering Host and Port itself.
	ServerOverride string
}

// Request is everything a Bootstrapper needs to render user-data for one specific node.
type Request struct {
	// NodeClaim is the claim being fulfilled.
	NodeClaim *karpcorev1.NodeClaim
	// NodeTaints are the taints the node must register with, including Karpenter's unregistered
	// taint. They are resolved per-NodeClaim rather than per-nodeclass.
	NodeTaints []corev1.Taint
	// Hostname is the hostname the instance will be created with.
	Hostname string
	// Storage is the resolved disk layout for this node: the Oxide disk names the launch is creating,
	// the NVMe serials they will present in the guest, and where each one is to end up. Nil when the
	// nodeclass configures no diskSets.
	//
	// Carried per-Request rather than resolved onto the nodeclass status because disk names are
	// minted per-NodeClaim: they embed this instance's fingerprint and ordinals, so no two nodes of a
	// nodeclass share them and there is nothing about them a reconcile of the nodeclass could
	// resolve.
	Storage *storage.Plan
}

// Bootstrapper joins nodes to the cluster for a single bootstrap provider.
//
// Resolve runs in the nodeclass reconcile loop and is responsible for provider-specific status and
// conditions. UserData runs per-NodeClaim at instance creation time and may have side effects, such
// as minting a short-lived credential, so it must not be called speculatively.
type Bootstrapper interface {
	// Name is the bootstrap kind this implementation handles, matching a v1alpha1.BootstrapKind*.
	Name() string
	// Resolve fills in the provider-specific parts of nodeClass.Status and sets the conditions the
	// provider owns. It returns an error only for failures that should requeue the nodeclass;
	// user-facing misconfiguration is reported through conditions instead.
	Resolve(ctx context.Context, nodeClass *v1alpha1.OxideNodeClass, cluster Cluster) error
	// UserData renders the user-data used to join a single node to the cluster.
	UserData(ctx context.Context, nodeClass *v1alpha1.OxideNodeClass, req Request) (string, error)
}

// Bootstrappers is the set of available implementations keyed by kind. It is assembled by the
// controller entrypoint so that pkg/bootstrap stays free of imports on its implementations.
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
