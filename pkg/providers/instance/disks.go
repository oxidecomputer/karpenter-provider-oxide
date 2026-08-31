package instance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (p *Provider) ensureDisks(
	ctx context.Context,
	body *oxide.InstanceCreate,
	owner ownership.Marker,
) error {
	specs := diskCreateSpecs(body)
	pre := make([]*oxide.InstanceDiskAttachmentCreate, 0, len(specs))
	for _, spec := range specs {
		if distributed(spec) {
			pre = append(pre, spec)
		}
	}
	if len(pre) == 0 {
		return nil
	}

	group, groupCtx := errgroup.WithContext(ctx)
	for _, spec := range pre {
		group.Go(func() error { return p.ensureDisk(groupCtx, spec, string(body.Name), owner) })
	}
	if err := group.Wait(); err != nil {
		return err
	}

	if spec, ok := diskCreateSpec(body.BootDisk); ok && distributed(spec) {
		body.BootDisk = oxide.InstanceDiskAttachment{
			Value: &oxide.InstanceDiskAttachmentAttach{Name: spec.Name},
		}
	}
	for i, attachment := range body.Disks {
		if spec, ok := diskCreateSpec(attachment); ok && distributed(spec) {
			body.Disks[i] = oxide.InstanceDiskAttachment{
				Value: &oxide.InstanceDiskAttachmentAttach{Name: spec.Name},
			}
		}
	}
	return nil
}

func (p *Provider) ensureDisk(
	ctx context.Context,
	spec *oxide.InstanceDiskAttachmentCreate,
	instanceName string,
	owner ownership.Marker,
) error {
	name := string(spec.Name)
	expected, err := expectedDiskMarker(spec.Description, owner)
	if err != nil {
		return err
	}

	for attempt := 0; ; attempt++ {
		log.FromContext(ctx).V(1).Info("Calling DiskCreate", "disk", spec.Name)
		created, err := p.disks.Create(ctx, &oxide.DiskCreate{
			Name:        spec.Name,
			Description: spec.Description,
			Size:        spec.Size,
			DiskBackend: spec.DiskBackend,
		})
		if err == nil {
			if err := ownership.Verify(created.Description, expected); err != nil {
				p.rejectOwnership(ctx, "adopt", "disk", created.Id, string(created.Name), err)
				return err
			}
			return p.waitForDiskReady(ctx, created)
		}
		log.FromContext(ctx).V(1).Info("Called DiskCreate", "disk", spec.Name)

		switch {
		case errors.Is(err, oxide.ErrInsufficientCapacity):
			return fmt.Errorf(
				"unable to create disk %s, %w, %w", name, ErrInsufficientStorage, err,
			)

		case ambiguous(ctx, err):
			existing, found := p.findDisk(ctx, name)
			if !found {
				return fmt.Errorf("unable to create disk %s, %w", name, err)
			}
			if err := ownership.Verify(existing.Description, expected); err != nil {
				p.rejectOwnership(ctx, "adopt", "disk", existing.Id, string(existing.Name), err)
				return err
			}
			return p.waitForDiskReady(ctx, existing)

		case errors.Is(err, oxide.ErrObjectAlreadyExists) && attempt == 0:
			replace, err := p.adoptDisk(ctx, spec, instanceName, expected)
			if err != nil {
				return err
			}
			if !replace {
				return nil
			}

		default:
			return fmt.Errorf("unable to create disk %s, %w", name, err)
		}
	}
}

func (p *Provider) adoptDisk(
	ctx context.Context,
	spec *oxide.InstanceDiskAttachmentCreate,
	instanceName string,
	expected ownership.Marker,
) (bool, error) {
	name := string(spec.Name)
	log.FromContext(ctx).V(1).Info("disk already exists, adopting it", "disk", name)

	existing, err := p.disks.View(ctx, name)
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {
			return false, fmt.Errorf(
				"disk %s already exists but was deleted before it could be adopted", name,
			)
		}
		return false, fmt.Errorf("unable to view existing disk %s, %w", name, err)
	}
	if err := ownership.Verify(existing.Description, expected); err != nil {
		p.rejectOwnership(ctx, "adopt", "disk", existing.Id, string(existing.Name), err)
		return false, err
	}

	if attached, ok := existing.State.AsAttached(); ok && attached.Instance != "" {
		if !p.instanceIs(ctx, attached.Instance, instanceName) {
			return false, fmt.Errorf(
				"disk %s already exists and is attached to instance %s rather than to %s, so it is not this launch's to adopt",
				name,
				attached.Instance,
				instanceName,
			)
		}
	}

	switch state := existing.State.State(); state {
	case oxide.DiskStateStateCreating,
		oxide.DiskStateStateDetached,
		oxide.DiskStateStateAttaching,
		oxide.DiskStateStateAttached:
		return false, p.waitForDiskReady(ctx, existing)

	default:
		if err := p.DeleteOwnedDisk(ctx, existing.Id, expected); err != nil {
			return false, fmt.Errorf(
				"disk %s already exists in state %q and could not be removed, %w",
				name, state, err,
			)
		}
		return true, nil
	}
}

func (p *Provider) waitForDiskReady(ctx context.Context, disk *oxide.Disk) error {
	started := time.Now()
	pollCtx, cancel := context.WithTimeout(ctx, p.deadlines.DiskCreating)
	defer cancel()

	current, err := p.disks.WaitAttachable(pollCtx, disk)
	if err == nil {
		return nil
	}
	if wait.Interrupted(err) {
		return &StuckLaunchResourceError{
			Kind:     KindDisk,
			ID:       disk.Id,
			Name:     string(disk.Name),
			State:    string(current.State.State()),
			StuckFor: time.Since(started),
		}
	}
	return err
}

func (p *Provider) abandonDisks(ctx context.Context, disks []OwnedDisk, cause error) {
	p.cleanup(ctx, func(cleanupCtx context.Context) {
		p.abandonDisksWithContext(cleanupCtx, disks, cause)
	})
}

func (p *Provider) abandonDisksWithContext(
	ctx context.Context,
	disks []OwnedDisk,
	cause error,
) {
	for _, disk := range disks {
		if disk.ID == "" {
			continue
		}
		if err := p.DeleteOwnedDisk(ctx, disk.ID, disk.Marker); err != nil {
			log.FromContext(ctx).Error(
				err,
				"unable to clean up a disk whose instance did not launch",
				"disk", disk.ID,
				"cause", cause.Error(),
			)
		}
	}
}

func (p *Provider) findDisk(ctx context.Context, name string) (*oxide.Disk, bool) {
	view, err := p.disks.View(ctx, name)
	if err != nil {
		if !errors.Is(err, oxide.ErrObjectNotFound) {
			log.FromContext(ctx).V(1).Info(
				"unable to look up a disk whose create gave no answer",
				"disk", name,
				"reason", err.Error(),
			)
		}
		return nil, false
	}
	return view, true
}

func (p *Provider) ownedAttachedDisks(
	ctx context.Context,
	instance *oxide.Instance,
	expected ownership.Marker,
) ([]OwnedDisk, error) {
	attached, err := p.disks.ListAttached(ctx, instance.Id)
	if err != nil {
		return nil, fmt.Errorf("unable to list disks attached to instance %s, %w", instance.Id, err)
	}
	var owned []OwnedDisk
	for _, disk := range attached {
		if poolDisk(disk.Description) {
			continue
		}
		marker, verifyErr := verifyNodeClaimDisk(disk.Description, expected)
		if verifyErr != nil {
			p.rejectOwnership(ctx, "cleanup", "disk", disk.Id, string(disk.Name), verifyErr)
			log.FromContext(ctx).Info(
				"leaving an attached disk intact because its ownership could not be verified",
				"disk-id", disk.Id,
				"disk", disk.Name,
				"instance-id", instance.Id,
			)
			continue
		}
		owned = appendOwnedDisks(owned, OwnedDisk{ID: disk.Id, Marker: marker})
	}
	return owned, nil
}

func (p *Provider) deleteOwnedDisks(ctx context.Context, disks []OwnedDisk) error {
	var errs []error
	for _, disk := range disks {
		if err := p.DeleteOwnedDisk(ctx, disk.ID, disk.Marker); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DeleteOwnedDisk re-reads and verifies a disk immediately before deleting it by immutable ID.
func (p *Provider) DeleteOwnedDisk(
	ctx context.Context,
	ref string,
	expected ownership.Marker,
) error {
	if ref == "" {
		return nil
	}

	return p.disks.DeleteOwnedEventually(ctx, ref, func(disk *oxide.Disk) error {
		err := ownership.Verify(disk.Description, expected)
		if err != nil {
			p.rejectOwnership(ctx, "delete", "disk", disk.Id, string(disk.Name), err)
		}
		return err
	})
}

func diskNames(body *oxide.InstanceCreate) []string {
	specs := diskCreateSpecs(body)
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, string(spec.Name))
	}
	return names
}

func (p *Provider) ownedTeardownDisks(
	ctx context.Context,
	teardown Teardown,
) ([]OwnedDisk, error) {
	var candidates []oxide.Disk
	if teardown.BootDiskName != "" {
		disk, err := p.disks.View(ctx, teardown.BootDiskName)
		if err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
			return nil, fmt.Errorf("unable to view boot disk candidate %s, %w", teardown.BootDiskName, err)
		}
		if err == nil {
			candidates = append(candidates, *disk)
		}
	}
	if teardown.DiskPrefix == "" {
		return p.verifyDiskCandidates(ctx, candidates, teardown.Owner), nil
	}
	disks, err := p.disks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to list disk candidates with prefix %s, %w", teardown.DiskPrefix, err)
	}
	for _, disk := range disks {
		if !strings.HasPrefix(string(disk.Name), teardown.DiskPrefix) {
			continue
		}
		candidates = append(candidates, disk)
	}
	return p.verifyDiskCandidates(ctx, candidates, teardown.Owner), nil
}

func (p *Provider) verifyDiskCandidates(
	ctx context.Context,
	candidates []oxide.Disk,
	expected ownership.Marker,
) []OwnedDisk {
	var owned []OwnedDisk
	for i := range candidates {
		disk := &candidates[i]
		if poolDisk(disk.Description) {
			continue
		}
		marker, err := verifyNodeClaimDisk(disk.Description, expected)
		if err != nil {
			p.rejectOwnership(ctx, "cleanup", "disk", disk.Id, string(disk.Name), err)
			continue
		}
		owned = appendOwnedDisks(owned, OwnedDisk{ID: disk.Id, Marker: marker})
	}
	return owned
}

func poolDisk(description string) bool {
	_, ok := ownership.ParsePool(description)
	return ok
}

func plannedOwnedDisks(
	body *oxide.InstanceCreate,
	owner ownership.Marker,
) ([]OwnedDisk, error) {
	var owned []OwnedDisk
	for _, spec := range diskCreateSpecs(body) {
		marker, err := expectedDiskMarker(spec.Description, owner)
		if err != nil {
			return nil, err
		}
		owned = appendOwnedDisks(owned, OwnedDisk{ID: string(spec.Name), Marker: marker})
	}
	return owned, nil
}

func expectedDiskMarker(description string, owner ownership.Marker) (ownership.Marker, error) {
	return verifyNodeClaimDisk(description, owner)
}

func verifyNodeClaimDisk(
	description string,
	owner ownership.Marker,
) (ownership.Marker, error) {
	marker, ok := ownership.Parse(description)
	if !ok {
		expected := owner
		expected.Kind = ownership.KindBootDisk
		return ownership.Marker{}, ownership.Verify(description, expected)
	}
	if marker.Kind != ownership.KindBootDisk && marker.Kind != ownership.KindDataDisk {
		return ownership.Marker{}, &ownership.ErrOwnershipConflict{
			Reason: ownership.ReasonWrongKind, Expected: owner, Observed: marker,
		}
	}
	expected := owner
	expected.Kind = marker.Kind
	if err := ownership.Verify(description, expected); err != nil {
		return ownership.Marker{}, err
	}
	return marker, nil
}

func appendOwnedDisks(dst []OwnedDisk, disks ...OwnedDisk) []OwnedDisk {
	seen := make(map[string]struct{}, len(dst)+len(disks))
	for _, disk := range dst {
		seen[disk.ID] = struct{}{}
	}
	for _, disk := range disks {
		if disk.ID == "" {
			continue
		}
		if _, ok := seen[disk.ID]; ok {
			continue
		}
		seen[disk.ID] = struct{}{}
		dst = append(dst, disk)
	}
	return dst
}

func diskIDs(disks []OwnedDisk) []string {
	ids := make([]string, 0, len(disks))
	for _, disk := range disks {
		ids = append(ids, disk.ID)
	}
	return ids
}

func diskCreateSpecs(body *oxide.InstanceCreate) []*oxide.InstanceDiskAttachmentCreate {
	var specs []*oxide.InstanceDiskAttachmentCreate
	if spec, ok := diskCreateSpec(body.BootDisk); ok {
		specs = append(specs, spec)
	}
	for _, attachment := range body.Disks {
		if spec, ok := diskCreateSpec(attachment); ok {
			specs = append(specs, spec)
		}
	}
	return specs
}

func diskCreateSpec(
	attachment oxide.InstanceDiskAttachment,
) (*oxide.InstanceDiskAttachmentCreate, bool) {
	switch spec := attachment.Value.(type) {
	case *oxide.InstanceDiskAttachmentCreate:
		return spec, true
	case oxide.InstanceDiskAttachmentCreate:
		return &spec, true
	default:
		return nil, false
	}
}

func distributed(spec *oxide.InstanceDiskAttachmentCreate) bool {
	_, local := spec.DiskBackend.AsLocal()
	return !local
}

func (p *Provider) instanceIs(ctx context.Context, id, name string) bool {
	if id == "" || name == "" {
		return false
	}
	view, err := p.ox.InstanceView(ctx, oxide.InstanceViewParams{Instance: oxide.NameOrId(id)})
	if err != nil {
		log.FromContext(ctx).V(1).Info(
			"unable to view the instance a disk is attached to, so the disk is not adopted",
			"instance-id", id,
			"reason", err.Error(),
		)
		return false
	}
	return string(view.Name) == name
}
