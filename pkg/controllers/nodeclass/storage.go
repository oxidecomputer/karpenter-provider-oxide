package nodeclass

import (
	"context"
	"fmt"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/image"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/snapshot"
)

func (c *Controller) resolveStorage(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) error {
	sets := nodeClass.Spec.Storage.DiskSets
	resolved := &v1alpha1.StorageStatus{
		BootSizeBytes: nodeClass.Spec.Storage.Boot.Size.Value(),
		DiskSets:      make([]v1alpha1.DiskSetStatus, 0, len(sets)),
	}

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
