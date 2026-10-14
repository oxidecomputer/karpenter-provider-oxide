// Package bootstrap configures how nodes join the cluster.
package bootstrap

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

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
	// Kubelet is the nodeclass's own reservations, layered over the shape's.
	Kubelet instancetype.Reserved
}

// KubeletArgs returns the kubelet arguments for a node of this shape: the
// reservations the instance type provider modeled, then the nodeclass's own
// arguments. A flag set twice takes its last value.
//
// Without this the provider models reservations kubelet never applies, and the
// scheduler's idea of allocatable drifts from the node's.
func KubeletArgs(specs instancetype.Specs, kubelet instancetype.Reserved, extra []string) []string {
	reserved := kubelet
	if specs.Ncpus > 0 && specs.MemInGiB > 0 {
		reserved = instancetype.ReservedFor(specs, kubelet)
	}
	return MergeKubeletArgs(reserved.KubeletArgs(), extra)
}

// MergeKubeletArgs flattens "key=value" argument sets into one, keeping each
// flag at the position it first appeared with the value it was last given.
func MergeKubeletArgs(sets ...[]string) []string {
	at := map[string]int{}
	var out []string
	for _, set := range sets {
		for _, arg := range set {
			name, _, _ := strings.Cut(arg, "=")
			if i, ok := at[name]; ok {
				out[i] = arg
				continue
			}
			at[name] = len(out)
			out = append(out, arg)
		}
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
	// Detect reports whether the cluster the controller runs in was bootstrapped by this
	// provider, which is what lets a nodeclass leave spec.bootstrap unset.
	Detect(ctx context.Context) (bool, error)
}

// Bootstrappers is the set of available implementations, in registration order.
type Bootstrappers struct {
	byKind map[string]Bootstrapper
	all    []Bootstrapper

	mu       sync.Mutex
	detected string
}

// New builds a registry from the given implementations.
func New(bootstrappers ...Bootstrapper) *Bootstrappers {
	registry := &Bootstrappers{byKind: map[string]Bootstrapper{}, all: bootstrappers}
	for _, b := range bootstrappers {
		registry.byKind[b.Name()] = b
	}
	return registry
}

// For returns the Bootstrapper for the nodeclass: the provider it configured, the
// one already detected for it, or, failing both, whichever provider recognizes
// the cluster this controller runs in.
func (b *Bootstrappers) For(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (Bootstrapper, error) {
	kind := nodeClass.BootstrapKind()
	if kind == "" {
		var err error
		if kind, err = b.detect(ctx); err != nil {
			return nil, err
		}
	}
	bootstrapper, ok := b.byKind[kind]
	if !ok {
		return nil, fmt.Errorf(
			"unsupported bootstrap provider %q on OxideNodeClass %q",
			kind,
			nodeClass.Name,
		)
	}
	return bootstrapper, nil
}

// detect asks every provider whether it built this cluster. The answer cannot
// change under a running controller, so the first success is cached.
func (b *Bootstrappers) detect(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.detected != "" {
		return b.detected, nil
	}

	var matched []string
	for _, bootstrapper := range b.all {
		ok, err := bootstrapper.Detect(ctx)
		if err != nil {
			return "", fmt.Errorf(
				"unable to determine whether this is a %s cluster: %w",
				bootstrapper.Name(),
				err,
			)
		}
		if ok {
			matched = append(matched, bootstrapper.Name())
		}
	}
	sort.Strings(matched)

	switch len(matched) {
	case 1:
		b.detected = matched[0]
		return b.detected, nil
	case 0:
		return "", fmt.Errorf(
			"unable to detect the cluster's bootstrap provider, "+
				"set spec.bootstrap on the OxideNodeClass to name one of %s",
			strings.Join(b.kinds(), ", "),
		)
	default:
		return "", fmt.Errorf(
			"the cluster looks like it was bootstrapped by more than one provider (%s), "+
				"set spec.bootstrap on the OxideNodeClass to name one",
			strings.Join(matched, ", "),
		)
	}
}

func (b *Bootstrappers) kinds() []string {
	out := make([]string, 0, len(b.all))
	for _, bootstrapper := range b.all {
		out = append(out, bootstrapper.Name())
	}
	sort.Strings(out)
	return out
}
