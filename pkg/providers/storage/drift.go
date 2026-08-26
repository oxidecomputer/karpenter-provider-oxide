package storage

import (
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Drifter decides whether a node's data disks were created from sources the nodeclass would no longer
// create them from.
//
// It is a type of its own rather than a method on anything, because this package holds no client and
// needs none: the comparison is between what the NodeClaim recorded at launch and what the nodeclass
// status resolves to now, both of which are already in hand.
type Drifter struct{}

// NewDrifter returns the disk set source drifter.
func NewDrifter() Drifter { return Drifter{} }

// DriftReason reports whether the disks of any diskSet came from a source the nodeclass has since
// moved off.
//
// The comparison is per set, and only over sets that still exist: a set removed from the spec has
// already moved the nodeclass hash, so it is reported as a nodeclass change rather than twice.
//
// Two cases deliberately do not drift. A NodeClaim with no record at all was launched before this
// provider kept one, and comparing against nothing would replace a fleet on an upgrade of Karpenter
// itself. A set whose source has not resolved reports nothing either, because replacing a node then
// would launch a replacement that cannot launch -- the same failure the image drifter guards against.
func (Drifter) DriftReason(
	nodeClaim *karpv1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) karpcloudprovider.DriftReason {
	recorded, found := nodeClaim.Annotations[v1alpha1.AnnotationDiskSetSources]
	if !found {
		return ""
	}
	was := v1alpha1.ParseDiskSetSources(recorded)
	now := v1alpha1.DiskSetSources(nodeClass.Status.Storage)

	for _, set := range nodeClass.Spec.Storage.DiskSets {
		// Only a sourced set can drift this way. A set of blank disks has nothing that could have
		// moved, and its geometry is covered by the nodeclass hash.
		if !set.Sourced() {
			continue
		}
		current, resolved := now[set.Name]
		if !resolved || current == "" {
			continue
		}
		previous, recordedForSet := was[set.Name]
		if !recordedForSet {
			// The set was added after this node launched. Adding one changes the spec, so the
			// nodeclass hash has already reported it.
			continue
		}
		if previous != current {
			return providers.DriftReasonDiskSetSource
		}
	}
	return ""
}
