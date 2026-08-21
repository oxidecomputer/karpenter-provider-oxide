// Package ippool resolves a nodeclass's IP pool selectors into the pools each launched instance gets
// an ephemeral external address from, and decides whether a node whose addresses no longer come from
// the selected pools has drifted.
package ippool

import (
	"context"
	"fmt"
	"slices"
	"strings"
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

// Result is one resolution of a nodeclass's IP pool selectors.
type Result = resolver.Resolution[v1alpha1.OxideIPPool, v1alpha1.IPPoolSelector]

// Provider is everything that talks to the Oxide API about IP pools.
type Provider struct {
	ox    *oxide.Client
	cache *resolver.Cache[[]v1alpha1.OxideIPPool]
}

// NewProvider returns a provider that re-resolves a nodeclass's IP pools once its reading is older
// than ttl.
//
// No project is taken: an IP pool is linked to a silo rather than owned by a project, so a pool name
// is unambiguous on its own.
func NewProvider(ox *oxide.Client, ttl time.Duration) *Provider {
	return &Provider{ox: ox, cache: resolver.NewCache[[]v1alpha1.OxideIPPool]("ippool", ttl)}
}

// Resolve returns the IP pools the nodeclass's selectors match.
func (p *Provider) Resolve(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (Result, error) {
	selectors := nodeClass.Spec.IPPoolSelector
	entry, err := p.cache.Get(
		ctx,
		resolver.Fingerprint(selectors),
		func(ctx context.Context) ([]v1alpha1.OxideIPPool, error) {
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

// resolve evaluates the selectors. A selector naming a pool is looked up directly; one selecting on
// default-ness or IP version is matched against a single listing, which serves all of them at once.
func (p *Provider) resolve(
	ctx context.Context,
	ipPoolSelectors []v1alpha1.IPPoolSelector,
) ([]v1alpha1.OxideIPPool, error) {
	var listSelectors []v1alpha1.IPPoolSelector
	var resolvedIPPools []oxide.SiloIpPool
	for _, ipPoolSelector := range ipPoolSelectors {
		nameOrID := ""
		if ipPoolSelector.ID != "" {
			nameOrID = ipPoolSelector.ID
		} else if ipPoolSelector.Name != "" {
			nameOrID = ipPoolSelector.Name
		}
		if nameOrID != "" {
			ipPool, err := p.ox.IpPoolView(ctx, oxide.IpPoolViewParams{
				Pool: oxide.NameOrId(nameOrID),
			})
			if err != nil {
				return nil, fmt.Errorf("IP Pool %q: %w", nameOrID, err)
			}
			resolvedIPPools = append(resolvedIPPools, *ipPool)
		} else {
			listSelectors = append(listSelectors, ipPoolSelector)
		}
	}

	var ipPools []oxide.SiloIpPool
	if len(listSelectors) > 0 {
		var err error
		ipPools, err = p.ox.IpPoolListAllPages(ctx, oxide.IpPoolListParams{})
		if err != nil {
			return nil, fmt.Errorf("list error: %w", err)
		}
	}

	for _, ipPoolSelector := range listSelectors {
		for _, ipPool := range ipPools {
			if ipPoolSelector.Default && !*ipPool.IsDefault {
				continue
			}
			if ipPoolSelector.IPVersion != "" &&
				!strings.HasSuffix(ipPoolSelector.IPVersion, string(ipPool.IpVersion)) {
				continue
			}
			resolvedIPPools = append(resolvedIPPools, ipPool)
		}
	}

	resolvedIPPools = lo.UniqBy(
		resolvedIPPools,
		func(ipPool oxide.SiloIpPool) string { return ipPool.Id },
	)
	return lo.Map(resolvedIPPools, func(ipPool oxide.SiloIpPool, _ int) v1alpha1.OxideIPPool {
		return v1alpha1.OxideIPPool{
			ID:        ipPool.Id,
			Name:      string(ipPool.Name),
			IPVersion: string(ipPool.IpVersion),
			Default:   *ipPool.IsDefault,
		}
	}), nil
}

// DriftReason compares the pools the instance's external addresses were allocated from at launch
// against the pools the nodeclass selects now. Both directions again: one ephemeral address is
// attached per selected pool at launch, and nothing attaches or detaches one afterwards.
func (p *Provider) DriftReason(
	nodeClaim *karpv1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) karpcloudprovider.DriftReason {
	recorded, ok := nodeClaim.Annotations[v1alpha1.AnnotationIPPools]
	if !ok {
		return ""
	}
	desired := sets.New(v1alpha1.IPPoolIDs(nodeClass.Status.IPPools)...)
	return lo.Ternary(
		!v1alpha1.ParseIDs(recorded).Equal(desired),
		providers.DriftReasonIPPool,
		"",
	)
}
