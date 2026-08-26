package storage

import (
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Drifter decides whether a node's data disks were created from sources the nodeclass would no longer create them from.
type Drifter struct{}

// NewDrifter returns the disk set source drifter.
func NewDrifter() Drifter { return Drifter{} }

// DriftReason reports whether the disks of any diskSet came from a source the nodeclass has since moved off.
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

		if !set.Sourced() {
			continue
		}
		current, resolved := now[set.Name]
		if !resolved || current == "" {
			continue
		}
		previous, recordedForSet := was[set.Name]
		if !recordedForSet {

			continue
		}
		if previous != current {
			return providers.DriftReasonDiskSetSource
		}
	}
	return ""
}
