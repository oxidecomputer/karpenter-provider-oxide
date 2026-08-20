package nodeclass

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
)

// affinityResolution is the outcome of resolving a nodeclass's affinity configuration.
//
// A resolution that did not complete carries a reason and a message rather than a shorter set of
// groups. Eviction is driven by the desired set, so publishing a set that shrank because of a
// transient API error or a group that has gone missing would drop those memberships across the whole
// fleet. The previous set is left in place instead and the nodeclass goes un-Ready.
type affinityResolution struct {
	// groups is the desired set, only meaningful when reason is empty.
	groups []v1alpha1.OxideAffinityGroup
	// reason and message are the condition to record when the resolution did not complete.
	reason  string
	message string
}

// resolved reports whether the whole configuration resolved, and so whether groups may be published.
func (r affinityResolution) resolved() bool {
	return r.reason == ""
}

// resolveAffinityGroups turns a nodeclass's affinity selectors into the set of groups every instance
// it launches joins.
//
// Selectors are referenced only: nothing here creates, updates or deletes a group. An ID or a name
// with no "*" is looked up directly, which is the only form the Oxide API can resolve; a name
// containing a "*" is matched against a single list of the project's groups, so one list call serves
// every wildcard selector.
func (c *Controller) resolveAffinityGroups(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (affinityResolution, error) {
	if nodeClass.Spec.Affinity == nil {
		return affinityResolution{}, nil
	}

	groups, unmatched, err := c.resolveAntiAffinityGroups(
		ctx,
		nodeClass.Spec.Affinity.AntiAffinityGroupSelector,
	)
	if err != nil {
		return affinityResolution{}, err
	}
	if len(unmatched) > 0 {
		return affinityResolution{
			reason:  v1alpha1.ConditionReasonAffinityGroupNotFound,
			message: describeAffinityGroupSelectors(unmatched),
		}, nil
	}

	// Two selectors may legitimately match the same group, and an instance is a member of a group
	// once or not at all. Sorted so that a set that has not changed does not look like it has.
	groups = lo.UniqBy(groups, func(group v1alpha1.OxideAffinityGroup) string { return group.ID })
	slices.SortFunc(groups, func(a, b v1alpha1.OxideAffinityGroup) int {
		return strings.Compare(a.ID, b.ID)
	})
	return affinityResolution{groups: groups}, nil
}

// resolveAntiAffinityGroups resolves the anti-affinity selectors, returning the groups they matched
// along with the selectors that matched nothing.
//
// A selector matching nothing is returned rather than erroring, because the two are handled
// differently: an unreadable API is retried, whereas a group that is not there is reported on the
// nodeclass so an operator can see which selector is wrong.
func (c *Controller) resolveAntiAffinityGroups(
	ctx context.Context,
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
		group, err := c.oxideClient.AntiAffinityGroupView(ctx, oxide.AntiAffinityGroupViewParams{
			AntiAffinityGroup: oxide.NameOrId(nameOrID),
			Project:           c.projectFor(nameOrID),
		})
		if err != nil {
			if errors.Is(err, oxide.ErrObjectNotFound) {
				unmatched = append(unmatched, selector)
				continue
			}
			return nil, nil, fmt.Errorf("anti-affinity group %q: %w", nameOrID, err)
		}
		resolved = append(resolved, antiAffinityGroup(*group))
	}

	if len(wildcards) == 0 {
		return resolved, unmatched, nil
	}

	// One list serves every wildcard selector, since they are all matched against the same project.
	groups, err := c.oxideClient.AntiAffinityGroupListAllPages(
		ctx,
		oxide.AntiAffinityGroupListParams{Project: oxide.NameOrId(c.project)},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("anti-affinity group list error: %w", err)
	}
	for _, selector := range wildcards {
		matched := false
		for _, group := range groups {
			if !matchName(selector.Name, string(group.Name)) {
				continue
			}
			// Every match is joined rather than one winner being picked, because an instance can
			// belong to many groups. So there is no truncation of the result either: the whole
			// matched set is what launches join and what drift compares against.
			resolved = append(resolved, antiAffinityGroup(group))
			matched = true
		}
		if !matched {
			unmatched = append(unmatched, selector)
		}
	}
	return resolved, unmatched, nil
}

// antiAffinityGroup renders an Oxide anti-affinity group as the status entry for it.
func antiAffinityGroup(group oxide.AntiAffinityGroup) v1alpha1.OxideAffinityGroup {
	return v1alpha1.OxideAffinityGroup{
		ID:     group.Id,
		Name:   string(group.Name),
		Type:   v1alpha1.AffinityGroupTypeAntiAffinity,
		Policy: string(group.Policy),
	}
}

// describeAffinityGroupSelectors renders the selectors that matched nothing back as the terms they
// were written with, so the condition can say which selector is wrong rather than only that
// something is.
func describeAffinityGroupSelectors(selectors []v1alpha1.AffinityGroupSelector) string {
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
