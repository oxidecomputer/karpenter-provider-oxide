// Package sshkey resolves a nodeclass's SSH key selectors into the keys copied into every instance
// it launches, and decides whether a node whose authorized keys are no longer the selected set has
// drifted.
//
// The cheapest of the providers -- one view per selector, and a nodeclass rarely names more than a
// couple -- and cached anyway. There is no reason for one provider to be the odd one out, and being
// uniform is what makes the freshness of the whole set one thing to reason about rather than five.
package sshkey

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/resolver"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Result is one resolution of a nodeclass's SSH key selectors.
type Result = resolver.Resolution[v1alpha1.OxideSSHKey, v1alpha1.SSHKeySelector]

// Provider is everything that talks to the Oxide API about SSH keys.
type Provider struct {
	ox    *oxide.Client
	cache *resolver.Cache[[]v1alpha1.OxideSSHKey]
}

// NewProvider returns a provider that re-resolves a nodeclass's SSH keys once its reading is older
// than ttl.
//
// No project is taken: an SSH key belongs to the user the token authenticates as, not to a project,
// so there is nothing to scope by.
func NewProvider(ox *oxide.Client, ttl time.Duration) *Provider {
	return &Provider{ox: ox, cache: resolver.NewCache[[]v1alpha1.OxideSSHKey]("sshkey", ttl)}
}

// Resolve returns the SSH keys the nodeclass's selectors name.
func (p *Provider) Resolve(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (Result, error) {
	selectors := nodeClass.Spec.SSHKeySelector
	entry, err := p.cache.Get(
		ctx,
		resolver.Fingerprint(selectors),
		func(ctx context.Context) ([]v1alpha1.OxideSSHKey, error) {
			return p.resolve(ctx, selectors)
		},
	)
	if err != nil {
		return Result{}, err
	}
	return Result{
		// Cloned so that a caller publishing this into an object it then patches cannot reach into
		// the reading the next caller is served.
		Resolved: slices.Clone(entry.Value),
		Age:      entry.Age(time.Now()),
		Stale:    entry.Stale,
	}, nil
}

// resolve looks each selector up directly. A key selector names one key, by ID or by name, so there
// is nothing here to list and match: a name that does not resolve is an error rather than a miss.
func (p *Provider) resolve(
	ctx context.Context,
	sshKeySelectors []v1alpha1.SSHKeySelector,
) ([]v1alpha1.OxideSSHKey, error) {
	var sshKeys []v1alpha1.OxideSSHKey
	for _, sshKeySelector := range sshKeySelectors {
		nameOrID := lo.Ternary(sshKeySelector.ID != "", sshKeySelector.ID, sshKeySelector.Name)
		sshKey, err := p.ox.CurrentUserSshKeyView(ctx, oxide.CurrentUserSshKeyViewParams{
			SshKey: oxide.NameOrId(nameOrID),
		})
		if err != nil {
			return nil, fmt.Errorf("ssh key %q: %w", nameOrID, err)
		}
		sshKeys = append(sshKeys, v1alpha1.OxideSSHKey{
			ID:   sshKey.Id,
			Name: string(sshKey.Name),
		})
	}
	return sshKeys, nil
}

// DriftReason compares the keys copied into the instance at launch against the keys the nodeclass
// selects now. Both directions count: a key added has to reach the node, and a key removed has to
// stop reaching it, and a running instance offers no way to do either.
func (p *Provider) DriftReason(
	nodeClaim *karpv1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) karpcloudprovider.DriftReason {
	recorded, ok := nodeClaim.Annotations[v1alpha1.AnnotationSSHKeys]
	if !ok {
		return ""
	}
	desired := sets.New(v1alpha1.SSHKeyIDs(nodeClass.Status.SSHKeys)...)
	return lo.Ternary(
		!v1alpha1.ParseIDs(recorded).Equal(desired),
		providers.DriftReasonSSHKey,
		"",
	)
}
