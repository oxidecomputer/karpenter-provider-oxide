// Package bootstrap configures how nodes join the cluster.
package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
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
	// InstanceType is the shape being launched, which fixes the node's kubelet reservations.
	InstanceType instancetype.Specs
}

// KubeletArgs returns the kubelet arguments for a node of this shape: the
// reservations the instance type provider modeled, then the nodeclass's own
// arguments. A flag set twice takes its last value, so operators can override.
//
// Without this the provider models reservations kubelet never applies, and the
// scheduler's idea of allocatable drifts from the node's.
func KubeletArgs(specs instancetype.Specs, extra []string) []string {
	var args []string
	if specs.Ncpus > 0 && specs.MemInGiB > 0 {
		args = instancetype.ReservedFor(specs).KubeletArgs()
	}
	args = append(args, extra...)

	at := map[string]int{}
	out := make([]string, 0, len(args))
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		if i, ok := at[name]; ok {
			out[i] = arg
			continue
		}
		at[name] = len(out)
		out = append(out, arg)
	}
	return out
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
