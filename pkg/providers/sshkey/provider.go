// Package sshkey resolves SSH key selectors and drift.
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

// NewProvider returns a provider that re-resolves a nodeclass's SSH keys once its reading is older than ttl.
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

		Resolved: slices.Clone(entry.Value),
		Age:      entry.Age(time.Now()),
		Stale:    entry.Stale,
	}, nil
}

func (p *Provider) resolve(
	ctx context.Context,
	sshKeySelectors []v1alpha1.SSHKeySelector,
) ([]v1alpha1.OxideSSHKey, error) {
	var sshKeys []v1alpha1.OxideSSHKey
	var listed []oxide.SshKey
	for _, sshKeySelector := range sshKeySelectors {
		if sshKeySelector.Name.IsPattern() {
			if listed == nil {
				var err error
				listed, err = p.ox.CurrentUserSshKeyListAllPages(
					ctx,
					oxide.CurrentUserSshKeyListParams{},
				)
				if err != nil {
					return nil, fmt.Errorf("unable to list ssh keys: %w", err)
				}
			}
			for _, sshKey := range listed {
				if sshKeySelector.Name.Matches(string(sshKey.Name)) {
					sshKeys = append(sshKeys, v1alpha1.OxideSSHKey{
						ID:   sshKey.Id,
						Name: string(sshKey.Name),
					})
				}
			}
			continue
		}
		nameOrID := lo.CoalesceOrEmpty(string(sshKeySelector.ID), string(sshKeySelector.Name))
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
	return lo.UniqBy(sshKeys, func(sshKey v1alpha1.OxideSSHKey) string { return sshKey.ID }), nil
}

// DriftReason compares the keys copied into the instance at launch against the keys the nodeclass selects now.
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
