// Package ippool resolves a nodeclass's ephemeral IP entries into the pool each launched instance
// takes an external address from, and decides whether a node whose addresses no longer come from
// those pools has drifted.
//
// An entry is a slot rather than a selector. Oxide allows at most one ephemeral address per IP
// version and refuses a pair that does not span two versions, so the number of addresses an instance
// gets is fixed by the nodeclass rather than by how many pools happen to exist in the silo. That is
// why a pool is named by reference here and never matched by wildcard: a reference resolving to two
// pools would be two addresses the nodeclass never asked for.
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

// reading is what the cache holds: either the resolved entries, or the reason the resolution did not
// complete. The failure is cached alongside the success so that a nodeclass naming a pool that does
// not exist is not re-asked on every reconcile of every nodeclass carrying the same entries.
type reading struct {
	resolved []v1alpha1.OxideEphemeralIP
	// unresolved is the entry that failed, and reason and message the condition to record for it.
	unresolved []v1alpha1.EphemeralIP
	reason     string
	message    string
}

// Provider is everything that talks to the Oxide API about IP pools.
type Provider struct {
	ox    *oxide.Client
	cache *resolver.Cache[reading]
}

// NewProvider returns a provider that re-resolves a nodeclass's ephemeral IPs once its reading is
// older than ttl.
//
// No project is taken: an IP pool is linked to a silo rather than owned by a project, so a pool name
// is unambiguous on its own.
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
	// Cloned so that a caller publishing this into an object it then patches cannot reach into the
	// reading the next caller is served.
	result.Resolved = slices.Clone(entry.Value.resolved)
	return result, nil
}

// resolve walks the entries in order. An entry naming a pool is viewed directly, which is the whole
// of the API traffic here: there is no listing to match against, because there is nothing to match.
// An entry naming no pool makes no call at all -- the silo's default is Nexus's to pick at launch,
// and recording a guess at it here would be a guess a change of default silently invalidates.
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
			// Reported on the nodeclass rather than returned. A pool that is not there is a statement
			// about the silo an operator has to act on, not something a retry fixes, and the API
			// reports it the same way whether the name is wrong or the pool was deleted.
			//
			// This does swallow a genuinely transient failure of this one call into a condition. The
			// cost of the alternative is worse: an unreachable API would take every nodeclass naming a
			// pool un-Ready with an error the cache would then re-ask for on every reconcile.
			return reading{
				unresolved: []v1alpha1.EphemeralIP{ephemeralIP},
				reason:     v1alpha1.ConditionReasonIPPoolNotFound,
				message:    fmt.Sprintf("IP pool %q could not be read: %v", nameOrID, err),
			}, nil
		}
		// The pool is the authority on its own version, so an entry that set ipVersion as well is
		// checked against it rather than trusted. Which of the two the operator meant is not knowable
		// from here, so it is reported rather than resolved in the pool's favour.
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

	// Two entries of one version is the pair Nexus's validate_ephemeral_ip_pair refuses at launch.
	// Checking it here turns a 400 that only appears when a node is needed into a condition on the
	// nodeclass that declared it. It cannot be checked by CEL: an entry naming only a pool has no
	// version until the pool has been read.
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

// DriftReason compares the addresses the instance was launched with against the entries the
// nodeclass declares now. Both directions: an entry added is an address the node does not have and
// cannot be given without a relaunch, and an entry removed is an address it still holds.
//
// The comparison is keyed by IP version, since that is the slot an ephemeral address occupies. That
// is what makes swapping which pool serves v4 and which serves v6 visible, which comparing an
// unordered set of pool IDs cannot see.
//
// An entry the spec left to the silo's default compares on version alone. The record holds the pool
// the instance actually got, and the desired side holds no pool at all, so the only pool comparison
// available would be against whatever the default happens to be right now -- which would replace a
// whole fleet the moment the silo default moved, over an entry that asked for exactly that.
//
// Ephemeral addresses can be attached to and detached from a live instance, so replacing the node is
// this provider's choice rather than something the Oxide API forces.
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
