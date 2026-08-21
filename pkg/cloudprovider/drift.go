package cloudprovider

import (
	"context"
	"fmt"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/samber/lo"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// IsDrifted returns whether a NodeClaim has drifted from the provisioning requirements it is tied to.
//
// Drift is decided two different ways here, because the nodeclass has two different kinds of field.
// What the spec states outright is compared by hash: it can only change when someone edits the
// object, and a hash detects that with no API calls and no per-field bookkeeping. What the spec only
// selects is compared as resolved state instead, because a selector's meaning is a function of the
// project as much as of the nodeclass. Hashing a selector would miss the change that matters most --
// a wildcard quietly matching a newly uploaded image -- and would report one that does not matter at
// all, a selector rewritten to name the resource it already matched.
//
// Resolved state is compared against a record the launch left on the NodeClaim rather than against
// the instance, since the interesting facts are not recoverable from a running instance: a pool is
// not recoverable from an address it handed out, and an authorized key says nothing about which
// selector put it there. An absent record is therefore not an empty one. It means the launch happened
// before this provider kept records, so there is nothing to compare and no drift is reported, which
// is what keeps upgrading Karpenter from replacing a fleet.
//
// The comparisons are also not all symmetric, and each one's direction is a claim about what can be
// changed in place. A subnet leaving the selected set replaces the nodes in it, while a subnet
// joining it replaces nothing, because any of the selected subnets is a valid place to be. An
// affinity group being added replaces nodes, since a group is joined at launch where placement is
// decided, while one being removed does not, since the instance is simply taken out of it in place.
// SSH keys and IP pools are compared as whole sets in both directions: nothing removes a key from a
// running node or takes an address back, so the only way to make either match is to launch again.
func (c *OxideCloudProvider) IsDrifted(
	ctx context.Context,
	nodeClaim *karpcorev1.NodeClaim,
) (cloudprovider.DriftReason, error) {
	nodeClass := &v1alpha1.OxideNodeClass{}
	if err := c.kubeClient.Get(
		ctx,
		client.ObjectKey{Name: nodeClaim.Spec.NodeClassRef.Name},
		nodeClass,
	); err != nil {
		// A nodeclass that is gone says nothing about where its nodes should sit, and the nodes are
		// about to be torn down with it in any case.
		if kerrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("unable to retrieve the OxideNodeClass, %w", err)
	}

	// Cheapest and broadest first, so a spec edit is reported as the spec edit it is rather than as
	// whichever resolved field happens to be checked first.
	if reason := nodeClassDrift(nodeClaim, nodeClass); reason != "" {
		return reason, nil
	}
	// Then one resource at a time, in the order the nodeclass controller resolves them. Each
	// provider owns the comparison for its own resource, because the direction of that comparison is
	// a claim about what can be changed in place -- see the providers themselves.
	for _, drifter := range c.drifters {
		if reason := drifter.DriftReason(nodeClaim, nodeClass); reason != "" {
			return reason, nil
		}
	}
	return "", nil
}

// nodeClassDrift compares the hash of the launch-time configuration the NodeClaim recorded against
// the hash the nodeclass carries now.
//
// Either side missing its hash means the two cannot be compared: a NodeClaim launched before this
// provider recorded one, or a nodeclass the hash controller has not stamped yet. Neither is evidence
// of drift, and treating it as such would replace nodes on the strength of a value that was never
// written. Mismatched hash versions are likewise incomparable, since the same configuration hashes
// differently across versions; the hash controller resolves that by re-stamping, not by drifting.
func nodeClassDrift(
	nodeClaim *karpcorev1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) cloudprovider.DriftReason {
	nodeClassHash, foundNodeClassHash := nodeClass.Annotations[v1alpha1.AnnotationOxideNodeClassHash]
	nodeClassHashVersion := nodeClass.Annotations[v1alpha1.AnnotationOxideNodeClassHashVersion]
	nodeClaimHash, foundNodeClaimHash := nodeClaim.Annotations[v1alpha1.AnnotationOxideNodeClassHash]
	nodeClaimHashVersion := nodeClaim.Annotations[v1alpha1.AnnotationOxideNodeClassHashVersion]

	if !foundNodeClassHash || !foundNodeClaimHash {
		return ""
	}
	if nodeClassHashVersion != nodeClaimHashVersion {
		return ""
	}
	return lo.Ternary(nodeClassHash != nodeClaimHash, providers.DriftReasonNodeClass, "")
}
