package cloudprovider

import (
	"context"
	"fmt"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/samber/lo"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Drift reasons, one per way a node can stop matching the nodeclass it came from. They are separate
// because they are the text an operator reads on the NodeClaim to find out why a node is being
// replaced, and "the nodeclass changed" is a poor answer when the nodeclass did not.
const (
	// DriftReasonNodeClass is a change to what a launch fixes into an instance and cannot revisit
	// afterwards: user data, environment, bootstrap configuration, kubelet args, boot disk size.
	// Detected by hash, since every one of those is declared on the spec and nothing outside
	// Kubernetes can move it.
	DriftReasonNodeClass cloudprovider.DriftReason = "NodeClassDrift"
	// DriftReasonImage is a node running an image the nodeclass would no longer launch, whether
	// because the selector changed or because a newer image matched the one it already had.
	DriftReasonImage cloudprovider.DriftReason = "ImageDrift"
	// DriftReasonSubnet is a node whose interface sits in a subnet the nodeclass no longer selects.
	// An interface cannot be moved between subnets, so replacing the node is the only way out of one.
	DriftReasonSubnet cloudprovider.DriftReason = "SubnetDrift"
	// DriftReasonSSHKey is a node whose authorized keys are not the ones the nodeclass now selects.
	// Keys are copied in at launch, so neither adding nor removing one reaches a running node.
	DriftReasonSSHKey cloudprovider.DriftReason = "SSHKeyDrift"
	// DriftReasonIPPool is a node whose ephemeral external addresses do not come from the pools the
	// nodeclass now selects.
	DriftReasonIPPool cloudprovider.DriftReason = "IPPoolDrift"
	// DriftReasonAffinityGroup is reported when a node is not in a group its nodeclass now says every
	// instance should be in. Replacing it is the only way it can join, since a group is joined at launch.
	DriftReasonAffinityGroup cloudprovider.DriftReason = "AffinityGroup"
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
	for _, drifted := range []func(*karpcorev1.NodeClaim, *v1alpha1.OxideNodeClass) cloudprovider.DriftReason{
		nodeClassDrift,
		imageDrift,
		subnetDrift,
		sshKeyDrift,
		ipPoolDrift,
		affinityGroupDrift,
	} {
		if reason := drifted(nodeClaim, nodeClass); reason != "" {
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
	return lo.Ternary(nodeClassHash != nodeClaimHash, DriftReasonNodeClass, "")
}

// imageDrift compares the image the instance booted from against the one the nodeclass would launch
// now, which is the first of the resolved images.
//
// Only the first: the runners-up are reported so a wildcard selector can be understood, not because a
// node is entitled to any of them. Nodes are consequently rolled onto a newly uploaded image that a
// wildcard matches, which is the point of writing a wildcard rather than pinning a version.
//
// An empty resolved list is not drift. It is a resolution that found nothing, already reported as
// ImagesResolved being false, and replacing the fleet over it would take every node down to launch
// replacements that cannot launch.
func imageDrift(
	nodeClaim *karpcorev1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) cloudprovider.DriftReason {
	launched := nodeClaim.Status.ImageID
	desired := lo.FirstOrEmpty(nodeClass.Status.Images).ID
	if launched == "" || desired == "" {
		return ""
	}
	return lo.Ternary(launched != desired, DriftReasonImage, "")
}

// subnetDrift asks whether the subnet the instance's interface was created in is still one the
// nodeclass selects. Membership, not equality: a launch picks one of the selected subnets, so every
// node is in a different subnet from some of its peers by design, and a subnet added to the set
// leaves every existing node exactly as valid as it was.
//
// An empty resolved set is not drift, for the same reason an empty image list is not: it is a
// resolution that found nothing rather than an instruction to move every node somewhere else.
func subnetDrift(
	nodeClaim *karpcorev1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) cloudprovider.DriftReason {
	recorded, ok := nodeClaim.Annotations[v1alpha1.AnnotationSubnet]
	if !ok || recorded == "" {
		return ""
	}
	desired := v1alpha1.SubnetIDs(nodeClass.Status.Subnets)
	if desired.Len() == 0 {
		return ""
	}
	return lo.Ternary(!desired.Has(recorded), DriftReasonSubnet, "")
}

// sshKeyDrift compares the keys copied into the instance at launch against the keys the nodeclass
// selects now. Both directions count: a key added has to reach the node, and a key removed has to
// stop reaching it, and a running instance offers no way to do either.
func sshKeyDrift(
	nodeClaim *karpcorev1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) cloudprovider.DriftReason {
	recorded, ok := nodeClaim.Annotations[v1alpha1.AnnotationSSHKeys]
	if !ok {
		return ""
	}
	desired := sets.New(v1alpha1.SSHKeyIDs(nodeClass.Status.SSHKeys)...)
	return lo.Ternary(!v1alpha1.ParseIDs(recorded).Equal(desired), DriftReasonSSHKey, "")
}

// ipPoolDrift compares the pools the instance's external addresses were allocated from at launch
// against the pools the nodeclass selects now. Both directions again: one ephemeral address is
// attached per selected pool at launch, and nothing attaches or detaches one afterwards.
func ipPoolDrift(
	nodeClaim *karpcorev1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) cloudprovider.DriftReason {
	recorded, ok := nodeClaim.Annotations[v1alpha1.AnnotationIPPools]
	if !ok {
		return ""
	}
	desired := sets.New(v1alpha1.IPPoolIDs(nodeClass.Status.IPPools)...)
	return lo.Ternary(!v1alpha1.ParseIDs(recorded).Equal(desired), DriftReasonIPPool, "")
}

// affinityGroupDrift reports the groups the nodeclass wants that this instance is not in.
//
// Additions only. A missing annotation therefore needs no special case: it is the empty set, and the
// empty set drifts exactly when something is desired, which is what makes a node launched before the
// feature existed drift once groups are configured and not before. A group leaving the desired set is
// not drift, because the affinity controller removes the membership in place.
func affinityGroupDrift(
	nodeClaim *karpcorev1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) cloudprovider.DriftReason {
	desired := v1alpha1.AffinityGroupIDs(nodeClass.Status.AffinityGroups)
	recorded := sets.New[string]()
	for _, ref := range v1alpha1.ParseAffinityGroups(
		nodeClaim.Annotations[v1alpha1.AnnotationAffinityGroups],
	) {
		recorded.Insert(ref.ID)
	}
	return lo.Ternary(desired.Difference(recorded).Len() > 0, DriftReasonAffinityGroup, "")
}
