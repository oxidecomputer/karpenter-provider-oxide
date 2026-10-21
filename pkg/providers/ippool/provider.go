// Package ippool resolves ephemeral IP pools and drift.
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
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Result is one resolution of a nodeclass's ephemeral IP entries.
type Result = resolver.Resolution[v1alpha1.OxideEphemeralIP, v1alpha1.EphemeralIP]

type reading struct {
	resolved []v1alpha1.OxideEphemeralIP

	unresolved []v1alpha1.EphemeralIP
	reason     string
	message    string
}

// Provider is everything that talks to the Oxide API about IP pools.
type Provider struct {
	ox    *oxide.Client
	cache *resolver.Cache[reading]
}

// NewProvider returns a provider that re-resolves a nodeclass's ephemeral IPs once its reading is older than ttl.
func NewProvider(ox *oxide.Client, ttl time.Duration) *Provider {
	return &Provider{ox: ox, cache: resolver.NewCache[reading]("ippool", ttl)}
}

// Resolve returns the pool each of the nodeclass's ephemeral IP entries allocates from.
func (p *Provider) Resolve(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (Result, error) {
	ephemeralIPs := nodeClass.Spec.EphemeralIPs
	if len(ephemeralIPs) == 0 {
		return Result{}, nil
	}
	entry, err := p.cache.Get(
		ctx,
		resolver.Fingerprint(ephemeralIPs),
		func(ctx context.Context) (reading, error) {
			return p.resolve(ctx, ephemeralIPs)
		},
	)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Age:   entry.Age(time.Now()),
		Stale: entry.Stale,
	}
	if entry.Value.reason != "" {
		result.Unmatched = entry.Value.unresolved
		result.Reason = entry.Value.reason
		result.Message = entry.Value.message
		return result, nil
	}

	result.Resolved = slices.Clone(entry.Value.resolved)
	return result, nil
}

func (p *Provider) resolve(
	ctx context.Context,
	ephemeralIPs []v1alpha1.EphemeralIP,
) (reading, error) {
	resolved := make([]v1alpha1.OxideEphemeralIP, 0, len(ephemeralIPs))
	for _, ephemeralIP := range ephemeralIPs {
		if ephemeralIP.Pool == nil {
			resolved = append(
				resolved,
				v1alpha1.OxideEphemeralIP{IPVersion: ephemeralIP.IPVersion},
			)
			continue
		}

		nameOrID := lo.CoalesceOrEmpty(string(ephemeralIP.Pool.ID), string(ephemeralIP.Pool.Name))
		pools, failed, err := p.lookUp(ctx, ephemeralIP, nameOrID)
		if err != nil {
			return reading{}, err
		}
		if failed != nil {
			return *failed, nil
		}

		entry := v1alpha1.OxideEphemeralIP{
			IPVersion: string(pools[0].IpVersion),
			PoolID:    pools[0].Id,
			PoolName:  string(pools[0].Name),
		}
		for _, pool := range pools[1:] {
			entry.Fallbacks = append(
				entry.Fallbacks,
				v1alpha1.OxideIPPool{ID: pool.Id, Name: string(pool.Name)},
			)
		}
		resolved = append(resolved, entry)
	}

	for i, ephemeralIP := range resolved {
		if !slices.ContainsFunc(resolved[i+1:], func(other v1alpha1.OxideEphemeralIP) bool {
			return other.IPVersion == ephemeralIP.IPVersion
		}) {
			continue
		}
		return reading{
			unresolved: ephemeralIPs,
			reason:     v1alpha1.ConditionReasonDuplicateIPVersion,
			message: fmt.Sprintf(
				"an instance can have at most one %s ephemeral address, but two entries resolve to %s",
				ephemeralIP.IPVersion,
				ephemeralIP.IPVersion,
			),
		}, nil
	}

	return reading{resolved: resolved}, nil
}

// lookUp finds the pools an entry allocates from, oldest first, or the reading to publish when it cannot.
func (p *Provider) lookUp(
	ctx context.Context,
	ephemeralIP v1alpha1.EphemeralIP,
	nameOrID string,
) ([]oxide.SiloIpPool, *reading, error) {
	var matched []oxide.SiloIpPool
	if ephemeralIP.Pool.Name.IsPattern() {
		pools, err := p.ox.IpPoolListAllPages(ctx, oxide.IpPoolListParams{})
		if err != nil {
			return nil, nil, fmt.Errorf("unable to list IP pools: %w", err)
		}
		matched = lo.Filter(pools, func(pool oxide.SiloIpPool, _ int) bool {
			return ephemeralIP.Pool.Name.Matches(string(pool.Name))
		})
		if len(matched) == 0 {
			return nil, &reading{
				unresolved: []v1alpha1.EphemeralIP{ephemeralIP},
				reason:     v1alpha1.ConditionReasonIPPoolNotFound,
				message:    fmt.Sprintf("no IP pool matched %q", nameOrID),
			}, nil
		}
		slices.SortStableFunc(matched, byAge)
	} else {
		pool, err := p.ox.IpPoolView(ctx, oxide.IpPoolViewParams{Pool: oxide.NameOrId(nameOrID)})
		if err != nil {
			return nil, &reading{
				unresolved: []v1alpha1.EphemeralIP{ephemeralIP},
				reason:     v1alpha1.ConditionReasonIPPoolNotFound,
				message:    fmt.Sprintf("IP pool %q could not be read: %v", nameOrID, err),
			}, nil
		}
		matched = []oxide.SiloIpPool{*pool}
	}

	// Fallbacks must keep the entry in one family; without a version asked for, the oldest match sets it.
	version := lo.CoalesceOrEmpty(oxide.IpVersion(ephemeralIP.IPVersion), matched[0].IpVersion)
	family := lo.Filter(matched, func(pool oxide.SiloIpPool, _ int) bool {
		return pool.IpVersion == version
	})
	if len(family) == 0 {
		return nil, &reading{
			unresolved: []v1alpha1.EphemeralIP{ephemeralIP},
			reason:     v1alpha1.ConditionReasonIPVersionMismatch,
			message: fmt.Sprintf(
				"IP pool %q allocates %s addresses but the entry asks for %s",
				strings.Join(lo.Map(matched, func(pool oxide.SiloIpPool, _ int) string {
					return string(pool.Name)
				}), ", "),
				matched[0].IpVersion,
				ephemeralIP.IPVersion,
			),
		}, nil
	}
	return family, nil, nil
}

func byAge(a, b oxide.SiloIpPool) int {
	switch {
	case a.TimeCreated == nil || b.TimeCreated == nil:
		return strings.Compare(string(a.Name), string(b.Name))
	case a.TimeCreated.Equal(*b.TimeCreated):
		return strings.Compare(string(a.Name), string(b.Name))
	default:
		return a.TimeCreated.Compare(*b.TimeCreated)
	}
}

// DriftReason compares the addresses the instance was launched with against the entries the nodeclass declares now.
func (p *Provider) DriftReason(
	nodeClaim *karpv1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) karpcloudprovider.DriftReason {
	annotation, ok := nodeClaim.Annotations[v1alpha1.AnnotationEphemeralIPs]
	if !ok {
		return ""
	}
	recorded := v1alpha1.ParseEphemeralIPs(annotation)
	desired := nodeClass.Status.EphemeralIPs
	if len(recorded) != len(desired) {
		return providers.DriftReasonEphemeralIP
	}
	for _, ephemeralIP := range desired {
		record, held := recorded[ephemeralIP.IPVersion]
		if !held {
			return providers.DriftReasonEphemeralIP
		}
		if ephemeralIP.PoolID != "" && !allocatesFrom(ephemeralIP, record.PoolID) {
			return providers.DriftReasonEphemeralIP
		}
	}
	return ""
}

func allocatesFrom(ephemeralIP v1alpha1.OxideEphemeralIP, poolID string) bool {
	return poolID == ephemeralIP.PoolID ||
		slices.ContainsFunc(ephemeralIP.Fallbacks, func(pool v1alpha1.OxideIPPool) bool {
			return pool.ID == poolID
		})
}
