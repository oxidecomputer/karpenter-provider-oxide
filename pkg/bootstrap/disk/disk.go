// Package disk describes how a launch's data disks appear inside the guest:
// their stable device names, what gets formatted, what gets mounted where, and
// which unit has to wait for all of it. Renderers turn this into user-data.
package disk

import (
	"fmt"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
)

const (
	// RulesPath is the udev rules file naming this instance's disks.
	RulesPath = "/etc/udev/rules.d/99-karpenter-disks.rules"
	// DeviceDir is where the udev rules put the stable names.
	DeviceDir = "/dev/karpenter"
	// DropInName is the file under the consumer unit's .d directory.
	DropInName = "10-karpenter-disks.conf"

	// typeAuto lets mount detect the filesystem a sourced disk already carries.
	typeAuto = "auto"
)

// mountOptions never carry nofail: a disk that cannot be mounted must keep the
// consumer down rather than let it start on an empty directory.
const mountOptions = "defaults,noatime,x-systemd.device-timeout=60s"

// mkfsOptions tune a data disk: no root reserve, no discard, lazy init. They
// omit the force flag; a renderer adds it if its formatter needs one.
var mkfsOptions = map[string][]string{
	v1alpha1.FilesystemExt4: {"-m", "0", "-E", "nodiscard,lazy_itable_init=1,lazy_journal_init=1"},
	v1alpha1.FilesystemXFS:  {"-K"},
}

// Format is one disk the guest formats on first boot.
type Format struct {
	Device     string
	Filesystem string
	Options    []string
}

// Mount is one fstab entry.
type Mount struct {
	Device     string
	MountPoint string
	// Type is the filesystem type, or auto for a disk created from an image or a snapshot.
	Type    string
	Options string
	// Formatted reports whether the guest created the filesystem, which is when it
	// still needs labelling on an SELinux system.
	Formatted bool
}

// Link is a raw block disk's device, linked into its set's mountDir.
type Link struct {
	Device string
	Path   string
}

// Layout is everything a renderer needs to express a plan's data disks.
type Layout struct {
	// Rules is the udev rules file that names every data disk.
	Rules   string
	Formats []Format
	// Mounts are in mount order, parents before children.
	Mounts []Mount
	Links  []Link
}

// Build resolves a plan's data disks, or returns nil when it has none.
func Build(plan *storage.Plan) (*Layout, error) {
	if plan == nil || len(plan.Data()) == 0 {
		return nil, nil
	}
	rules, err := UdevRules(plan)
	if err != nil {
		return nil, err
	}
	layout := &Layout{Rules: rules}
	for _, set := range plan.Sets {
		for _, disk := range set.Disks {
			if err := layout.add(set.Name, disk); err != nil {
				return nil, err
			}
		}
	}
	for _, disk := range plan.MountOrder() {
		options := mountOptions
		if disk.ReadOnly {
			options += ",ro"
		}
		layout.Mounts = append(layout.Mounts, Mount{
			Device:     DevicePath(disk.Set, disk.Ordinal),
			MountPoint: disk.MountPoint,
			Type:       fstabType(disk.Filesystem),
			Options:    options,
			Formatted:  disk.Filesystem != "",
		})
	}
	return layout, nil
}

func (l *Layout) add(setName string, disk storage.Disk) error {
	device := DevicePath(setName, disk.Ordinal)
	for _, field := range []string{device, disk.MountPoint, disk.BlockLink} {
		if field == "" {
			continue
		}
		if err := checkField(setName, disk.Ordinal, field); err != nil {
			return err
		}
	}
	switch disk.Filesystem {
	case v1alpha1.FilesystemExt4, v1alpha1.FilesystemXFS:
		l.Formats = append(l.Formats, Format{
			Device: device, Filesystem: disk.Filesystem, Options: mkfsOptions[disk.Filesystem],
		})
	case v1alpha1.FilesystemNone:
		if disk.BlockLink == "" {
			return fmt.Errorf(
				"disk: diskSet %q disk %d is a raw block disk without a link path",
				setName, disk.Ordinal,
			)
		}
		l.Links = append(l.Links, Link{Device: device, Path: disk.BlockLink})
	case "":
		// Created from an image or a snapshot, so it already carries a filesystem.
	default:
		return fmt.Errorf(
			"disk: diskSet %q disk %d has the unsupported filesystem %q",
			setName, disk.Ordinal, disk.Filesystem,
		)
	}
	return nil
}

// MountPoints lists where the mounted disks land, in mount order.
func (l *Layout) MountPoints() []string {
	points := make([]string, 0, len(l.Mounts))
	for _, mount := range l.Mounts {
		points = append(points, mount.MountPoint)
	}
	return points
}

// DropIn renders the consumer unit drop-in that keeps it down until every disk
// is mounted, or "" when nothing is mounted.
func (l *Layout) DropIn() string {
	if len(l.Mounts) == 0 {
		return ""
	}
	return "[Unit]\n" +
		"# A disk that cannot be mounted keeps this unit down instead of letting it start on\n" +
		"# an empty directory.\n" +
		"RequiresMountsFor=" + strings.Join(l.MountPoints(), " ") + "\n"
}

// DeviceName is a disk's stable udev name.
func DeviceName(setName string, ordinal int32) string {
	return fmt.Sprintf("%s-%d", setName, ordinal)
}

// DevicePath is where DeviceName lands under /dev.
func DevicePath(setName string, ordinal int32) string {
	return DeviceDir + "/" + DeviceName(setName, ordinal)
}

// UdevRules renders the rule file that gives every data disk a stable name.
func UdevRules(plan *storage.Plan) (string, error) {
	var out strings.Builder
	out.WriteString("# Generated by karpenter-provider-oxide.\n")
	for _, set := range plan.Sets {
		for _, disk := range set.Disks {
			if err := checkSerial(set.Name, disk.Ordinal, disk.Serial); err != nil {
				return "", err
			}
			fmt.Fprintf(&out,
				`SUBSYSTEM=="block", ENV{DEVTYPE}=="disk", KERNEL=="nvme*n*", `+
					`ATTRS{serial}=="%s", SYMLINK+="%s"`+"\n",
				disk.Serial,
				strings.TrimPrefix(DevicePath(set.Name, disk.Ordinal), "/dev/"),
			)
		}
	}
	return out.String(), nil
}

func fstabType(filesystem string) string {
	if filesystem == "" {
		return typeAuto
	}
	return filesystem
}

func checkField(setName string, ordinal int32, value string) error {
	if strings.ContainsAny(value, " \t\n\r'\"\\") {
		return fmt.Errorf(
			"disk: diskSet %q disk %d has the value %q, which carries whitespace, a quote or a "+
				"backslash and cannot be rendered into the node's disk configuration",
			setName, ordinal, value,
		)
	}
	return nil
}

func checkSerial(setName string, ordinal int32, serial string) error {
	if serial == "" {
		return fmt.Errorf("disk: diskSet %q disk %d has no serial", setName, ordinal)
	}
	if err := checkField(setName, ordinal, serial); err != nil {
		return err
	}
	if strings.ContainsAny(serial, "*?[]!") {
		return fmt.Errorf(
			"disk: diskSet %q disk %d has the serial %q, which contains an fnmatch metacharacter "+
				"and could not be matched literally by a udev rule",
			setName, ordinal, serial,
		)
	}
	return nil
}
