package nodeclass

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"
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

	if nodeClass.Spec.Affinity.Managed != nil {
		managed, failure, err := c.ensureManagedGroup(ctx, nodeClass)
		if err != nil {
			return affinityResolution{}, err
		}
		if !failure.resolved() {
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
		resolved = append(resolved, antiAffinityGroup(*group, false))
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
			resolved = append(resolved, antiAffinityGroup(group, false))
			matched = true
		}
		if !matched {
			unmatched = append(unmatched, selector)
		}
	}
	return resolved, unmatched, nil
}

// ensureManagedGroup creates the one group Karpenter owns for this nodeclass if it is not already
// there, and returns it as a member of the desired set.
//
// A group that already carries the desired name but not the ownership marker is never adopted. The
// nodeclass holds un-Ready instead, because silently taking ownership of a hand-made group means
// silently deleting it later.
//
// The second return value is the condition to record when the group could not be resolved, and is
// zero when it could. A resolution failure here is not an error: it is a statement about the
// nodeclass or the project that an operator has to act on, whereas an error is an API call to retry.
func (c *Controller) ensureManagedGroup(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (v1alpha1.OxideAffinityGroup, affinityResolution, error) {
	name, err := managedGroupName(nodeClass)
	if err != nil {
		return v1alpha1.OxideAffinityGroup{}, affinityResolution{
			reason:  v1alpha1.ConditionReasonInvalidGroupName,
			message: err.Error(),
		}, nil
	}
	description := managedGroupDescription(nodeClass)

	existing, err := c.oxideClient.AntiAffinityGroupView(ctx, oxide.AntiAffinityGroupViewParams{
		AntiAffinityGroup: oxide.NameOrId(name),
		Project:           oxide.NameOrId(c.project),
	})
	switch {
	case err == nil:
		// The description is the only marker these objects offer: they carry no tags or labels. So
		// ownership is an exact match against the marker this nodeclass would have written, UID
		// included, which is also what stops one cluster from reclaiming another's group.
		if existing.Description != description {
			return v1alpha1.OxideAffinityGroup{}, affinityResolution{
				reason: v1alpha1.ConditionReasonGroupNotOwned,
				message: fmt.Sprintf(
					"anti-affinity group %q already exists and is not managed by this nodeclass", name,
				),
			}, nil
		}
		return antiAffinityGroup(*existing, true), affinityResolution{}, nil

	case !errors.Is(err, oxide.ErrObjectNotFound):
		return v1alpha1.OxideAffinityGroup{}, affinityResolution{}, fmt.Errorf(
			"managed anti-affinity group %q: %w", name, err,
		)
	}

	created, err := c.oxideClient.AntiAffinityGroupCreate(ctx, oxide.AntiAffinityGroupCreateParams{
		Project: oxide.NameOrId(c.project),
		Body: &oxide.AntiAffinityGroupCreate{
			Name:        oxide.Name(name),
			Description: description,
			// The only failure domain the API offers, so there is nothing to expose on the spec.
			FailureDomain: oxide.FailureDomainSled,
			Policy:        oxide.AffinityPolicy(nodeClass.Spec.Affinity.Managed.Policy),
		},
	})
	if err != nil {
		// Created between the view above and this call. Retried rather than resolved here: deciding
		// ownership from the losing side of a race would have to guess at what it lost to, whereas
		// the next reconcile views the group and either adopts it or refuses it on the marker.
		return v1alpha1.OxideAffinityGroup{}, affinityResolution{}, fmt.Errorf(
			"unable to create the managed anti-affinity group %q: %w", name, err,
		)
	}
	log.FromContext(ctx).Info(
		"created the managed anti-affinity group for the nodeclass",
		"group", name,
		"policy", created.Policy,
	)
	return antiAffinityGroup(*created, true), affinityResolution{}, nil
}

// collectManagedGroups deletes the groups Karpenter owns for this nodeclass that are no longer in
// its desired set, and reports whether any of them is still being held.
//
// A superseded group and a removed one are the same case: a group Karpenter owns that is not in the
// desired set. Both are emptied by the nodeclaim controller as their nodes are replaced or simply
// drop the membership, and both are deleted here once empty. Groups Karpenter does not own are never
// deleted, only left behind with its memberships dropped.
//
// keep is passed in rather than read from the status so that finalization can collect everything by
// passing an empty set, regardless of what the spec still asks for.
//
// Run on every reconcile rather than only on nodeclasses configuring a managed group, because
// whether one owns a group is a fact about the project and not about the current spec: a nodeclass
// whose managed group has just been unset has nothing left in its spec to say a group is out there.
func (c *Controller) collectManagedGroups(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	keep sets.Set[string],
) (held bool, err error) {
	description := managedGroupDescription(nodeClass)

	groups, err := c.oxideClient.AntiAffinityGroupListAllPages(
		ctx,
		oxide.AntiAffinityGroupListParams{Project: oxide.NameOrId(c.project)},
	)
	if err != nil {
		return false, fmt.Errorf("anti-affinity group list error: %w", err)
	}

	var errs []error
	for _, group := range groups {
		if group.Description != description || keep.Has(group.Id) {
			continue
		}
		// Emptiness is established with a member list rather than by attempting the delete and
		// interpreting the refusal, so that a delete refused for some other reason is not mistaken
		// for a group that still has members.
		members, err := c.oxideClient.AntiAffinityGroupMemberListAllPages(
			ctx,
			oxide.AntiAffinityGroupMemberListParams{AntiAffinityGroup: oxide.NameOrId(group.Id)},
		)
		if err != nil {
			errs = append(
				errs,
				fmt.Errorf("members of anti-affinity group %q: %w", group.Name, err),
			)
			continue
		}
		if len(members) > 0 {
			held = true
			continue
		}
		if err := c.oxideClient.AntiAffinityGroupDelete(ctx, oxide.AntiAffinityGroupDeleteParams{
			AntiAffinityGroup: oxide.NameOrId(group.Id),
		}); err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
			errs = append(
				errs,
				fmt.Errorf("unable to delete anti-affinity group %q: %w", group.Name, err),
			)
			continue
		}
		log.FromContext(ctx).Info(
			"deleted a managed anti-affinity group the nodeclass no longer wants",
			"group", group.Name,
		)
	}
	return held, errors.Join(errs...)
}

// managedGroupIDs is the set of managed group IDs in a resolved desired set, which is what
// collection keeps. Only the managed ones: a selected group Karpenter does not own is never deleted,
// so it is never a candidate for collection either.
func managedGroupIDs(groups []v1alpha1.OxideAffinityGroup) sets.Set[string] {
	ids := sets.New[string]()
	for _, group := range groups {
		if group.Managed {
			ids.Insert(group.ID)
		}
	}
	return ids
}

// managedGroupName is the name of the group Karpenter owns for a nodeclass:
//
//	<nodeclass>-<aa|af>-<a|f>
//
// So default-aa-a is the allow-policy anti-affinity group for nodeclass "default".
//
// The name encodes the type and the policy because neither can be patched: changing one means a new
// object, and the old group cannot be emptied first, since nodes leave it only by being replaced and
// the replacements have to join something during the drift. Under a fixed name they would join the
// group that is supposed to be going away, which would then never empty and never be deletable.
//
// The suffix is deliberately terse. The group does not need a self-describing name — it carries the
// policy as a field, its kind is implied by the collection it lives in, and the description carries
// the prose — and every character spent restating that is one taken from the nodeclass name the user
// chose.
func managedGroupName(nodeClass *v1alpha1.OxideNodeClass) (string, error) {
	managed := nodeClass.Spec.Affinity.Managed
	name := fmt.Sprintf(
		"%s-%s-%s",
		nodeClass.Name,
		v1alpha1.AffinityGroupTypeCode(managed.Type),
		managedGroupPolicyCode(managed.Policy),
	)
	if err := validOxideName(name); err != nil {
		return "", fmt.Errorf(
			"the managed group name %q derived from the nodeclass name is not a legal Oxide name: %w",
			name, err,
		)
	}
	return name, nil
}

// managedGroupDescription is the ownership marker written into the managed group's description,
// which is the only marker these objects offer: they carry no tags or labels.
//
// The UID is included so that two clusters sharing one Oxide project cannot garbage collect each
// other's group. The name collision itself is inherent, so one Karpenter-project pairing per
// nodeclass name is the supported configuration.
func managedGroupDescription(nodeClass *v1alpha1.OxideNodeClass) string {
	return fmt.Sprintf(
		"%s managed group for oxidenodeclass/%s (%s)",
		apis.Group,
		nodeClass.Name,
		nodeClass.UID,
	)
}

// managedGroupPolicyCode is the policy as it appears in a managed group's name. An unset policy is
// defaulted by the API server, so it only reaches here empty on an object written before the
// default existed, where allow is what the API itself would have chosen.
func managedGroupPolicyCode(policy string) string {
	if policy == string(oxide.AffinityPolicyFail) {
		return "f"
	}
	return "a"
}

// maxOxideNameLength is the Oxide API's limit on a Name.
const maxOxideNameLength = 63

// validOxideName reports whether a string is a legal Oxide Name, explaining what is wrong with it
// when it is not. Names must begin with a lowercase ASCII letter, consist only of ASCII letters,
// digits and "-", not end with a "-", not be a UUID, and be at most 63 characters.
//
// Only ever called on a name that already carries the managed group suffix, so the trailing-hyphen
// and bare-UUID rules cannot be violated and are not checked.
func validOxideName(name string) error {
	if len(name) > maxOxideNameLength {
		return fmt.Errorf(
			"it is %d characters, over the %d character limit", len(name), maxOxideNameLength,
		)
	}
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return errors.New("it does not begin with a lowercase ASCII letter")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			continue
		default:
			return fmt.Errorf(
				"it contains %q, which is not an ASCII letter, a digit or a hyphen", r,
			)
		}
	}
	return nil
}

// antiAffinityGroup renders an Oxide anti-affinity group as the status entry for it.
func antiAffinityGroup(
	group oxide.AntiAffinityGroup,
	managed bool,
) v1alpha1.OxideAffinityGroup {
	return v1alpha1.OxideAffinityGroup{
		ID:      group.Id,
		Name:    string(group.Name),
		Type:    v1alpha1.AffinityGroupTypeAntiAffinity,
		Policy:  string(group.Policy),
		Managed: managed,
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
