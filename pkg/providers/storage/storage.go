// Package storage plans attached disks.
package storage

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
)

const (
	diskPrefix = "kd-"

	fingerprintLength = 12

	serialLength = 20

	localBlockSize = 4096
)

// ErrTooManyDisks is a layout asking for more disks than an instance can attach.
var ErrTooManyDisks = errors.New("too many disks")

// Disk is one disk a launch will create.
type Disk struct {
	// Name is the Oxide disk name.
	Name string
	// Serial is the guest-visible NVMe serial number.
	Serial string
	// Set is the diskSet this disk belongs to, empty for the boot disk.
	Set string
	// Index is the flat index across a launch's disks.
	Index int
	// Ordinal is the disk's position within its set, counting from zero.
	Ordinal int32
	Boot    bool
	// Local marks sled-local storage.
	Local     bool
	ReadOnly  bool
	SizeBytes int64
	// Filesystem is what the guest formats this disk with.
	Filesystem string
	// MountPoint is empty for raw block disks.
	MountPoint string
	// BlockLink is where a raw block disk's device is symlinked, or empty otherwise.
	BlockLink string
	// ImageID and SnapshotID are the resolved source, at most one of which is set.
	ImageID    string
	SnapshotID string
}

// Set is one resolved diskSet, holding the disks it expands to.
type Set struct {
	Name       string
	Type       string
	Layout     string
	Filesystem string
	Disks      []Disk
}

// Plan is every disk one launch creates.
type Plan struct {
	InstanceName string
	// Prefix is what every disk of this launch, boot disk included, is named with.
	Prefix string
	Boot   Disk
	Sets   []Set
}

// Build resolves a nodeclass, an instance name and a launch token into the disks a launch creates.
func Build(instanceName, token string, nodeClass *v1alpha1.OxideNodeClass) (Plan, error) {
	if instanceName == "" {
		return Plan{}, errors.New("an instance name is required to plan disks")
	}
	if token == "" {
		return Plan{}, errors.New("a launch token is required to plan disks")
	}
	spec := nodeClass.Spec.Storage

	if total, max := spec.Disks(), v1alpha1.MaxDisks(); total > max {
		return Plan{}, fmt.Errorf(
			"%w: the layout asks for %d disks including the boot disk, and an instance can attach %d",
			ErrTooManyDisks,
			total,
			max,
		)
	}

	prefix := Prefix(instanceName, token)
	bootName := bootDiskName(prefix)
	plan := Plan{
		InstanceName: instanceName,
		Prefix:       prefix,
		Boot: Disk{

			Name:      bootName,
			Serial:    Serial(bootName),
			Index:     0,
			Boot:      true,
			SizeBytes: spec.Boot.Size.Value(),

			ImageID: lo.FirstOrEmpty(nodeClass.Status.Images).ID,
		},
	}

	index := 0
	for _, set := range spec.DiskSets {
		resolved, err := buildSet(&plan, nodeClass, set, &index)
		if err != nil {
			return Plan{}, err
		}
		plan.Sets = append(plan.Sets, resolved)
	}

	if err := checkSerials(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func buildSet(
	plan *Plan,
	nodeClass *v1alpha1.OxideNodeClass,
	set v1alpha1.DiskSet,
	index *int,
) (Set, error) {
	rawBlock := set.RawBlock()

	if err := checkMountFields(set); err != nil {
		return Set{}, err
	}

	imageID, snapshotID, err := source(nodeClass, set)
	if err != nil {
		return Set{}, err
	}

	resolved := Set{
		Name:       set.Name,
		Type:       set.EffectiveType(),
		Layout:     set.EffectiveLayout(),
		Filesystem: set.EffectiveFilesystem(),
	}

	for ordinal := int32(0); ordinal < set.EffectiveCount(); ordinal++ {
		*index++
		name := diskName(plan.Prefix, *index, set.Name, ordinal)
		disk := Disk{
			Name:       name,
			Serial:     Serial(name),
			Set:        set.Name,
			Index:      *index,
			Ordinal:    ordinal,
			Local:      set.EffectiveType() == v1alpha1.DiskSetTypeLocal,
			ReadOnly:   set.ReadOnly != nil && *set.ReadOnly,
			SizeBytes:  set.Size.Value(),
			Filesystem: set.EffectiveFilesystem(),
			ImageID:    imageID,
			SnapshotID: snapshotID,
		}
		switch {
		case rawBlock:

			disk.BlockLink = path.Join(set.MountDir, fmt.Sprintf("%s-%d", set.Name, ordinal))
		case set.MountPath != "":
			disk.MountPoint = set.MountPath
		default:
			disk.MountPoint = path.Join(set.MountDir, fmt.Sprintf("%s-%d", set.Name, ordinal))
		}
		resolved.Disks = append(resolved.Disks, disk)
	}
	return resolved, nil
}

func source(nodeClass *v1alpha1.OxideNodeClass, set v1alpha1.DiskSet) (string, string, error) {
	if !set.Sourced() {
		return "", "", nil
	}
	status, ok := lo.Find(diskSetStatuses(nodeClass), func(s v1alpha1.DiskSetStatus) bool {
		return s.Name == set.Name
	})
	if !ok {
		return "", "", fmt.Errorf(
			"diskSet %q selects an image or a snapshot that has not resolved yet", set.Name,
		)
	}

	var id string
	var sourceSize int64
	var kind string
	switch {
	case len(set.ImageSelector) > 0:
		image, found := lo.First(status.Images)
		if !found {
			return "", "", fmt.Errorf("diskSet %q: no image matched its selector", set.Name)
		}
		id, sourceSize, kind = image.ID, image.SizeBytes, "image"
	default:
		snapshot, found := lo.First(status.Snapshots)
		if !found {
			return "", "", fmt.Errorf("diskSet %q: no snapshot matched its selector", set.Name)
		}
		id, sourceSize, kind = snapshot.ID, snapshot.SizeBytes, "snapshot"
	}

	if size := set.Size.Value(); sourceSize > 0 && size < sourceSize {
		return "", "", fmt.Errorf(
			"diskSet %q asks for %d bytes per disk, which is smaller than the %s it is created from at %d bytes",
			set.Name,
			size,
			kind,
			sourceSize,
		)
	}
	if len(set.ImageSelector) > 0 {
		return id, "", nil
	}
	return "", id, nil
}

func diskSetStatuses(nodeClass *v1alpha1.OxideNodeClass) []v1alpha1.DiskSetStatus {
	if nodeClass.Status.Storage == nil {
		return nil
	}
	return nodeClass.Status.Storage.DiskSets
}

func checkMountFields(set v1alpha1.DiskSet) error {
	if (set.MountPath == "") == (set.MountDir == "") {
		return fmt.Errorf(
			"diskSet %q must set exactly one of mountPath or mountDir", set.Name,
		)
	}
	target, field := set.MountPath, "mountPath"
	if target == "" {
		target, field = set.MountDir, "mountDir"
	}
	if !path.IsAbs(target) {
		return fmt.Errorf("diskSet %q: %s %q must be an absolute path", set.Name, field, target)
	}
	if path.Clean(target) != target {
		return fmt.Errorf(
			"diskSet %q: %s %q is not a clean path, %q was expected",
			set.Name, field, target, path.Clean(target),
		)
	}

	if i := strings.IndexAny(target, "'\"\\\n\r\t "); i >= 0 {
		return fmt.Errorf(
			"diskSet %q: %s %q contains %q, which is not allowed in a mount target",
			set.Name, field, target, target[i],
		)
	}
	return nil
}

func checkSerials(plan Plan) error {
	seen := map[string]string{plan.Boot.Serial: plan.Boot.Name}
	for _, set := range plan.Sets {
		for _, disk := range set.Disks {
			if other, ok := seen[disk.Serial]; ok {
				return fmt.Errorf(
					"disks %q and %q would both present the guest the NVMe serial %q, and the guest kernel would reject one of them",
					other,
					disk.Name,
					disk.Serial,
				)
			}
			seen[disk.Serial] = disk.Name
		}
	}
	return nil
}

// Prefix is what every disk of one launch is named with.
func Prefix(instanceName, token string) string {
	return diskPrefix + fingerprint(instanceName+"\x00"+token) + "-"
}

// LegacyPrefix is the prefix disks were named with before a launch had a token.
func LegacyPrefix(instanceName string) string {
	return diskPrefix + fingerprint(instanceName) + "-"
}

func bootDiskName(prefix string) string {
	return prefix + "0-boot"
}

// Serial is what the guest sees of a disk name: its first serialLength bytes.
func Serial(name string) string {
	if len(name) <= serialLength {
		return name
	}
	return name[:serialLength]
}

func diskName(prefix string, index int, setName string, ordinal int32) string {
	return fmt.Sprintf("%s%d-%s-%d", prefix, index, setName, ordinal)
}

func fingerprint(launch string) string {
	sum := sha256.Sum256([]byte(launch))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return strings.ToLower(encoded)[:fingerprintLength]
}

// Data is every data disk in the plan, flattened out of its sets in spec order.
func (p Plan) Data() []Disk {
	disks := make([]Disk, 0, len(p.Sets))
	for _, set := range p.Sets {
		disks = append(disks, set.Disks...)
	}
	return disks
}

// All is every disk the launch creates, the boot disk first.
func (p Plan) All() []Disk {
	return append([]Disk{p.Boot}, p.Data()...)
}

// Names is every disk name the launch owns.
func (p Plan) Names() []string {
	return lo.Map(p.All(), func(disk Disk, _ int) string { return disk.Name })
}

// DistributedBytes is the storage this launch would add to the silo's provisioned total.
func (p Plan) DistributedBytes() int64 {
	total := p.Boot.SizeBytes
	for _, disk := range p.Data() {
		if !disk.Local {
			total += disk.SizeBytes
		}
	}
	return total
}

// MountOrder returns disks with parent paths first.
func (p Plan) MountOrder() []Disk {
	var mounted []Disk
	for _, set := range p.Sets {
		for _, disk := range set.Disks {
			if disk.MountPoint != "" {
				mounted = append(mounted, disk)
			}
		}
	}
	slices.SortStableFunc(mounted, func(a, b Disk) int {
		return strings.Compare(a.MountPoint, b.MountPoint)
	})
	return mounted
}

// BlockLinks is every raw block disk in the plan, in set and ordinal order.
func (p Plan) BlockLinks() []Disk {
	var links []Disk
	for _, set := range p.Sets {
		for _, disk := range set.Disks {
			if disk.BlockLink != "" {
				links = append(links, disk)
			}
		}
	}
	return links
}

// BootAttachment is the boot disk as an instance create asks for it.
func (p Plan) BootAttachment(description string) oxide.InstanceDiskAttachment {
	return oxide.InstanceDiskAttachment{Value: attachmentCreate(p.Boot, description)}
}

// DataAttachments is every data disk as an instance create asks for it.
func (p Plan) DataAttachments(description func(Disk) string) []oxide.InstanceDiskAttachment {
	data := p.Data()
	attachments := make([]oxide.InstanceDiskAttachment, 0, len(data))
	for _, disk := range data {
		attachments = append(attachments, oxide.InstanceDiskAttachment{
			Value: attachmentCreate(disk, description(disk)),
		})
	}
	return attachments
}

func attachmentCreate(disk Disk, description string) *oxide.InstanceDiskAttachmentCreate {
	return &oxide.InstanceDiskAttachmentCreate{
		Name:        oxide.Name(disk.Name),
		Description: description,
		Size:        oxide.ByteCount(disk.SizeBytes),
		DiskBackend: backend(disk),
	}
}

func backend(disk Disk) oxide.DiskBackend {
	if disk.Local {
		return oxide.DiskBackend{Value: &oxide.DiskBackendLocal{}}
	}
	var src oxide.DiskSource
	switch {
	case disk.SnapshotID != "":
		src = oxide.DiskSource{Value: &oxide.DiskSourceSnapshot{
			SnapshotId: disk.SnapshotID,
			ReadOnly:   readOnly(disk),
		}}
	case disk.Boot || disk.ImageID != "":
		src = oxide.DiskSource{Value: &oxide.DiskSourceImage{
			ImageId:  disk.ImageID,
			ReadOnly: readOnly(disk),
		}}
	default:
		src = oxide.DiskSource{Value: &oxide.DiskSourceBlank{BlockSize: localBlockSize}}
	}
	return oxide.DiskBackend{Value: &oxide.DiskBackendDistributed{DiskSource: src}}
}

func readOnly(disk Disk) *bool {
	if !disk.ReadOnly {
		return nil
	}
	return new(true)
}
