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
func (c *Controller) groupAPIs() []groupAPI {
	return []groupAPI{c.affinityAPI(), c.antiAffinityAPI()}
}

// groupAPIFor is the half a group kind lives behind. Anything other than Affinity is anti-affinity,
// which is both the enum's other value and the safer default: it is the non-experimental API.
func (c *Controller) groupAPIFor(kind string) groupAPI {
	if kind == v1alpha1.AffinityGroupTypeAffinity {
		return c.affinityAPI()
	}
	return c.antiAffinityAPI()
}

func (c *Controller) antiAffinityAPI() groupAPI {
	return groupAPI{
		kind: v1alpha1.AffinityGroupTypeAntiAffinity,
		noun: "anti-affinity group",
		view: func(ctx context.Context, nameOrID string) (oxideGroup, error) {
			group, err := c.oxideClient.AntiAffinityGroupView(
				ctx,
				oxide.AntiAffinityGroupViewParams{
					AntiAffinityGroup: oxide.NameOrId(nameOrID),
					Project:           c.projectFor(nameOrID),
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
			groups, err := c.oxideClient.AntiAffinityGroupListAllPages(
				ctx,
				oxide.AntiAffinityGroupListParams{Project: oxide.NameOrId(c.project)},
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
			created, err := c.oxideClient.AntiAffinityGroupCreate(
				ctx,
				oxide.AntiAffinityGroupCreateParams{
					Project: oxide.NameOrId(c.project),
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
			members, err := c.oxideClient.AntiAffinityGroupMemberListAllPages(
				ctx,
				oxide.AntiAffinityGroupMemberListParams{AntiAffinityGroup: oxide.NameOrId(id)},
			)
			return len(members), err
		},
		delete: func(ctx context.Context, id string) error {
			return c.oxideClient.AntiAffinityGroupDelete(
				ctx,
				oxide.AntiAffinityGroupDeleteParams{AntiAffinityGroup: oxide.NameOrId(id)},
			)
		},
	}
}

// affinityAPI is the experimental half. Every affinity-group method the SDK offers carries the
// Experimental prefix; anti-affinity does not.
func (c *Controller) affinityAPI() groupAPI {
	return groupAPI{
		kind: v1alpha1.AffinityGroupTypeAffinity,
		noun: "affinity group",
		view: func(ctx context.Context, nameOrID string) (oxideGroup, error) {
			group, err := c.oxideClient.ExperimentalAffinityGroupView(
				ctx,
				oxide.AffinityGroupViewParams{
					AffinityGroup: oxide.NameOrId(nameOrID),
					Project:       c.projectFor(nameOrID),
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
			groups, err := c.oxideClient.ExperimentalAffinityGroupListAllPages(
				ctx,
				oxide.AffinityGroupListParams{Project: oxide.NameOrId(c.project)},
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
			created, err := c.oxideClient.ExperimentalAffinityGroupCreate(
				ctx,
				oxide.AffinityGroupCreateParams{
					Project: oxide.NameOrId(c.project),
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
			members, err := c.oxideClient.ExperimentalAffinityGroupMemberListAllPages(
				ctx,
				oxide.AffinityGroupMemberListParams{AffinityGroup: oxide.NameOrId(id)},
			)
			return len(members), err
		},
		delete: func(ctx context.Context, id string) error {
			return c.oxideClient.ExperimentalAffinityGroupDelete(
				ctx,
				oxide.AffinityGroupDeleteParams{AffinityGroup: oxide.NameOrId(id)},
			)
		},
	}
}

// resolveAffinityGroups turns a nodeclass's affinity selectors into the set of groups every instance
// it launches joins.
//
// Selectors are referenced only: nothing here creates, updates or deletes a group they name. An ID or
// a name with no "*" is looked up directly, which is the only form the Oxide API can resolve; a name
// containing a "*" is matched against a single list of the project's groups, so one list call serves
// every wildcard selector of that kind.
func (c *Controller) resolveAffinityGroups(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (affinityResolution, error) {
	if nodeClass.Spec.Affinity == nil {
		return affinityResolution{}, nil
	}

	var groups []v1alpha1.OxideAffinityGroup
	var unmatched []v1alpha1.AffinityGroupSelector
	// Two lists rather than one, because an instance legitimately belongs to several groups of both
	// kinds and the two use different API endpoints. Walked in a fixed order so that the condition
	// message an unresolved selector produces is stable rather than reshuffling every reconcile.
	for _, kind := range []struct {
		api       groupAPI
		selectors []v1alpha1.AffinityGroupSelector
	}{
		{c.affinityAPI(), nodeClass.Spec.Affinity.AffinityGroupSelector},
		{c.antiAffinityAPI(), nodeClass.Spec.Affinity.AntiAffinityGroupSelector},
	} {
		if len(kind.selectors) == 0 {
			continue
		}
		resolved, missed, err := c.resolveGroups(ctx, kind.api, kind.selectors)
		if err != nil {
			return affinityResolution{}, err
		}
		groups = append(groups, resolved...)
		unmatched = append(unmatched, missed...)
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

// resolveGroups resolves the selectors for one kind of group, returning the groups they matched along
// with the selectors that matched nothing.
//
// A selector matching nothing is returned rather than erroring, because the two are handled
// differently: an unreadable API is retried, whereas a group that is not there is reported on the
// nodeclass so an operator can see which selector is wrong.
func (c *Controller) resolveGroups(
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
			if !matchName(selector.Name, group.name) {
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
	managed := nodeClass.Spec.Affinity.Managed
	api := c.groupAPIFor(managed.Type)
	description := managedGroupDescription(nodeClass)

	existing, err := api.view(ctx, name)
	switch {
	case err == nil:
		// The description is the only marker these objects offer: they carry no tags or labels. So
		// ownership is an exact match against the marker this nodeclass would have written, UID
		// included, which is also what stops one cluster from reclaiming another's group.
		if existing.description != description {
			return v1alpha1.OxideAffinityGroup{}, affinityResolution{
				reason: v1alpha1.ConditionReasonGroupNotOwned,
				message: fmt.Sprintf(
					"%s %q already exists and is not managed by this nodeclass", api.noun, name,
				),
			}, nil
		}
		return api.status(existing, true), affinityResolution{}, nil

	case !errors.Is(err, oxide.ErrObjectNotFound):
		return v1alpha1.OxideAffinityGroup{}, affinityResolution{}, fmt.Errorf(
			"managed %s %q: %w", api.noun, name, err,
		)
	}

	created, err := api.create(ctx, name, description, managed.Policy)
	if err != nil {
		// Includes the group having been created between the view above and this call. Retried rather
		// than resolved here: deciding ownership from the losing side of a race would have to guess at
		// what it lost to, whereas the next reconcile views the group and either adopts it or refuses
		// it on the marker.
		return v1alpha1.OxideAffinityGroup{}, affinityResolution{}, fmt.Errorf(
			"unable to create the managed %s %q: %w", api.noun, name, err,
		)
	}
	log.FromContext(ctx).Info(
		"created the managed group for the nodeclass",
		"group", name,
		"kind", api.kind,
		"policy", created.policy,
	)
	return api.status(created, true), affinityResolution{}, nil
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
// Both kinds are swept regardless of what the spec asks for now, and on every reconcile rather than
// only on nodeclasses configuring a managed group, because whether one owns a group is a fact about
// the project and not about the current spec: a nodeclass whose managed group has just been unset, or
// whose type has just been changed, has nothing left in its spec to say the old group is out there.
func (c *Controller) collectManagedGroups(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	keep sets.Set[string],
) (held bool, err error) {
	description := managedGroupDescription(nodeClass)

	var errs []error
	for _, api := range c.groupAPIs() {
		groups, err := api.list(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s list error: %w", api.noun, err))
			continue
		}
		for _, group := range groups {
			if group.description != description || keep.Has(group.id) {
				continue
			}
			// Emptiness is established with a member list rather than by attempting the delete and
			// interpreting the refusal, so that a delete refused for some other reason is not
			// mistaken for a group that still has members.
			members, err := api.members(ctx, group.id)
			if err != nil {
				errs = append(errs, fmt.Errorf("members of %s %q: %w", api.noun, group.name, err))
				continue
			}
			if members > 0 {
				held = true
				continue
			}
			if err := api.delete(ctx, group.id); err != nil &&
				!errors.Is(err, oxide.ErrObjectNotFound) {
				errs = append(
					errs,
					fmt.Errorf("unable to delete %s %q: %w", api.noun, group.name, err),
				)
				continue
			}
			log.FromContext(ctx).Info(
				"deleted a managed group the nodeclass no longer wants",
				"group", group.name,
				"kind", api.kind,
			)
		}
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
// So default-aa-a is the allow-policy anti-affinity group for nodeclass "default", and gpu-nodes-af-f
// is the fail-policy affinity group for "gpu-nodes".
//
// The name encodes the type and the policy because neither can be patched: changing one means a new
// object, and the old group cannot be emptied first, since nodes leave it only by being replaced and
// the replacements have to join something during the drift. Under a fixed name they would join the
// group that is supposed to be going away, which would then never empty and never be deletable.
//
// The suffix is deliberately terse. The group does not need a self-describing name — it carries the
// policy as a field, its kind is implied by the collection it lives in, and the description carries
// the prose — and every character spent restating that is one taken from the nodeclass name the user
// chose. At 5 characters, nodeclass names up to 58 characters work.
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
