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
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/resolver"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// DescribeSelectors renders subnet selectors back as the terms they were written with.
func DescribeSelectors(selectors []v1alpha1.SubnetSelector) string {
	if len(selectors) == 0 {
		return "no subnet selectors are configured"
	}
	described := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		parts := make([]string, 0, 4)
		if selector.ID != "" {
			parts = append(parts, "id="+string(selector.ID))
		}
		if selector.Name != "" {
			parts = append(parts, "name="+string(selector.Name))
		}
		if selector.VPCID != "" {
			parts = append(parts, "vpcID="+string(selector.VPCID))
		}
		if selector.VPCName != "" {
			parts = append(parts, "vpcName="+string(selector.VPCName))
		}
		if selector.Project != "" {
			parts = append(parts, "project="+selector.Project)
		}
		described = append(described, "{"+strings.Join(parts, ", ")+"}")
	}
	return "no subnets matched " + strings.Join(described, " or ")
}

// Result is one resolution of a nodeclass's subnet selectors.
type Result = resolver.Resolution[v1alpha1.OxideSubnet, v1alpha1.SubnetSelector]

// Provider is everything that talks to the Oxide API about subnets.
type Provider struct {
	ox       *oxide.Client
	projects *project.Resolver
	cache    *resolver.Cache[reading]
	vpcs     *resolver.Cache[vpcInfo]
}

// reading is one resolution as cached: the subnets matched, and the selectors that found their
// subnet in a VPC or project other than the one they meant.
type reading struct {
	subnets   []v1alpha1.OxideSubnet
	misplaced []string
}

type vpcInfo struct {
	name      string
	projectID string
}

// NewProvider returns a provider that re-resolves a nodeclass's subnets once its reading is older than ttl.
func NewProvider(ox *oxide.Client, projects *project.Resolver, ttl time.Duration) *Provider {
	return &Provider{
		ox:       ox,
		projects: projects,
		cache:    resolver.NewCache[reading]("subnet", ttl),
		vpcs:     newVPCCache(ttl),
	}
}

func newVPCCache(ttl time.Duration) *resolver.Cache[vpcInfo] {
	return resolver.NewCache[vpcInfo]("subnet-vpc", ttl)
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
		func(ctx context.Context) (reading, error) {
			return p.resolve(ctx, selectors)
		},
	)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Resolved: slices.Clone(entry.Value.subnets),
		Age:      entry.Age(time.Now()),
		Stale:    entry.Stale,
	}
	switch {
	case len(entry.Value.misplaced) > 0:
		result.Reason = v1alpha1.ConditionReasonProjectMismatch
		result.Message = strings.Join(entry.Value.misplaced, "; ")
	case len(result.Resolved) == 0:
		result.Unmatched = selectors
		result.Reason = v1alpha1.ConditionReasonSubnetsNotFound
		result.Message = DescribeSelectors(selectors)
	}
	return result, nil
}

func (p *Provider) resolve(
	ctx context.Context,
	subnetSelectors []v1alpha1.SubnetSelector,
) (reading, error) {
	var out reading
	var resolvedSubnets []oxide.VpcSubnet
	vpcIDToName := map[string]string{}
	for _, subnetSelector := range subnetSelectors {
		selected, err := p.resolveSelector(ctx, subnetSelector, vpcIDToName)
		if err != nil {
			return reading{}, err
		}
		for _, subnet := range selected {
			misplaced, err := p.misplaced(ctx, subnetSelector, subnet)
			if err != nil {
				return reading{}, err
			}
			if misplaced != "" {
				out.misplaced = append(out.misplaced, misplaced)
				continue
			}
			resolvedSubnets = append(resolvedSubnets, subnet)
		}
	}

	for _, subnet := range lo.UniqBy(resolvedSubnets, func(subnet oxide.VpcSubnet) string {
		return subnet.Id
	}) {
		vpcName, ok := vpcIDToName[subnet.VpcId]
		if !ok {
			vpc, err := p.lookUpVPC(ctx, subnet.VpcId)
			if err != nil {
				return reading{}, fmt.Errorf("vpc information for subnet %q: %w", subnet.Id, err)
			}
			vpcName = vpc.name
		}
		out.subnets = append(out.subnets, v1alpha1.OxideSubnet{
			ID:        subnet.Id,
			Name:      string(subnet.Name),
			VPCID:     subnet.VpcId,
			VPCName:   vpcName,
			IPv4Block: string(subnet.Ipv4Block),
			IPv6Block: string(subnet.Ipv6Block),
		})

	}
	return out, nil
}

// resolveSelector returns the subnets one selector matches, recording the names of the VPCs it
// learned on the way so they need no second lookup.
func (p *Provider) resolveSelector(
	ctx context.Context,
	selector v1alpha1.SubnetSelector,
	vpcIDToName map[string]string,
) ([]oxide.VpcSubnet, error) {
	in := p.projects.For(selector.Project)
	vpcs, err := p.vpcsFor(ctx, selector, in, vpcIDToName)
	if err != nil {
		return nil, err
	}
	var subnets []oxide.VpcSubnet
	for _, vpcRef := range vpcs {
		found, err := p.subnetsIn(ctx, selector, vpcRef, in)
		if err != nil {
			return nil, err
		}
		for _, subnet := range found {
			if !selector.VPCName.IsPattern() && selector.VPCName != "" {
				vpcIDToName[subnet.VpcId] = string(selector.VPCName)
			}
			subnets = append(subnets, subnet)
		}
	}
	return subnets, nil
}

// vpcsFor returns the VPC references, IDs or exact names, a selector's subnets are looked up in.
// A subnet selected by ID needs none.
func (p *Provider) vpcsFor(
	ctx context.Context,
	selector v1alpha1.SubnetSelector,
	in string,
	vpcIDToName map[string]string,
) ([]string, error) {
	switch {
	case selector.ID != "":
		return []string{""}, nil
	case selector.VPCID != "":
		return []string{string(selector.VPCID)}, nil
	case selector.VPCName.IsPattern():
		vpcs, err := p.ox.VpcListAllPages(ctx, oxide.VpcListParams{Project: oxide.NameOrId(in)})
		if err != nil {
			return nil, fmt.Errorf("unable to list VPCs in project %q: %w", in, err)
		}
		var refs []string
		for _, vpc := range vpcs {
			if selector.VPCName.Matches(string(vpc.Name)) {
				vpcIDToName[vpc.Id] = string(vpc.Name)
				refs = append(refs, vpc.Id)
			}
		}
		return refs, nil
	case selector.VPCName != "":
		return []string{string(selector.VPCName)}, nil
	default:
		return nil, nil
	}
}

// subnetsIn returns the subnets a selector matches within one VPC.
func (p *Provider) subnetsIn(
	ctx context.Context,
	selector v1alpha1.SubnetSelector,
	vpcRef string,
	in string,
) ([]oxide.VpcSubnet, error) {
	nameOrID := lo.CoalesceOrEmpty(string(selector.ID), string(selector.Name))
	vpc := project.Scope(vpcRef, nameOrID)
	scoped := project.Scope(in, nameOrID, vpcRef)

	if selector.ID != "" || (selector.Name != "" && !selector.Name.IsPattern()) {
		subnet, err := p.ox.VpcSubnetView(ctx, oxide.VpcSubnetViewParams{
			Subnet:  oxide.NameOrId(nameOrID),
			Vpc:     vpc,
			Project: scoped,
		})
		if err != nil {
			if errors.Is(err, oxide.ErrObjectNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf(
				"subnet %q from VPC %q and project %q: %w",
				nameOrID, vpc, scoped, err,
			)
		}
		if selector.Name != "" && !selector.Name.Matches(string(subnet.Name)) {
			return nil, nil
		}
		return []oxide.VpcSubnet{*subnet}, nil
	}
	subnets, err := p.ox.VpcSubnetListAllPages(ctx, oxide.VpcSubnetListParams{
		Vpc: vpc, Project: scoped,
	})
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"subnets from VPC %q and project %q: %w", vpc, scoped, err,
		)
	}
	return lo.Filter(subnets, func(subnet oxide.VpcSubnet, _ int) bool {
		return selector.Name == "" || selector.Name.Matches(string(subnet.Name))
	}), nil
}

// misplaced explains how a subnet found by an ID misses the VPC or project the selector meant,
// or returns "" when it is where the selector said. A selector that named its VPC resolved
// inside the project already, so there is nothing to check.
func (p *Provider) misplaced(
	ctx context.Context,
	selector v1alpha1.SubnetSelector,
	subnet oxide.VpcSubnet,
) (string, error) {
	if selector.ID == "" && selector.VPCID == "" {
		return "", nil
	}
	if selector.VPCID != "" && !selector.VPCID.Matches(subnet.VpcId) {
		return fmt.Sprintf(
			"subnet %s is in VPC %s, not VPC %s", subnet.Id, subnet.VpcId, selector.VPCID,
		), nil
	}
	vpc, err := p.lookUpVPC(ctx, subnet.VpcId)
	if err != nil {
		return "", fmt.Errorf("vpc information for subnet %q: %w", subnet.Id, err)
	}
	if selector.VPCName != "" && !selector.VPCName.Matches(vpc.name) {
		return fmt.Sprintf(
			"subnet %s is in VPC %s, not VPC %s", subnet.Id, vpc.name, selector.VPCName,
		), nil
	}
	intended := p.projects.For(selector.Project)
	ok, err := p.projects.Matches(ctx, intended, vpc.projectID)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf(
			"subnet %s is in project %s, not project %s", subnet.Id, vpc.projectID, intended,
		), nil
	}
	return "", nil
}

func (p *Provider) lookUpVPC(ctx context.Context, vpcID string) (vpcInfo, error) {
	entry, err := p.vpcs.Get(ctx, vpcID, func(ctx context.Context) (vpcInfo, error) {
		vpc, err := p.ox.VpcView(ctx, oxide.VpcViewParams{Vpc: oxide.NameOrId(vpcID)})
		if err != nil {
			return vpcInfo{}, err
		}
		return vpcInfo{name: string(vpc.Name), projectID: vpc.ProjectId}, nil
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
