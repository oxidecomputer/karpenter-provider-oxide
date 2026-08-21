// Package subnet resolves a nodeclass's subnet selectors into the subnets its instances' interfaces
// are created in, and decides whether a node sitting in one it no longer selects has drifted.
//
// Two things are cached here rather than one. The resolution itself is keyed by the selectors, as
// everywhere else; the VPC name a resolved subnet belongs to is cached separately, keyed by VPC ID,
// because a subnet only carries its VPC's ID and InstanceCreate's network interface takes the VPC's
// name. Without that second cache, recovering the name costs a VpcView per resolved subnet on every
// single reconcile -- the largest avoidable source of API calls this package had.
package subnet

import (
	"context"
	"errors"
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

// Result is one resolution of a nodeclass's subnet selectors.
type Result = resolver.Resolution[v1alpha1.OxideSubnet, v1alpha1.SubnetSelector]

// Provider is everything that talks to the Oxide API about subnets.
type Provider struct {
	ox      *oxide.Client
	cache   *resolver.Cache[[]v1alpha1.OxideSubnet]
	vpcName *resolver.Cache[string]
}

// NewProvider returns a provider that re-resolves a nodeclass's subnets once its reading is older
// than ttl.
//
// The VPC name lookup is given the same ttl rather than a longer one. It is load-bearing at launch --
// an interface is created against a VPC by name -- so a name that has been renamed out from under a
// nodeclass must not outlive the subnet reading that referred to it.
//
// No project is taken: a subnet selector carries its own, and the API rejects a project passed
// alongside an ID, so there is nothing left for a provider-wide project to scope.
func NewProvider(ox *oxide.Client, ttl time.Duration) *Provider {
	return &Provider{
		ox:      ox,
		cache:   resolver.NewCache[[]v1alpha1.OxideSubnet]("subnet", ttl),
		vpcName: newVPCNameCache(ttl),
	}
}

// newVPCNameCache is the VPC name cache, kept separate from the constructor so that a test can give
// the two halves different freshness and so say which of them a call it counted came from.
func newVPCNameCache(ttl time.Duration) *resolver.Cache[string] {
	return resolver.NewCache[string]("subnet-vpc-name", ttl)
}

// Resolve returns the subnets the nodeclass's selectors match.
func (p *Provider) Resolve(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (Result, error) {
	selectors := nodeClass.Spec.SubnetSelector
	entry, err := p.cache.Get(
		ctx,
		resolver.Fingerprint(selectors),
		func(ctx context.Context) ([]v1alpha1.OxideSubnet, error) {
			return p.resolve(ctx, selectors)
		},
	)
	if err != nil {
		return Result{}, err
	}
	return Result{
		// Cloned for the same reason the other providers clone: the caller publishes this into an
		// object it then patches, and the cache goes on serving the reading.
		Resolved: slices.Clone(entry.Value),
		Age:      entry.Age(time.Now()),
		Stale:    entry.Stale,
	}, nil
}

func (p *Provider) resolve(
	ctx context.Context,
	subnetSelectors []v1alpha1.SubnetSelector,
) ([]v1alpha1.OxideSubnet, error) {
	var resolvedSubnets []oxide.VpcSubnet
	vpcIDToName := map[string]string{}
	for _, subnetSelector := range subnetSelectors {
		nameOrID := ""
		project := subnetSelector.Project
		vpcNameOrID := ""

		if subnetSelector.VPCID != "" {
			vpcNameOrID = subnetSelector.VPCID
			// project cannot be used to query if ID is used
			project = ""
		} else if subnetSelector.VPCName != "" {
			vpcNameOrID = subnetSelector.VPCName
		}

		if subnetSelector.ID != "" {
			nameOrID = subnetSelector.ID
			// project cannot be used to query if ID is used
			project = ""
		} else if subnetSelector.Name != "" {
			nameOrID = subnetSelector.Name
		}

		if nameOrID != "" {
			subnet, err := p.ox.VpcSubnetView(ctx, oxide.VpcSubnetViewParams{
				Subnet:  oxide.NameOrId(nameOrID),
				Vpc:     oxide.NameOrId(vpcNameOrID),
				Project: oxide.NameOrId(project),
			})
			if err != nil {
				if errors.Is(err, oxide.ErrObjectNotFound) {
					continue
				}
				return nil, fmt.Errorf(
					"subnet %q from VPC %q and project %q: %w",
					nameOrID,
					vpcNameOrID,
					project,
					err,
				)
			}
			resolvedSubnets = append(resolvedSubnets, *subnet)
		} else if vpcNameOrID != "" { // Only VPC is passed
			subnets, err := p.ox.VpcSubnetListAllPages(ctx, oxide.VpcSubnetListParams{
				Vpc:     oxide.NameOrId(vpcNameOrID),
				Project: oxide.NameOrId(project),
			})
			if err != nil {
				if errors.Is(err, oxide.ErrObjectNotFound) {
					continue
				}
				return nil, fmt.Errorf(
					"subnets from VPC %q and project %q: %w",
					vpcNameOrID,
					project,
					err,
				)
			}
			resolvedSubnets = append(resolvedSubnets, subnets...)
		}

		// Cache the VPC ID to Name mapping if a name is used in the selector since we need the VPC
		// Name in the instance create to create a network interface.
		// InstanceCreate#CreateNetworkInterface does not accept a VPC ID, only a Name.
		if subnetSelector.VPCName != "" {
			vpcIDToName[resolvedSubnets[len(resolvedSubnets)-1].VpcId] = subnetSelector.VPCName
		}

	}

	var oxideSubnets []v1alpha1.OxideSubnet
	for _, subnet := range lo.Uniq(resolvedSubnets) {
		vpcName, ok := vpcIDToName[subnet.VpcId]
		if !ok {
			var err error
			vpcName, err = p.lookUpVPCName(ctx, subnet.VpcId)
			if err != nil {
				return nil, fmt.Errorf("vpc information for subnet %q: %w", subnet.Id, err)
			}
		}
		oxideSubnets = append(oxideSubnets, v1alpha1.OxideSubnet{
			ID:        subnet.Id,
			Name:      string(subnet.Name),
			VPCID:     subnet.VpcId,
			VPCName:   vpcName,
			IPv4Block: string(subnet.Ipv4Block),
			IPv6Block: string(subnet.Ipv6Block),
		})

	}
	return oxideSubnets, nil
}

// lookUpVPCName recovers a VPC's name from its ID, which is all a resolved subnet carries and not
// what a launch needs.
//
// Cached by ID rather than by the selectors, because the answer is a fact about the VPC and not
// about the question: every nodeclass with a subnet in the same VPC wants the same name.
func (p *Provider) lookUpVPCName(ctx context.Context, vpcID string) (string, error) {
	entry, err := p.vpcName.Get(ctx, vpcID, func(ctx context.Context) (string, error) {
		vpc, err := p.ox.VpcView(ctx, oxide.VpcViewParams{Vpc: oxide.NameOrId(vpcID)})
		if err != nil {
			return "", err
		}
		return string(vpc.Name), nil
	})
	return entry.Value, err
}

// DriftReason asks whether the subnet the instance's interface was created in is still one the
// nodeclass selects. Membership, not equality: a launch picks one of the selected subnets, so every
// node is in a different subnet from some of its peers by design, and a subnet added to the set
// leaves every existing node exactly as valid as it was.
//
// An empty resolved set is not drift, for the same reason an empty image list is not: it is a
// resolution that found nothing rather than an instruction to move every node somewhere else.
func (p *Provider) DriftReason(
	nodeClaim *karpv1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) karpcloudprovider.DriftReason {
	recorded, ok := nodeClaim.Annotations[v1alpha1.AnnotationSubnet]
	if !ok || recorded == "" {
		return ""
	}
	desired := v1alpha1.SubnetIDs(nodeClass.Status.Subnets)
	if desired.Len() == 0 {
		return ""
	}
	return lo.Ternary(!desired.Has(recorded), providers.DriftReasonSubnet, "")
}
