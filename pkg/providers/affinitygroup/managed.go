// The managed affinity group: the one group Karpenter creates, keeps and deletes for a nodeclass
// that asks for one.
//
// Separate from resolution because it is a genuinely separate half. It writes, it is driven by the
// nodeclass's finalizer as well as by its spec, and it is swept regardless of what the spec currently
// asks for -- a nodeclass whose managed group has just been unset has nothing left in its spec to say
// the old group is still out there. None of it is cached, for the same reason: a write served from a
// cache is a write that did not happen.
package affinitygroup

import (
	"context"
	"errors"
	"fmt"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

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
func (p *Provider) ensureManagedGroup(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (v1alpha1.OxideAffinityGroup, Result, error) {
	name, err := managedGroupName(nodeClass)
	if err != nil {
		return v1alpha1.OxideAffinityGroup{}, Result{
			Reason:  v1alpha1.ConditionReasonInvalidGroupName,
			Message: err.Error(),
		}, nil
	}
	managed := nodeClass.Spec.Affinity.Managed
	api := p.groupAPIFor(managed.Type)
	description := managedGroupDescription(nodeClass)

	existing, err := api.view(ctx, name)
	switch {
	case err == nil:
		// The description is the only marker these objects offer: they carry no tags or labels. So
		// ownership is an exact match against the marker this nodeclass would have written, UID
		// included, which is also what stops one cluster from reclaiming another's group.
		if existing.description != description {
			return v1alpha1.OxideAffinityGroup{}, Result{
				Reason: v1alpha1.ConditionReasonGroupNotOwned,
				Message: fmt.Sprintf(
					"%s %q already exists and is not managed by this nodeclass", api.noun, name,
				),
			}, nil
		}
		return api.status(existing, true), Result{}, nil

	case !errors.Is(err, oxide.ErrObjectNotFound):
		return v1alpha1.OxideAffinityGroup{}, Result{}, fmt.Errorf(
			"managed %s %q: %w", api.noun, name, err,
		)
	}

	created, err := api.create(ctx, name, description, managed.Policy)
	if err != nil {
		// Includes the group having been created between the view above and this call. Retried rather
		// than resolved here: deciding ownership from the losing side of a race would have to guess at
		// what it lost to, whereas the next reconcile views the group and either adopts it or refuses
		// it on the marker.
		return v1alpha1.OxideAffinityGroup{}, Result{}, fmt.Errorf(
			"unable to create the managed %s %q: %w", api.noun, name, err,
		)
	}
	log.FromContext(ctx).Info(
		"created the managed group for the nodeclass",
		"group", name,
		"kind", api.kind,
		"policy", created.policy,
	)
	return api.status(created, true), Result{}, nil
}

// Collect deletes the groups Karpenter owns for this nodeclass that are no longer in
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
func (p *Provider) Collect(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	keep sets.Set[string],
) (held bool, err error) {
	description := managedGroupDescription(nodeClass)

	var errs []error
	for _, api := range p.groupAPIs() {
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

// ManagedIDs is the set of managed group IDs in a resolved desired set, which is what
// collection keeps. Only the managed ones: a selected group Karpenter does not own is never deleted,
// so it is never a candidate for collection either.
func ManagedIDs(groups []v1alpha1.OxideAffinityGroup) sets.Set[string] {
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
			name,
			err,
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
