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

// Collect deletes the groups Karpenter owns for this nodeclass that are no longer in its desired set.
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

// ManagedIDs is the set of managed group IDs in a resolved desired set, which is what collection keeps.
func ManagedIDs(groups []v1alpha1.OxideAffinityGroup) sets.Set[string] {
	ids := sets.New[string]()
	for _, group := range groups {
		if group.Managed {
			ids.Insert(group.ID)
		}
	}
	return ids
}

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

func managedGroupDescription(nodeClass *v1alpha1.OxideNodeClass) string {
	return fmt.Sprintf(
		"%s managed group for oxidenodeclass/%s (%s)",
		apis.Group,
		nodeClass.Name,
		nodeClass.UID,
	)
}

func managedGroupPolicyCode(policy string) string {
	if policy == string(oxide.AffinityPolicyFail) {
		return "f"
	}
	return "a"
}

const maxOxideNameLength = 63

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
