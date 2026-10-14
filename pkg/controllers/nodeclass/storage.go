package nodeclass

import (
	"context"
	"fmt"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
)

func (c *Controller) resolveStorage(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) error {
	sets := nodeClass.Spec.Storage.DiskSets
	nodeClass.Status.Storage = nil
	resolved := &v1alpha1.StorageStatus{
		BootSizeBytes: nodeClass.Spec.Storage.Boot.Size.Value(),
		DiskSets:      make([]v1alpha1.DiskSetStatus, 0, len(sets)),
	}

	if err := nodeClass.Spec.Storage.Boot.ValidateSize(); err != nil {
		setInvalidStorageCondition(
			nodeClass, v1alpha1.ConditionReasonInvalidDiskSize, err.Error(),
		)
		return nil
	}
	for _, set := range sets {
		if err := set.ValidateSize(); err != nil {
			setInvalidStorageCondition(
				nodeClass, v1alpha1.ConditionReasonInvalidDiskSize, err.Error(),
			)
			return nil
		}
	}

	if total, max := nodeClass.Spec.Storage.Disks(), v1alpha1.MaxDisks(); total > max {
		setInvalidStorageCondition(
			nodeClass,
			v1alpha1.ConditionReasonTooManyDisks,
			fmt.Sprintf(
				"the layout asks for %d disks including the boot disk, and an instance can attach %d",
				total,
				max,
			),
		)
		return nil
	}

	if len(nodeClass.Status.Images) == 0 {
		setInvalidStorageCondition(
			nodeClass,
			v1alpha1.ConditionReasonDiskSourceNotFound,
			"the boot disk has no resolved boot image",
		)
		return nil
	}
	if err := nodeClass.Spec.Storage.Boot.ValidateSourceSize(
		nodeClass.Status.Images[0].SizeBytes,
	); err != nil {
		setInvalidStorageCondition(
			nodeClass,
			v1alpha1.ConditionReasonDiskSourceSizeMismatch,
			fmt.Sprintf("selected boot image: %v", err),
		)
		return nil
	}

	var unresolved []string
	var mismatched []string
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
			if !images.Complete() {
				unresolved = append(
					unresolved,
					fmt.Sprintf("diskSet %q: %s", set.Name, images.Message),
				)
				continue
			}
			status.Images = images.Resolved[:min(len(images.Resolved), maxStatusImages)]
			if err := set.ValidateSourceSize(status.Images[0].SizeBytes); err != nil {
				mismatched = append(mismatched, fmt.Sprintf("image: %v", err))
				continue
			}

		case len(set.SnapshotSelector) > 0:
			snapshots, err := c.snapshots.Resolve(ctx, set.SnapshotSelector)
			if err != nil {
				return fmt.Errorf("unable to retrieve snapshots for diskSet %q: %w", set.Name, err)
			}
			logStale(ctx, "diskset-snapshot", snapshots.Stale, snapshots.Age)
			if !snapshots.Complete() {
				unresolved = append(
					unresolved,
					fmt.Sprintf("diskSet %q: %s", set.Name, snapshots.Message),
				)
				continue
			}
			status.Snapshots = snapshots.Resolved[:min(len(snapshots.Resolved), maxStatusImages)]
			if err := set.ValidateSourceSize(status.Snapshots[0].SizeBytes); err != nil {
				mismatched = append(mismatched, fmt.Sprintf("snapshot: %v", err))
				continue
			}
		}
		resolved.DiskSets = append(resolved.DiskSets, status)
	}

	switch {
	case len(mismatched) > 0:
		setInvalidStorageCondition(
			nodeClass,
			v1alpha1.ConditionReasonDiskSourceSizeMismatch,
			strings.Join(mismatched, "; "),
		)
	case len(unresolved) > 0:
		setInvalidStorageCondition(
			nodeClass,
			v1alpha1.ConditionReasonDiskSourceNotFound,
			strings.Join(unresolved, "; "),
		)
	default:
		nodeClass.Status.Storage = resolved
		nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeStorageResolved)
	}
	return nil
}

func setInvalidStorageCondition(
	nodeClass *v1alpha1.OxideNodeClass,
	reason string,
	message string,
) {
	nodeClass.StatusConditions().SetFalse(
		v1alpha1.ConditionTypeStorageResolved,
		reason,
		message,
	)
}

func mountPoints(set v1alpha1.DiskSet) []string {
	if set.RawBlock() {
		return nil
	}
	if set.MountPath != "" {
		return []string{set.MountPath}
	}
	return targetsUnder(set)
}

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
