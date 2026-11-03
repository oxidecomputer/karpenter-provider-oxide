package disk

import (
	"slices"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"k8s.io/apimachinery/pkg/api/resource"
)

func nodeClass(sets ...v1alpha1.DiskSet) *v1alpha1.OxideNodeClass {
	return &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot:     v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
				DiskSets: sets,
			},
		},
		Status: v1alpha1.OxideNodeClassStatus{
			Images: []v1alpha1.OxideImage{{
				ID:        "11111111-1111-1111-1111-111111111111",
				SizeBytes: 4 << 30,
			}},
		},
	}
}

func diskSet(name string, count int32, fs, mountPath, mountDir string) v1alpha1.DiskSet {
	return v1alpha1.DiskSet{
		Name:       name,
		Type:       v1alpha1.DiskSetTypeDistributed,
		Count:      count,
		Size:       resource.MustParse("100Gi"),
		Filesystem: fs,
		MountPath:  mountPath,
		MountDir:   mountDir,
	}
}

func plan(t *testing.T, sets ...v1alpha1.DiskSet) *storage.Plan {
	t.Helper()
	built, err := storage.Build("inst-test", "launch-token", nodeClass(sets...))
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}
	return &built
}

func build(t *testing.T, sets ...v1alpha1.DiskSet) *Layout {
	t.Helper()
	layout, err := Build(plan(t, sets...))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return layout
}

func TestBuildIsNilWithoutDataDisks(t *testing.T) {
	if layout := build(t); layout != nil {
		t.Errorf("a plan with no data disks built %+v, want nil", layout)
	}
	if layout, err := Build(nil); err != nil || layout != nil {
		t.Errorf("a nil plan built (%+v, %v), want (nil, nil)", layout, err)
	}
}

func TestBuildClassifiesEveryDisk(t *testing.T) {
	layout := build(t,
		diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/rancher", ""),
		diskSet("local-pv", 2, v1alpha1.FilesystemXFS, "", "/mnt/local-pv"),
		diskSet("raw", 1, v1alpha1.FilesystemNone, "", "/mnt/raw"),
	)

	formatted := make([]string, 0, len(layout.Formats))
	for _, format := range layout.Formats {
		formatted = append(formatted, format.Device+" "+format.Filesystem)
	}
	wantFormatted := []string{
		"/dev/karpenter/kubernetes-0 ext4",
		"/dev/karpenter/local-pv-0 xfs",
		"/dev/karpenter/local-pv-1 xfs",
	}
	if !slices.Equal(formatted, wantFormatted) {
		t.Errorf("formats = %v, want %v", formatted, wantFormatted)
	}

	wantMounts := []string{
		"/mnt/local-pv/local-pv-0",
		"/mnt/local-pv/local-pv-1",
		"/var/lib/rancher",
	}
	if got := layout.MountPoints(); !slices.Equal(got, wantMounts) {
		t.Errorf("mount points = %v, want them in mount order %v", got, wantMounts)
	}
	for _, mount := range layout.Mounts {
		if strings.Contains(mount.Options, "nofail") {
			t.Errorf(
				"%s gained nofail, which would let the kubelet start without it",
				mount.MountPoint,
			)
		}
		if !mount.Formatted {
			t.Errorf("%s is not marked as formatted by the guest", mount.MountPoint)
		}
	}

	if len(layout.Links) != 1 || layout.Links[0] != (Link{
		Device: "/dev/karpenter/raw-0", Path: "/mnt/raw/raw-0",
	}) {
		t.Errorf("links = %+v, want the raw disk linked beneath its mountDir", layout.Links)
	}

	if !strings.Contains(layout.DropIn(), "RequiresMountsFor="+strings.Join(wantMounts, " ")+"\n") {
		t.Errorf("drop-in does not require every mount:\n%s", layout.DropIn())
	}
}

func TestMkfsOptionsSkipReserveAndDiscard(t *testing.T) {
	layout := build(t,
		diskSet("ext", 1, v1alpha1.FilesystemExt4, "/mnt/ext", ""),
		diskSet("xfs", 1, v1alpha1.FilesystemXFS, "/mnt/xfs", ""),
	)
	for _, format := range layout.Formats {
		joined := strings.Join(format.Options, " ")
		var want string
		switch format.Filesystem {
		case v1alpha1.FilesystemExt4:
			want = "-m 0 -E nodiscard,lazy_itable_init=1,lazy_journal_init=1"
		case v1alpha1.FilesystemXFS:
			want = "-K"
		}
		if joined != want {
			t.Errorf("%s mkfs options = %q, want %q", format.Filesystem, joined, want)
		}
		if strings.Contains(joined, "-F") || strings.Contains(joined, "-f") {
			t.Errorf("%s options carry a force flag, which the renderer owns", format.Filesystem)
		}
	}
}

func TestRawOnlyLayoutHasNoDropIn(t *testing.T) {
	layout := build(t, diskSet("raw", 2, v1alpha1.FilesystemNone, "", "/mnt/raw"))
	if layout.DropIn() != "" {
		t.Errorf("a layout with nothing to mount rendered a drop-in:\n%s", layout.DropIn())
	}
	if len(layout.Formats) != 0 || len(layout.Mounts) != 0 || len(layout.Links) != 2 {
		t.Errorf("raw layout = %+v, want only two links", layout)
	}
}

func TestSourcedDiskIsMountedNotFormatted(t *testing.T) {
	set := diskSet("sourced", 1, "", "/mnt/sourced", "")
	// A read-only sourced disk must match its source size.
	set.Size = resource.MustParse("4Gi")
	set.ImageSelector = []v1alpha1.ImageSelector{{Name: "some-image"}}
	readOnly := true
	set.ReadOnly = &readOnly
	nc := nodeClass(set)
	nc.Status.Storage = &v1alpha1.StorageStatus{
		DiskSets: []v1alpha1.DiskSetStatus{{
			Name: "sourced",
			Images: []v1alpha1.OxideImage{{
				ID:        "11111111-1111-1111-1111-111111111111",
				SizeBytes: 4 << 30,
			}},
		}},
	}
	built, err := storage.Build("inst-test", "launch-token", nc)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}
	layout, err := Build(&built)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(layout.Formats) != 0 {
		t.Errorf("a sourced disk would be formatted: %+v", layout.Formats)
	}
	if len(layout.Mounts) != 1 {
		t.Fatalf("mounts = %+v, want one", layout.Mounts)
	}
	mount := layout.Mounts[0]
	if mount.Type != typeAuto || mount.Formatted {
		t.Errorf("sourced mount = %+v, want type auto and not formatted", mount)
	}
	if !strings.HasSuffix(mount.Options, ",ro") {
		t.Errorf("a read-only attachment is not mounted ro: %q", mount.Options)
	}
}

func TestUdevRules(t *testing.T) {
	built := plan(t,
		diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/rancher", ""),
		diskSet("local-pv", 2, v1alpha1.FilesystemXFS, "", "/mnt/local-pv"),
	)
	rules, err := UdevRules(built)
	if err != nil {
		t.Fatalf("UdevRules: %v", err)
	}
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSpace(rules), "\n") {
		if !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 3 {
		t.Fatalf("got %d rules, want one per data disk (3):\n%s", len(lines), rules)
	}
	for _, disk := range built.Data() {
		want := `ATTRS{serial}=="` + disk.Serial + `", SYMLINK+="karpenter/` +
			DeviceName(disk.Set, disk.Ordinal) + `"`
		if !strings.Contains(rules, want) {
			t.Errorf("no rule matching %q:\n%s", want, rules)
		}
	}
	for _, want := range []string{`ENV{DEVTYPE}=="disk"`, `KERNEL=="nvme*n*"`} {
		if strings.Count(rules, want) != 3 {
			t.Errorf("not every rule carries %s", want)
		}
	}
	if strings.Contains(rules, built.Boot.Serial) {
		t.Error("the rules name the boot disk's serial")
	}
}

func TestUdevRulesRefuseAGlobbingSerial(t *testing.T) {
	built := &storage.Plan{Sets: []storage.Set{{
		Name: "kubernetes",
		Disks: []storage.Disk{
			{Name: "kd-abc", Serial: "kd-abc*", Filesystem: v1alpha1.FilesystemExt4},
		},
	}}}
	if _, err := UdevRules(built); err == nil {
		t.Error("a serial containing a glob metacharacter was accepted")
	}
}

func TestBuildRefusesUnrepresentableValues(t *testing.T) {
	for name, mountPoint := range map[string]string{
		"a space":     "/mnt/with space",
		"a tab":       "/mnt/with\ttab",
		"a quote":     "/mnt/with'quote",
		"a backslash": `/mnt/with\backslash`,
		"a newline":   "/mnt/with\nnewline",
	} {
		built := &storage.Plan{Sets: []storage.Set{{
			Name: "kubernetes",
			Type: v1alpha1.DiskSetTypeDistributed,
			Disks: []storage.Disk{{
				Name:       "kd-abc-1-kubernetes-0",
				Serial:     "kd-abc-1-kubernetes",
				Set:        "kubernetes",
				Filesystem: v1alpha1.FilesystemExt4,
				MountPoint: mountPoint,
			}},
		}}}
		if _, err := Build(built); err == nil {
			t.Errorf("a mount point containing %s was accepted", name)
		}
	}
}
