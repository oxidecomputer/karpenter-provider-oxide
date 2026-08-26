// Package ippool resolves ephemeral IP pools and drift.
package ippool

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

		nameOrID := lo.Ternary(
			ephemeralIP.Pool.ID != "",
			ephemeralIP.Pool.ID,
			ephemeralIP.Pool.Name,
		)
		pool, err := p.ox.IpPoolView(ctx, oxide.IpPoolViewParams{Pool: oxide.NameOrId(nameOrID)})
		if err != nil {

			return reading{
				unresolved: []v1alpha1.EphemeralIP{ephemeralIP},
				reason:     v1alpha1.ConditionReasonIPPoolNotFound,
				message:    fmt.Sprintf("IP pool %q could not be read: %v", nameOrID, err),
			}, nil
		}

		if ephemeralIP.IPVersion != "" && ephemeralIP.IPVersion != string(pool.IpVersion) {
			return reading{
				unresolved: []v1alpha1.EphemeralIP{ephemeralIP},
				reason:     v1alpha1.ConditionReasonIPVersionMismatch,
				message: fmt.Sprintf(
					"IP pool %q allocates %s addresses but the entry asks for %s",
					nameOrID,
					pool.IpVersion,
					ephemeralIP.IPVersion,
				),
			}, nil
		}
		resolved = append(resolved, v1alpha1.OxideEphemeralIP{
			IPVersion: string(pool.IpVersion),
			PoolID:    pool.Id,
			PoolName:  string(pool.Name),
		})
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
		if ephemeralIP.PoolID != "" && record.PoolID != ephemeralIP.PoolID {
			return providers.DriftReasonEphemeralIP
		}
	}
	return ""
}
