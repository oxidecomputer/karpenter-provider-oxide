// Package affinitygroup resolves placement groups and drift.
package affinitygroup

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
	"k8s.io/apimachinery/pkg/util/sets"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Result is one resolution of a nodeclass's affinity configuration.
type Result = resolver.Resolution[v1alpha1.OxideAffinityGroup, v1alpha1.AffinityGroupSelector]

type reading struct {
	resolved  []v1alpha1.OxideAffinityGroup
	unmatched []v1alpha1.AffinityGroupSelector
}

// Provider is everything that talks to the Oxide API about affinity and anti-affinity groups.
type Provider struct {
	ox *oxide.Client

	project string
	cache   *resolver.Cache[reading]
}

// NewProvider returns a provider that re-resolves a nodeclass's groups once its reading is older than ttl.
func NewProvider(ox *oxide.Client, oxideProject string, ttl time.Duration) *Provider {
	return &Provider{
		ox:      ox,
		project: oxideProject,
		cache:   resolver.NewCache[reading]("affinitygroup", ttl),
	}
}

type oxideGroup struct {
	id          string
	name        string
	description string
	policy      string
}

type groupAPI struct {
	kind string

	noun    string
	view    func(ctx context.Context, nameOrID string) (oxideGroup, error)
	list    func(ctx context.Context) ([]oxideGroup, error)
	create  func(ctx context.Context, name, description, policy string) (oxideGroup, error)
	members func(ctx context.Context, id string) (int, error)
	delete  func(ctx context.Context, id string) error
}

func (p *Provider) groupAPIs() []groupAPI {
	return []groupAPI{p.affinityAPI(), p.antiAffinityAPI()}
}

func (p *Provider) groupAPIFor(kind string) groupAPI {
	if kind == v1alpha1.AffinityGroupTypeAffinity {
		return p.affinityAPI()
	}
	return p.antiAffinityAPI()
}

//nolint:dupl // see above
func (p *Provider) antiAffinityAPI() groupAPI {
	return groupAPI{
		kind: v1alpha1.AffinityGroupTypeAntiAffinity,
		noun: "anti-affinity group",
		view: func(ctx context.Context, nameOrID string) (oxideGroup, error) {
			group, err := p.ox.AntiAffinityGroupView(
				ctx,
				oxide.AntiAffinityGroupViewParams{
					AntiAffinityGroup: oxide.NameOrId(nameOrID),
					Project:           project.Scope(p.project, nameOrID),
				},
			)
			if err != nil {
				return oxideGroup{}, err
			}
			return oxideGroup{
				id:          group.Id,
				name:        string(group.Name),
				description: group.Description,
				policy:      string(group.Policy),
			}, nil
		},
		list: func(ctx context.Context) ([]oxideGroup, error) {
			groups, err := p.ox.AntiAffinityGroupListAllPages(
				ctx,
				oxide.AntiAffinityGroupListParams{Project: oxide.NameOrId(p.project)},
			)
			if err != nil {
				return nil, err
			}
			return lo.Map(
				groups,
				func(group oxide.AntiAffinityGroup, _ int) oxideGroup {
					return oxideGroup{
						id:          group.Id,
						name:        string(group.Name),
						description: group.Description,
						policy:      string(group.Policy),
					}
				},
			), nil
		},
		create: func(ctx context.Context, name, description, policy string) (oxideGroup, error) {
			created, err := p.ox.AntiAffinityGroupCreate(
				ctx,
				oxide.AntiAffinityGroupCreateParams{
					Project: oxide.NameOrId(p.project),
					Body: &oxide.AntiAffinityGroupCreate{
						Name:        oxide.Name(name),
						Description: description,

						FailureDomain: oxide.FailureDomainSled,
						Policy:        oxide.AffinityPolicy(policy),
					},
				},
			)
			if err != nil {
				return oxideGroup{}, err
			}
			return oxideGroup{
				id:          created.Id,
				name:        string(created.Name),
				description: created.Description,
				policy:      string(created.Policy),
			}, nil
		},
		members: func(ctx context.Context, id string) (int, error) {
			members, err := p.ox.AntiAffinityGroupMemberListAllPages(
				ctx,
				oxide.AntiAffinityGroupMemberListParams{AntiAffinityGroup: oxide.NameOrId(id)},
			)
			return len(members), err
		},
		delete: func(ctx context.Context, id string) error {
			return p.ox.AntiAffinityGroupDelete(
				ctx,
				oxide.AntiAffinityGroupDeleteParams{AntiAffinityGroup: oxide.NameOrId(id)},
			)
		},
	}
}

//nolint:dupl // see antiAffinityAPI
func (p *Provider) affinityAPI() groupAPI {
	return groupAPI{
		kind: v1alpha1.AffinityGroupTypeAffinity,
		noun: "affinity group",
		view: func(ctx context.Context, nameOrID string) (oxideGroup, error) {
			group, err := p.ox.ExperimentalAffinityGroupView(
				ctx,
				oxide.AffinityGroupViewParams{
					AffinityGroup: oxide.NameOrId(nameOrID),
					Project:       project.Scope(p.project, nameOrID),
				},
			)
			if err != nil {
				return oxideGroup{}, err
			}
			return oxideGroup{
				id:          group.Id,
				name:        string(group.Name),
				description: group.Description,
				policy:      string(group.Policy),
			}, nil
		},
		list: func(ctx context.Context) ([]oxideGroup, error) {
			groups, err := p.ox.ExperimentalAffinityGroupListAllPages(
				ctx,
				oxide.AffinityGroupListParams{Project: oxide.NameOrId(p.project)},
			)
			if err != nil {
				return nil, err
			}
			return lo.Map(groups, func(group oxide.AffinityGroup, _ int) oxideGroup {
				return oxideGroup{
					id:          group.Id,
					name:        string(group.Name),
					description: group.Description,
					policy:      string(group.Policy),
				}
			}), nil
		},
		create: func(ctx context.Context, name, description, policy string) (oxideGroup, error) {
			created, err := p.ox.ExperimentalAffinityGroupCreate(
				ctx,
				oxide.AffinityGroupCreateParams{
					Project: oxide.NameOrId(p.project),
					Body: &oxide.AffinityGroupCreate{
						Name:          oxide.Name(name),
						Description:   description,
						FailureDomain: oxide.FailureDomainSled,
						Policy:        oxide.AffinityPolicy(policy),
					},
				},
			)
			if err != nil {
				return oxideGroup{}, err
			}
			return oxideGroup{
				id:          created.Id,
				name:        string(created.Name),
				description: created.Description,
				policy:      string(created.Policy),
			}, nil
		},
		members: func(ctx context.Context, id string) (int, error) {
			members, err := p.ox.ExperimentalAffinityGroupMemberListAllPages(
				ctx,
				oxide.AffinityGroupMemberListParams{AffinityGroup: oxide.NameOrId(id)},
			)
			return len(members), err
		},
		delete: func(ctx context.Context, id string) error {
			return p.ox.ExperimentalAffinityGroupDelete(
				ctx,
				oxide.AffinityGroupDeleteParams{AffinityGroup: oxide.NameOrId(id)},
			)
		},
	}
}

// Resolve turns a nodeclass's affinity selectors into the set of groups every instance it launches joins.
func (p *Provider) Resolve(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (Result, error) {
	if nodeClass.Spec.Affinity == nil {
		return Result{}, nil
	}
	selectors := nodeClass.Spec.Affinity

	entry, err := p.cache.Get(
		ctx,

		resolver.Fingerprint([]any{
			selectors.AffinityGroupSelector,
			selectors.AntiAffinityGroupSelector,
			p.project,
		}),
		func(ctx context.Context) (reading, error) {
			return p.resolveSelectors(ctx, selectors)
		},
	)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Age:   entry.Age(time.Now()),
		Stale: entry.Stale,
	}
	if len(entry.Value.unmatched) > 0 {

		result.Unmatched = entry.Value.unmatched
		result.Reason = v1alpha1.ConditionReasonAffinityGroupNotFound
		result.Message = DescribeSelectors(entry.Value.unmatched)
		return result, nil
	}

	groups := slices.Clone(entry.Value.resolved)
	if selectors.Managed != nil {
		managed, failure, err := p.ensureManagedGroup(ctx, nodeClass)
		if err != nil {
			return Result{}, err
		}
		if !failure.Complete() {
			failure.Age, failure.Stale = result.Age, result.Stale
			return failure, nil
		}
		groups = append(groups, managed)
	}

	groups = lo.UniqBy(groups, func(group v1alpha1.OxideAffinityGroup) string { return group.ID })
	slices.SortFunc(groups, func(a, b v1alpha1.OxideAffinityGroup) int {
		return strings.Compare(a.ID, b.ID)
	})
	result.Resolved = groups
	return result, nil
}

func (p *Provider) resolveSelectors(
	ctx context.Context,
	affinity *v1alpha1.Affinity,
) (reading, error) {
	var groups []v1alpha1.OxideAffinityGroup
	var unmatched []v1alpha1.AffinityGroupSelector

	for _, kind := range []struct {
		api       groupAPI
		selectors []v1alpha1.AffinityGroupSelector
	}{
		{p.affinityAPI(), affinity.AffinityGroupSelector},
		{p.antiAffinityAPI(), affinity.AntiAffinityGroupSelector},
	} {
		if len(kind.selectors) == 0 {
			continue
		}
		resolved, missed, err := p.resolveGroups(ctx, kind.api, kind.selectors)
		if err != nil {
			return reading{}, err
		}
		groups = append(groups, resolved...)
		unmatched = append(unmatched, missed...)
	}
	return reading{resolved: groups, unmatched: unmatched}, nil
}

func (p *Provider) resolveGroups(
	ctx context.Context,
	api groupAPI,
	selectors []v1alpha1.AffinityGroupSelector,
) ([]v1alpha1.OxideAffinityGroup, []v1alpha1.AffinityGroupSelector, error) {
	var resolved []v1alpha1.OxideAffinityGroup
	var unmatched []v1alpha1.AffinityGroupSelector
	var wildcards []v1alpha1.AffinityGroupSelector

	for _, selector := range selectors {
		nameOrID := string(selector.ID)
		if nameOrID == "" {
			if selector.Name.IsPattern() {
				wildcards = append(wildcards, selector)
				continue
			}
			nameOrID = string(selector.Name)
		}

		if nameOrID == "" {
			unmatched = append(unmatched, selector)
			continue
		}
		group, err := api.view(ctx, nameOrID)
		if err != nil {
			if errors.Is(err, oxide.ErrObjectNotFound) {
				unmatched = append(unmatched, selector)
				continue
			}
			return nil, nil, fmt.Errorf("%s %q: %w", api.noun, nameOrID, err)
		}
		resolved = append(resolved, api.status(group, false))
	}

	if len(wildcards) == 0 {
		return resolved, unmatched, nil
	}

	groups, err := api.list(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s list error: %w", api.noun, err)
	}
	for _, selector := range wildcards {
		matched := false
		for _, group := range groups {
			if !selector.Name.Matches(group.name) {
				continue
			}

			resolved = append(resolved, api.status(group, false))
			matched = true
		}
		if !matched {
			unmatched = append(unmatched, selector)
		}
	}
	return resolved, unmatched, nil
}

func (a groupAPI) status(group oxideGroup, managed bool) v1alpha1.OxideAffinityGroup {
	return v1alpha1.OxideAffinityGroup{
		ID:      group.id,
		Name:    group.name,
		Type:    a.kind,
		Policy:  group.policy,
		Managed: managed,
	}
}

// DescribeSelectors formats unmatched selectors.
func DescribeSelectors(selectors []v1alpha1.AffinityGroupSelector) string {
	described := lo.Map(
		selectors,
		func(selector v1alpha1.AffinityGroupSelector, _ int) string {
			var terms []string
			for _, term := range []struct{ key, value string }{
				{"id", string(selector.ID)},
				{"name", string(selector.Name)},
			} {
				if term.value != "" {
					terms = append(terms, fmt.Sprintf("%s=%s", term.key, term.value))
				}
			}
			if len(terms) == 0 {
				return "{}"
			}
			return "{" + strings.Join(terms, ",") + "}"
		},
	)
	return "no groups matched " + strings.Join(described, " or ")
}

// DriftReason reports the groups the nodeclass wants that this instance is not in.
func (p *Provider) DriftReason(
	nodeClaim *karpv1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) karpcloudprovider.DriftReason {
	desired := v1alpha1.AffinityGroupIDs(nodeClass.Status.AffinityGroups)
	recorded := sets.New[string]()
	for _, ref := range v1alpha1.ParseAffinityGroups(
		nodeClaim.Annotations[v1alpha1.AnnotationAffinityGroups],
	) {
		recorded.Insert(ref.ID)
	}
	return lo.Ternary(
		desired.Difference(recorded).Len() > 0,
		providers.DriftReasonAffinityGroup,
		"",
	)
}
