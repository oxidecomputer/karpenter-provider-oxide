// Package affinitygroup resolves a nodeclass's affinity and anti-affinity group selectors into the
// groups every instance it launches joins, owns the one group Karpenter creates for a nodeclass that
// asks for one, and decides whether a node missing a desired group has drifted.
//
// The only provider that writes. Resolution is cached like everywhere else, but the managed group's
// lifecycle in managed.go is not: creating a group and collecting the ones a nodeclass no longer
// wants are writes, and a write served from a cache would be a write that did not happen. See
// managed.go.
package affinitygroup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/resolver"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Result is one resolution of a nodeclass's affinity configuration.
//
// A resolution that did not complete carries a Reason and a Message rather than a shorter set of
// groups. Eviction is driven by the desired set, so publishing a set that shrank because of a
// transient API error or a group that has gone missing would drop those memberships across the whole
// fleet. The previous set is left in place instead and the nodeclass goes un-Ready.
type Result = resolver.Resolution[v1alpha1.OxideAffinityGroup, v1alpha1.AffinityGroupSelector]

// reading is what the cache holds: the groups the selectors matched and the selectors that matched
// nothing. Both halves are needed, because a selector that matched nothing is reported on the
// nodeclass rather than retried, and the condition message has to name it.
//
// Only the selector resolution is cached. The managed group is appended afterwards on every
// reconcile, since ensuring it exists is a write.
type reading struct {
	resolved  []v1alpha1.OxideAffinityGroup
	unmatched []v1alpha1.AffinityGroupSelector
}

// Provider is everything that talks to the Oxide API about affinity and anti-affinity groups.
type Provider struct {
	ox *oxide.Client
	// project is the Oxide project the groups live in, which is what makes a group name unambiguous
	// and so is part of the question a cached reading answered.
	project string
	cache   *resolver.Cache[reading]
}

// NewProvider returns a provider that re-resolves a nodeclass's groups once its reading is older
// than ttl.
func NewProvider(ox *oxide.Client, project string, ttl time.Duration) *Provider {
	return &Provider{
		ox:      ox,
		project: project,
		cache:   resolver.NewCache[reading]("affinitygroup", ttl),
	}
}

// projectFor returns the project a lookup should be scoped by. A project is what makes a name
// unambiguous, but the API rejects one passed alongside an ID, which is unambiguous already.
func (p *Provider) projectFor(ref string) oxide.NameOrId {
	if _, err := uuid.Parse(ref); err == nil {
		return ""
	}
	return oxide.NameOrId(p.project)
}

// oxideGroup is the part of an Oxide affinity or anti-affinity group this package works with.
//
// The two kinds are structurally identical and live behind entirely separate endpoints with no
// common Go type between them, so this and groupAPI below are what let one implementation of
// resolution, ownership and collection serve both rather than being written out twice.
type oxideGroup struct {
	id          string
	name        string
	description string
	policy      string
}

// groupAPI is the half of the Oxide API one kind of group lives behind.
type groupAPI struct {
	// kind is the type reported on a resolved group, and the noun used in errors and log lines.
	kind string
	// noun is how the kind reads in prose.
	noun    string
	view    func(ctx context.Context, nameOrID string) (oxideGroup, error)
	list    func(ctx context.Context) ([]oxideGroup, error)
	create  func(ctx context.Context, name, description, policy string) (oxideGroup, error)
	members func(ctx context.Context, id string) (int, error)
	delete  func(ctx context.Context, id string) error
}

// groupAPIs is both halves, in the order they are resolved and collected in.
func (p *Provider) groupAPIs() []groupAPI {
	return []groupAPI{p.affinityAPI(), p.antiAffinityAPI()}
}

// groupAPIFor is the half a group kind lives behind. Anything other than Affinity is anti-affinity,
// which is both the enum's other value and the safer default: it is the non-experimental API.
func (p *Provider) groupAPIFor(kind string) groupAPI {
	if kind == v1alpha1.AffinityGroupTypeAffinity {
		return p.affinityAPI()
	}
	return p.antiAffinityAPI()
}

// The two adapters below are deliberately near-identical. The affinity and anti-affinity halves of
// the API share no Go type at all, so the resemblance cannot be factored out any further than the
// groupAPI indirection above already does.
//
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
					Project:           p.projectFor(nameOrID),
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
						// The only failure domain the API offers, so there is nothing to expose.
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

// affinityAPI is the experimental half. Every affinity-group method the SDK offers carries the
// Experimental prefix; anti-affinity does not.
//
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
					Project:       p.projectFor(nameOrID),
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

// Resolve turns a nodeclass's affinity selectors into the set of groups every instance it launches
// joins.
//
// The selector half is cached; the managed group is not. Ensuring the managed group exists is a
// write, and it is also the half that can fail in ways an operator has to act on rather than ways
// worth retrying, so it runs on every reconcile against the live API.
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
		// The project is part of the key as well as the selectors: the same name in two projects is
		// two different groups, so a reading taken in one does not answer the question in the other.
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
		// Reported rather than published. A selector naming a group that is not there is a statement
		// about the project an operator has to act on, and shrinking the desired set over it would
		// evict those memberships across the fleet in the meantime.
		result.Unmatched = entry.Value.unmatched
		result.Reason = v1alpha1.ConditionReasonAffinityGroupNotFound
		result.Message = DescribeSelectors(entry.Value.unmatched)
		return result, nil
	}

	// Cloned before anything is appended, so that the managed group is not written into the reading
	// the cache goes on serving.
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

	// Two selectors may legitimately match the same group, and an instance is a member of a group
	// once or not at all. Sorted so that a set that has not changed does not look like it has.
	groups = lo.UniqBy(groups, func(group v1alpha1.OxideAffinityGroup) string { return group.ID })
	slices.SortFunc(groups, func(a, b v1alpha1.OxideAffinityGroup) int {
		return strings.Compare(a.ID, b.ID)
	})
	result.Resolved = groups
	return result, nil
}

// resolveSelectors resolves both kinds of selector against the Oxide API.
//
// Selectors are referenced only: nothing here creates, updates or deletes a group they name. An ID or
// a name with no "*" is looked up directly, which is the only form the Oxide API can resolve; a name
// containing a "*" is matched against a single list of the project's groups, so one list call serves
// every wildcard selector of that kind.
func (p *Provider) resolveSelectors(
	ctx context.Context,
	affinity *v1alpha1.Affinity,
) (reading, error) {
	var groups []v1alpha1.OxideAffinityGroup
	var unmatched []v1alpha1.AffinityGroupSelector
	// Two lists rather than one, because an instance legitimately belongs to several groups of both
	// kinds and the two use different API endpoints. Walked in a fixed order so that the condition
	// message an unresolved selector produces is stable rather than reshuffling every reconcile.
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

// resolveGroups resolves the selectors for one kind of group, returning the groups they matched along
// with the selectors that matched nothing.
//
// A selector matching nothing is returned rather than erroring, because the two are handled
// differently: an unreadable API is retried, whereas a group that is not there is reported on the
// nodeclass so an operator can see which selector is wrong.
func (p *Provider) resolveGroups(
	ctx context.Context,
	api groupAPI,
	selectors []v1alpha1.AffinityGroupSelector,
) ([]v1alpha1.OxideAffinityGroup, []v1alpha1.AffinityGroupSelector, error) {
	var resolved []v1alpha1.OxideAffinityGroup
	var unmatched []v1alpha1.AffinityGroupSelector
	var wildcards []v1alpha1.AffinityGroupSelector

	for _, selector := range selectors {
		nameOrID := selector.ID
		if nameOrID == "" {
			if strings.Contains(selector.Name, "*") {
				wildcards = append(wildcards, selector)
				continue
			}
			nameOrID = selector.Name
		}
		// Neither an ID nor a name. Nothing to look up, and an empty selector matching every group
		// would be a far worse reading of it than matching none.
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

	// One list serves every wildcard selector, since they are all matched against the same project.
	groups, err := api.list(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s list error: %w", api.noun, err)
	}
	for _, selector := range wildcards {
		matched := false
		for _, group := range groups {
			if !resolver.MatchName(selector.Name, group.name) {
				continue
			}
			// Every match is joined rather than one winner being picked, because an instance can
			// belong to many groups. So there is no truncation of the result either: the whole
			// matched set is what launches join and what drift compares against.
			resolved = append(resolved, api.status(group, false))
			matched = true
		}
		if !matched {
			unmatched = append(unmatched, selector)
		}
	}
	return resolved, unmatched, nil
}

// status renders an Oxide group as the status entry for it.
func (a groupAPI) status(group oxideGroup, managed bool) v1alpha1.OxideAffinityGroup {
	return v1alpha1.OxideAffinityGroup{
		ID:      group.id,
		Name:    group.name,
		Type:    a.kind,
		Policy:  group.policy,
		Managed: managed,
	}
}

// DescribeSelectors renders the selectors that matched nothing back as the terms they
// were written with, so the condition can say which selector is wrong rather than only that
// something is.
func DescribeSelectors(selectors []v1alpha1.AffinityGroupSelector) string {
	described := lo.Map(
		selectors,
		func(selector v1alpha1.AffinityGroupSelector, _ int) string {
			var terms []string
			for _, term := range []struct{ key, value string }{
				{"id", selector.ID},
				{"name", selector.Name},
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
//
// Additions only. A missing annotation therefore needs no special case: it is the empty set, and the
// empty set drifts exactly when something is desired, which is what makes a node launched before the
// feature existed drift once groups are configured and not before. A group leaving the desired set is
// not drift, because the affinity controller removes the membership in place.
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
