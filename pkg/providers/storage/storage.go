// Package storage turns an OxideNodeClass's storage spec, its resolved status and one instance name
// into the concrete disks a launch creates.
//
// It is pure: no Oxide client, no API calls, no cache. That matters because it is the single place a
// disk name is minted. A disk's name is the only handle the guest has on it -- Nexus sets the
// emulated NVMe controller's serial number to the first 20 bytes of the name -- so the launch that
// creates the disks and the user-data that tells the guest which is which have to agree exactly. Two
// copies of the naming rule is the one bug that would break serial matching in production while every
// unit test passed, so there is one copy and everything reads from it.
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
	// dataPrefix marks a disk as a Karpenter data disk, as opposed to a boot disk, which keeps the
	// instance's own name.
	dataPrefix = "kd-"

	// fingerprintLength is how many base32 characters of the instance name's digest go into a data
	// disk name. Twelve characters is 60 bits, which over the instance names alive in a project at
	// once is a collision probability of about N^2/2^61: four in a hundred billion at ten thousand
	// instances. It is also fixed width, which is the load-bearing part -- see diskName.
	fingerprintLength = 12

	// serialLength is how much of a disk name the guest sees. Nexus passes the first 20 bytes of the
	// name to Propolis as the NVMe controller's serial number, and the serial derives the NVMe
	// subsystem NQN, so two disks on one instance whose names agree in their first 20 bytes make the
	// guest kernel reject one of them.
	serialLength = 20

	// localBlockSize is the block size a local disk is fixed at by the Oxide API. Blank distributed
	// disks are created with the same size so that a set does not change shape when its type does.
	localBlockSize = 4096
)

// ErrTooManyDisks is a layout asking for more disks than an instance can attach.
var ErrTooManyDisks = errors.New("too many disks")

// Disk is one disk a launch will create.
type Disk struct {
	// Name is the Oxide disk name.
	Name string
	// Serial is what the guest sees as this disk's NVMe serial number, and the only deterministic
	// handle it has on the disk. Always exactly serialLength bytes: the minimum length of a diskSet
	// name is what guarantees a data disk name is at least that long, so the serial is never padded
	// and the guest never has to strip padding to match it.
	Serial string
	// Set is the diskSet this disk belongs to, empty for the boot disk.
	Set string
	// Index is the flat index across a launch's disks. Zero is the boot disk; data disks are 1 and up,
	// and this is the byte that makes their names unique within the instance.
	Index int
	// Ordinal is the disk's position within its set, counting from zero.
	Ordinal int32
	Boot    bool
	// Local is whether the disk is sled-local rather than distributed. It decides both the Oxide disk
	// backend and, at launch, whether the disk is created ahead of the instance or by it.
	Local     bool
	ReadOnly  bool
	SizeBytes int64
	// Filesystem is what the guest formats this disk with. Empty means format nothing, which is what
	// a disk created from an image or a snapshot needs. v1alpha1.FilesystemNone means leave it a raw
	// block device.
	Filesystem string
	// MountPoint is where the guest mounts this disk, or empty for a raw block disk, which is not
	// mounted in its own right.
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
	// DataPrefix is the prefix every data disk of this launch was named with. It is derived from the
	// instance name alone, which is what lets a teardown find disks a launch created but never
	// attached -- something no attachment listing can report.
	DataPrefix string
	Boot       Disk
	Sets       []Set
}

// Build resolves a nodeclass and an instance name into the disks a launch creates.
//
// It fails rather than launching something wrong. Most of what it checks is also refused by CEL on
// the CRD, and it checks again anyway: a nodeclass stored before a rule shipped is not re-validated
// by the apiserver until it is next written, so the launch is the only place left to catch it.
func Build(instanceName string, nodeClass *v1alpha1.OxideNodeClass) (Plan, error) {
	if instanceName == "" {
		return Plan{}, errors.New("an instance name is required to plan disks")
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

	plan := Plan{
		InstanceName: instanceName,
		DataPrefix:   DataPrefix(instanceName),
		Boot: Disk{
			// The boot disk keeps the instance's own name. It is the one disk name an operator can
			// read and tie to a node without a hash table, its serial cannot collide with a data
			// disk's because those all start "kd-", and renaming it would mean carrying the old name
			// through teardown forever: the name is the only handle left on the disk once the
			// instance is gone and can no longer be asked what it booted from.
			Name:      instanceName,
			Serial:    Serial(instanceName),
			Index:     0,
			Boot:      true,
			SizeBytes: spec.Boot.Size.Value(),
			// Images is empty when no selector matched. Launch with an empty image and let the Oxide
			// API reject it, which is what happened when an unmatched selector still left a
			// zero-valued entry here.
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
		name := diskName(plan.InstanceName, *index, set.Name, ordinal)
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
			// Always beneath the directory, including for a set of one. The local static provisioner
			// discovers the contents of its hostDir, so a hostDir that is itself a symlink to a
			// device discovers nothing. CEL forbids mountPath here so there is no other case.
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

// source returns the resolved image or snapshot a set's disks are created from, and refuses a set
// whose selector resolved to nothing rather than launching a disk with an empty source.
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

	// Refused rather than rounded up. A disk silently larger than the nodeclass asked for is a
	// storage bill nobody agreed to, and the Oxide API would reject the create anyway with nothing on
	// the nodeclass to explain it.
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

// checkMountFields refuses a mount target that would not survive being interpolated into the
// generated shell and JSON. Rejecting these here is what lets the renderer print them as literals
// rather than escaping at every use, and CEL already refuses most of them on the way in.
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
	// A space is refused along with the quoting characters. It survives the shell, but /etc/fstab
	// separates its fields on whitespace and would need it written as \040, and a mount point spelled
	// one way in the table and another in fstab is worse than a nodeclass that is refused outright.
	if i := strings.IndexAny(target, "'\"\\\n\r\t "); i >= 0 {
		return fmt.Errorf(
			"diskSet %q: %s %q contains %q, which is not allowed in a mount target",
			set.Name, field, target, target[i],
		)
	}
	return nil
}

// checkSerials refuses a plan in which two disks would present the guest the same NVMe serial.
//
// This is the failure this whole package's naming scheme exists to prevent, so it is asserted rather
// than assumed. The guest kernel rejects the second of two disks whose serials collide, which means a
// node that boots one disk short with nothing in the launch to explain it. Caught here, it is an
// error on the launch instead.
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

// DataPrefix is the prefix every data disk of a launch is named with. Derived from the instance name
// alone, so a teardown that has nothing but the NodeClaim name can still find them.
func DataPrefix(instanceName string) string {
	return dataPrefix + fingerprint(instanceName) + "-"
}

// Serial is what the guest sees of a disk name: its first serialLength bytes.
func Serial(name string) string {
	if len(name) <= serialLength {
		return name
	}
	return name[:serialLength]
}

// diskName builds a data disk's name.
//
//	kd-<fingerprint>-<index>-<set>-<ordinal>
//	│  │             │       │     └ ordinal within the set, for readability and the mount directory
//	│  │             │       └────── diskSet name, for readability
//	│  │             └────────────── flat index across the launch's data disks, 1..7
//	│  └──────────────────────────── 12 fixed-width base32 characters of the instance name's digest
//	└─────────────────────────────── marks a Karpenter data disk
//
// The fingerprint being fixed width is the whole point: everything before the index is a constant
// number of bytes, so the index always lands on byte 17, inside the 20 bytes the guest sees as the
// serial, and nothing a user writes in a diskSet name can push it out. Within one instance the
// fingerprint is constant and the index is distinct, so no two of its disks can collide.
//
// The minimum diskSet name length of three characters then makes every data disk name at least 23
// bytes, so the serial is a truncation rather than a padded field, and the guest can compare it as a
// plain string.
func diskName(instanceName string, index int, setName string, ordinal int32) string {
	return fmt.Sprintf("%s%d-%s-%d", DataPrefix(instanceName), index, setName, ordinal)
}

// fingerprint is a fixed-width, lowercase base32 digest of the instance name.
//
// Base32 rather than hex so that 60 bits fit in 12 characters, and lowercased because an Oxide name
// may not contain uppercase. RFC 4648's alphabet has no 0, 1, 8 or 9, so a fingerprint can never be
// confused with the decimal index and ordinal that follow it, and can never start with a digit.
func fingerprint(instanceName string) string {
	sum := sha256.Sum256([]byte(instanceName))
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

// Names is every disk name the launch owns. It is what cleanup abandons on a failure: the whole
// planned set rather than the disks that happened to be created, since a name in it can only ever
// have been created by an earlier attempt at this same launch.
func (p Plan) Names() []string {
	return lo.Map(p.All(), func(disk Disk, _ int) string { return disk.Name })
}

// DistributedBytes is the storage this launch would add to the silo's provisioned total.
//
// Local disks are deliberately excluded. The Oxide API reports one storage figure with no way to tell
// the two backends apart, so whether local disks are charged to it is not something this code can
// know. The two ways to be wrong are not symmetric: under-counting means the pre-check passes and the
// disk create is refused, which the launch already handles and reports as insufficient storage, while
// over-counting means refusing launches the rack would have accepted and explaining the refusal with a
// number that is not true, which nothing recovers from. So this counts what it is sure of and leaves
// the API as the authority on the rest.
func (p Plan) DistributedBytes() int64 {
	total := p.Boot.SizeBytes
	for _, disk := range p.Data() {
		if !disk.Local {
			total += disk.SizeBytes
		}
	}
	return total
}

// MountOrder is every disk the guest mounts, ordered so that a parent directory is always mounted
// before anything nested beneath it.
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
func (p Plan) BootAttachment() oxide.InstanceDiskAttachment {
	return oxide.InstanceDiskAttachment{Value: attachmentCreate(p.Boot, "Boot disk")}
}

// DataAttachments is every data disk as an instance create asks for it.
func (p Plan) DataAttachments() []oxide.InstanceDiskAttachment {
	data := p.Data()
	attachments := make([]oxide.InstanceDiskAttachment, 0, len(data))
	for _, disk := range data {
		attachments = append(attachments, oxide.InstanceDiskAttachment{
			Value: attachmentCreate(
				disk,
				fmt.Sprintf("diskSet %s disk %d", disk.Set, disk.Ordinal),
			),
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

// backend is the Oxide disk backend for a disk.
//
// A local disk has no disk source at all: DiskBackendLocal is an empty struct, which is the API's way
// of saying a local disk cannot be created from an image or a snapshot and has a block size fixed at
// 4 KiB. That is why a Local diskSet cannot carry a source selector.
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

// readOnly is nil unless the disk is actually read-only, so that the common case sends no field at
// all rather than an explicit false.
func readOnly(disk Disk) *bool {
	if !disk.ReadOnly {
		return nil
	}
	return new(true)
}
