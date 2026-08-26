package nodeclass

import (
	"context"
	"fmt"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/image"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/snapshot"
)

// resolveStorage publishes the nodeclass's resolved disk layout and sets ConditionTypeStorageResolved.
//
// The geometry -- sizes, counts, mount points, the effective filesystem -- is published
// unconditionally, because it needs no API call and is worth having on the object even when a
// selector fails. The sources are not: on an incomplete resolution the previously published layout is
// left exactly as it was, following AffinityGroups rather than Images. Status.Storage is what a launch
// reads to decide which image or snapshot a data disk is created from, so replacing it with a partial
// set would launch disks from the wrong source, or from none.
func (c *Controller) resolveStorage(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) error {
	sets := nodeClass.Spec.Storage.DiskSets
	resolved := &v1alpha1.StorageStatus{
		BootSizeBytes: nodeClass.Spec.Storage.Boot.Size.Value(),
		DiskSets:      make([]v1alpha1.DiskSetStatus, 0, len(sets)),
	}

	// Reported rather than only refused by CEL, for the nodeclass the apiserver has not re-validated
	// since the rule shipped. A launch would refuse it too, but with nothing on the nodeclass to say
	// why no node ever appears.
	if total, max := nodeClass.Spec.Storage.Disks(), v1alpha1.MaxDisks(); total > max {
		nodeClass.StatusConditions().SetFalse(
			v1alpha1.ConditionTypeStorageResolved,
			v1alpha1.ConditionReasonTooManyDisks,
			fmt.Sprintf(
				"the layout asks for %d disks including the boot disk, and an instance can attach %d",
				total,
				max,
			),
		)
		return nil
	}

	var unresolved []string
	var tooSmall []string
	for _, set := range sets {
		status := v1alpha1.DiskSetStatus{
			Name:        set.Name,
			Type:        set.EffectiveType(),
			Count:       set.EffectiveCount(),
			SizeBytes:   set.Size.Value(),
			Layout:      set.EffectiveLayout(),
			Filesystem:  set.EffectiveFilesystem(),
			MountPoints: mountPoints(set),
			BlockLinks:  blockLinks(set),
		}

		switch {
		case len(set.ImageSelector) > 0:
			images, err := c.images.ResolveSelectors(ctx, set.ImageSelector)
			if err != nil {
				return fmt.Errorf("unable to retrieve images for diskSet %q: %w", set.Name, err)
			}
			logStale(ctx, "diskset-image", images.Stale, images.Age)
			if len(images.Resolved) == 0 {
				unresolved = append(unresolved, fmt.Sprintf(
					"diskSet %q: %s", set.Name, image.DescribeSelectors(images.Unmatched),
				))
				continue
			}
			status.Images = images.Resolved[:min(len(images.Resolved), maxStatusImages)]
			if reason, ok := tooSmallFor(set, status.Images[0].SizeBytes, "image"); !ok {
				tooSmall = append(tooSmall, reason)
				continue
			}

		case len(set.SnapshotSelector) > 0:
			snapshots, err := c.snapshots.Resolve(ctx, set.SnapshotSelector)
			if err != nil {
				return fmt.Errorf("unable to retrieve snapshots for diskSet %q: %w", set.Name, err)
			}
			logStale(ctx, "diskset-snapshot", snapshots.Stale, snapshots.Age)
			if len(snapshots.Resolved) == 0 {
				unresolved = append(unresolved, fmt.Sprintf(
					"diskSet %q: %s", set.Name, snapshot.DescribeSelectors(snapshots.Unmatched),
				))
				continue
			}
			status.Snapshots = snapshots.Resolved[:min(len(snapshots.Resolved), maxStatusImages)]
			if reason, ok := tooSmallFor(set, status.Snapshots[0].SizeBytes, "snapshot"); !ok {
				tooSmall = append(tooSmall, reason)
				continue
			}
		}
		resolved.DiskSets = append(resolved.DiskSets, status)
	}

	// The condition is only registered on a nodeclass with a sourced diskSet, so there is nothing to
	// set for one whose data disks are all blank. The geometry is still published.
	if !nodeClass.Spec.Storage.NeedsSourceResolution() {
		nodeClass.Status.Storage = resolved
		return nil
	}

	switch {
	case len(tooSmall) > 0:
		nodeClass.StatusConditions().SetFalse(
			v1alpha1.ConditionTypeStorageResolved,
			v1alpha1.ConditionReasonDiskTooSmall,
			strings.Join(tooSmall, "; "),
		)
	case len(unresolved) > 0:
		nodeClass.StatusConditions().SetFalse(
			v1alpha1.ConditionTypeStorageResolved,
			v1alpha1.ConditionReasonDiskSourceNotFound,
			strings.Join(unresolved, "; "),
		)
	default:
		nodeClass.Status.Storage = resolved
		nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeStorageResolved)
	}
	return nil
}

// tooSmallFor reports whether a set's disks are large enough for the source they are created from,
// and the message to publish when they are not.
//
// A source whose size is unknown is not checked: a zero here means the resolution did not carry one,
// not that the source is empty, and refusing a launch over a missing figure would be worse than
// letting the Oxide API be the authority.
func tooSmallFor(set v1alpha1.DiskSet, sourceBytes int64, kind string) (string, bool) {
	if sourceBytes <= 0 || set.Size.Value() >= sourceBytes {
		return "", true
	}
	return fmt.Sprintf(
		"diskSet %q asks for %d bytes per disk, which is smaller than the %s it is created from at %d bytes",
		set.Name,
		set.Size.Value(),
		kind,
		sourceBytes,
	), false
}

// mountPoints is where each of a set's disks is mounted, in ordinal order.
//
// It mirrors what pkg/providers/storage computes for a launch. Duplicated deliberately rather than
// shared: the launch's version needs an instance name to mint disk names, which a reconcile of the
// nodeclass does not have, and the mount geometry is the half that does not depend on it. The naming
// half, which is the part that would be dangerous to have two copies of, is not duplicated.
func mountPoints(set v1alpha1.DiskSet) []string {
	if set.RawBlock() {
		return nil
	}
	if set.MountPath != "" {
		return []string{set.MountPath}
	}
	return targetsUnder(set)
}

// blockLinks is where each of a raw block set's devices is linked, in ordinal order.
func blockLinks(set v1alpha1.DiskSet) []string {
	if !set.RawBlock() {
		return nil
	}
	return targetsUnder(set)
}

func targetsUnder(set v1alpha1.DiskSet) []string {
	targets := make([]string, 0, set.EffectiveCount())
	for ordinal := int32(0); ordinal < set.EffectiveCount(); ordinal++ {
		targets = append(targets, fmt.Sprintf("%s/%s-%d", set.MountDir, set.Name, ordinal))
	}
	return targets
}
