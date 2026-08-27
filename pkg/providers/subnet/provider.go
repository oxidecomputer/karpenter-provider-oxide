// Package subnet resolves subnet selectors and drift.
package subnet

import (
	"context"
	"errors"
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

// DescribeSelectors renders subnet selectors as a concise condition message.
func DescribeSelectors(selectors []v1alpha1.SubnetSelector) string {
	described := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		parts := make([]string, 0, 4)
		if selector.ID != "" {
			parts = append(parts, "id="+selector.ID)
		}
		if selector.Name != "" {
			parts = append(parts, "name="+selector.Name)
		}
		if selector.VPCID != "" {
			parts = append(parts, "vpcID="+selector.VPCID)
		}
		if selector.VPCName != "" {
			parts = append(parts, "vpcName="+selector.VPCName)
		}
		described = append(described, "{"+strings.Join(parts, ", ")+"}")
	}
	return strings.Join(described, ", ")
}

// Result is one resolution of a nodeclass's subnet selectors.
type Result = resolver.Resolution[v1alpha1.OxideSubnet, v1alpha1.SubnetSelector]

// Provider is everything that talks to the Oxide API about subnets.
type Provider struct {
	ox      *oxide.Client
	cache   *resolver.Cache[[]v1alpha1.OxideSubnet]
	vpcName *resolver.Cache[string]
}

// NewProvider returns a provider that re-resolves a nodeclass's subnets once its reading is older than ttl.
func NewProvider(ox *oxide.Client, ttl time.Duration) *Provider {
	return &Provider{
		ox:      ox,
		cache:   resolver.NewCache[[]v1alpha1.OxideSubnet]("subnet", ttl),
		vpcName: newVPCNameCache(ttl),
	}
}

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
		selected, err := p.resolveSelector(ctx, subnetSelector)
		if err != nil {
			return nil, err
		}
		for _, subnet := range selected {
			if subnetSelector.VPCName != "" {
				vpcIDToName[subnet.VpcId] = subnetSelector.VPCName
			}
			resolvedSubnets = append(resolvedSubnets, subnet)
		}
	}

	var oxideSubnets []v1alpha1.OxideSubnet
	for _, subnet := range lo.UniqBy(resolvedSubnets, func(subnet oxide.VpcSubnet) string {
		return subnet.Id
	}) {
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

func (p *Provider) resolveSelector(
	ctx context.Context,
	selector v1alpha1.SubnetSelector,
) ([]oxide.VpcSubnet, error) {
	nameOrID := ""
	project := selector.Project
	vpcNameOrID := ""

	if selector.VPCID != "" {
		vpcNameOrID = selector.VPCID
		project = ""
	} else if selector.VPCName != "" {
		vpcNameOrID = selector.VPCName
	}
	if selector.ID != "" {
		nameOrID = selector.ID
		project = ""
	} else if selector.Name != "" {
		nameOrID = selector.Name
	}

	if nameOrID != "" {
		subnet, err := p.ox.VpcSubnetView(ctx, oxide.VpcSubnetViewParams{
			Subnet: oxide.NameOrId(nameOrID), Vpc: oxide.NameOrId(vpcNameOrID), Project: oxide.NameOrId(project),
		})
		if err != nil {
			if errors.Is(err, oxide.ErrObjectNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf(
				"subnet %q from VPC %q and project %q: %w",
				nameOrID, vpcNameOrID, project, err,
			)
		}
		return []oxide.VpcSubnet{*subnet}, nil
	}
	if vpcNameOrID == "" {
		return nil, nil
	}
	subnets, err := p.ox.VpcSubnetListAllPages(ctx, oxide.VpcSubnetListParams{
		Vpc: oxide.NameOrId(vpcNameOrID), Project: oxide.NameOrId(project),
	})
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"subnets from VPC %q and project %q: %w", vpcNameOrID, project, err,
		)
	}
	return subnets, nil
}

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

// DriftReason asks whether the subnet the instance's interface was created in is still one the nodeclass selects.
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
