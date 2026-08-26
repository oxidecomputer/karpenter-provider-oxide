package storage

import (
	"regexp"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// oxideName is what the Oxide API accepts as a name: lowercase alphanumerics and hyphens, starting
// with a letter, not ending in a hyphen, at most 63 characters.
var oxideName = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

func set(t *testing.T, name string, opts ...func(*v1alpha1.DiskSet)) v1alpha1.DiskSet {
	t.Helper()
	s := v1alpha1.DiskSet{
		Name:     name,
		Type:     v1alpha1.DiskSetTypeDistributed,
		Count:    1,
		Size:     resource.MustParse("100Gi"),
		Layout:   v1alpha1.DiskSetLayoutIndividual,
		MountDir: "/mnt/" + name,
	}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

func nodeClass(t *testing.T, sets ...v1alpha1.DiskSet) *v1alpha1.OxideNodeClass {
	t.Helper()
	return &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot:     v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
				DiskSets: sets,
			},
		},
	}
}

func mustBuild(t *testing.T, instance string, nc *v1alpha1.OxideNodeClass) Plan {
	t.Helper()
	plan, err := Build(instance, nc)
	if err != nil {
		t.Fatalf("Build(%q) returned an unexpected error: %v", instance, err)
	}
	return plan
}

// Every disk name a plan can produce has to be at least as long as the serial window, because the
// guest matches the serial as a plain string. A shorter name would be padded by the NVMe layer and
// the guest would have to know to strip the padding.
func TestSerialIsAlwaysExactlyTwentyBytes(t *testing.T) {
	tests := []struct {
		name     string
		instance string
		sets     []v1alpha1.DiskSet
	}{
		{
			name:     "shortest possible diskSet name",
			instance: "karpenter-a",
			sets:     []v1alpha1.DiskSet{set(t, "abc")},
		},
		{
			name:     "longest possible diskSet name",
			instance: "karpenter-default-abcde",
			sets:     []v1alpha1.DiskSet{set(t, strings.Repeat("a", 20))},
		},
		{
			name:     "a full instance of seven data disks",
			instance: "karpenter-default-x7k2p",
			sets: []v1alpha1.DiskSet{
				set(t, "kubernetes", func(s *v1alpha1.DiskSet) { s.Count = 3 }),
				set(t, "local-pv", func(s *v1alpha1.DiskSet) {
					s.Count = 4
					s.Type = v1alpha1.DiskSetTypeLocal
				}),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := mustBuild(t, tt.instance, nodeClass(t, tt.sets...))
			for _, disk := range plan.Data() {
				if got, want := len(disk.Serial), serialLength; got != want {
					t.Errorf(
						"disk %q serial %q is %d bytes, want %d",
						disk.Name,
						disk.Serial,
						got,
						want,
					)
				}
				if got, want := len(disk.Name), serialLength; got < want {
					t.Errorf("disk name %q is %d bytes, want at least %d", disk.Name, got, want)
				}
			}
		})
	}
}

// The uniqueness byte has to sit inside the serial window at a position nothing a user writes can
// move. This is the invariant the whole naming scheme exists to hold.
func TestIndexAlwaysLandsInsideTheSerialWindow(t *testing.T) {
	const indexOffset = 16 // zero-based: byte 17

	for _, setName := range []string{"abc", "local-pv", strings.Repeat("z", 20)} {
		t.Run(setName, func(t *testing.T) {
			plan := mustBuild(t, "karpenter-default-x7k2p", nodeClass(t,
				set(t, setName, func(s *v1alpha1.DiskSet) { s.Count = 7 }),
			))
			for _, disk := range plan.Data() {
				got := string(disk.Name[indexOffset])
				want := string(rune('0' + disk.Index))
				if got != want {
					t.Errorf("disk %q has %q at byte %d, want the index %q",
						disk.Name, got, indexOffset+1, want)
				}
				if indexOffset >= serialLength {
					t.Fatalf("index offset %d is outside the serial window", indexOffset)
				}
			}
		})
	}
}

// A collision would cost the node a disk silently, so it must be an error on the launch.
func TestBuildRejectsCollidingSerials(t *testing.T) {
	// Two sets whose names agree in their first two characters still differ, because the index byte
	// precedes the name. Force a genuine collision by planning a boot disk whose name collides with
	// a data disk instead.
	plan := mustBuild(t, "karpenter-default-x7k2p", nodeClass(t,
		set(t, "kubernetes", func(s *v1alpha1.DiskSet) { s.Count = 7 }),
	))
	seen := map[string]string{}
	for _, disk := range plan.All() {
		if other, ok := seen[disk.Serial]; ok {
			t.Fatalf("plan already contains a serial collision between %q and %q", other, disk.Name)
		}
		seen[disk.Serial] = disk.Name
	}

	// And assert the guard itself fires when handed a plan that does collide.
	colliding := Plan{
		Boot: Disk{Name: "boot", Serial: "same"},
		Sets: []Set{{Disks: []Disk{{Name: "data", Serial: "same"}}}},
	}
	err := checkSerials(colliding)
	if err == nil {
		t.Fatal("checkSerials accepted a plan with two disks sharing a serial")
	}
	for _, want := range []string{"boot", "data", "same"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestDiskNamesAreLegalOxideNames(t *testing.T) {
	plan := mustBuild(t, "karpenter-default-x7k2p", nodeClass(t,
		set(t, strings.Repeat("a", 20), func(s *v1alpha1.DiskSet) { s.Count = 7 }),
	))
	for _, disk := range plan.All() {
		if !oxideName.MatchString(disk.Name) {
			t.Errorf("disk name %q is not a legal Oxide name", disk.Name)
		}
		if len(disk.Name) > 63 {
			t.Errorf(
				"disk name %q is %d bytes, over the Oxide limit of 63",
				disk.Name,
				len(disk.Name),
			)
		}
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	nc := nodeClass(t, set(t, "kubernetes", func(s *v1alpha1.DiskSet) { s.Count = 3 }))
	first := mustBuild(t, "karpenter-default-x7k2p", nc)
	second := mustBuild(t, "karpenter-default-x7k2p", nc)
	if got, want := strings.Join(
		second.Names(),
		",",
	), strings.Join(
		first.Names(),
		",",
	); got != want {
		t.Errorf("Build is not deterministic:\n got %q\nwant %q", got, want)
	}
	other := mustBuild(t, "karpenter-default-y8m3q", nc)
	if other.DataPrefix == first.DataPrefix {
		t.Errorf("two instances share the data prefix %q", first.DataPrefix)
	}
}

// The boot disk deliberately keeps the instance's own name, so a teardown that has nothing but the
// NodeClaim name can still find it.
func TestBootDiskKeepsTheInstanceName(t *testing.T) {
	plan := mustBuild(t, "karpenter-default-x7k2p", nodeClass(t))
	if got, want := plan.Boot.Name, "karpenter-default-x7k2p"; got != want {
		t.Errorf("boot disk name = %q, want %q", got, want)
	}
	if strings.HasPrefix(plan.Boot.Name, dataPrefix) {
		t.Errorf("boot disk name %q collides with the data disk prefix", plan.Boot.Name)
	}
}

func TestMountResolution(t *testing.T) {
	tests := []struct {
		name       string
		set        v1alpha1.DiskSet
		wantMounts []string
		wantLinks  []string
	}{
		{
			name: "mountPath is the literal mount point for a single disk",
			set: set(t, "kubernetes", func(s *v1alpha1.DiskSet) {
				s.MountDir = ""
				s.MountPath = "/var/lib/rancher"
			}),
			wantMounts: []string{"/var/lib/rancher"},
		},
		{
			name: "mountDir is a parent even for a single disk",
			set: set(t, "local-pv", func(s *v1alpha1.DiskSet) {
				s.MountDir = "/mnt/local-pv"
			}),
			wantMounts: []string{"/mnt/local-pv/local-pv-0"},
		},
		{
			name: "mountDir numbers each disk of a set",
			set: set(t, "local-pv", func(s *v1alpha1.DiskSet) {
				s.Count = 4
				s.MountDir = "/mnt/local-pv"
			}),
			wantMounts: []string{
				"/mnt/local-pv/local-pv-0",
				"/mnt/local-pv/local-pv-1",
				"/mnt/local-pv/local-pv-2",
				"/mnt/local-pv/local-pv-3",
			},
		},
		{
			name: "a raw block set is linked, never mounted",
			set: set(t, "raw", func(s *v1alpha1.DiskSet) {
				s.Count = 2
				s.Filesystem = v1alpha1.FilesystemNone
				s.MountDir = "/mnt/raw"
			}),
			wantLinks: []string{"/mnt/raw/raw-0", "/mnt/raw/raw-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := mustBuild(t, "karpenter-default-x7k2p", nodeClass(t, tt.set))

			var mounts []string
			for _, disk := range plan.MountOrder() {
				mounts = append(mounts, disk.MountPoint)
			}
			if got, want := strings.Join(
				mounts,
				",",
			), strings.Join(
				tt.wantMounts,
				",",
			); got != want {
				t.Errorf("mount points = %q, want %q", got, want)
			}

			var links []string
			for _, disk := range plan.BlockLinks() {
				links = append(links, disk.BlockLink)
			}
			if got, want := strings.Join(links, ","), strings.Join(tt.wantLinks, ","); got != want {
				t.Errorf("block links = %q, want %q", got, want)
			}
		})
	}
}

// A raw block disk must never end up with a mount point, and a mounted disk must never end up with a
// link. The renderer switches on exactly this.
func TestRawBlockAndMountedAreMutuallyExclusive(t *testing.T) {
	plan := mustBuild(t, "karpenter-default-x7k2p", nodeClass(t,
		set(t, "raw", func(s *v1alpha1.DiskSet) {
			s.Count = 2
			s.Filesystem = v1alpha1.FilesystemNone
			s.MountDir = "/mnt/raw"
		}),
		set(t, "data", func(s *v1alpha1.DiskSet) { s.MountDir = "/mnt/data" }),
	))
	for _, disk := range plan.Data() {
		if disk.MountPoint != "" && disk.BlockLink != "" {
			t.Errorf("disk %q has both a mount point %q and a link %q",
				disk.Name, disk.MountPoint, disk.BlockLink)
		}
		if disk.MountPoint == "" && disk.BlockLink == "" {
			t.Errorf("disk %q has neither a mount point nor a link", disk.Name)
		}
	}
}

// Nested mounts have to be established outermost first or the inner mount is hidden.
func TestMountOrderPutsParentsFirst(t *testing.T) {
	plan := mustBuild(t, "karpenter-default-x7k2p", nodeClass(t,
		set(t, "inner", func(s *v1alpha1.DiskSet) {
			s.MountDir = ""
			s.MountPath = "/mnt/data/inner"
		}),
		set(t, "outer", func(s *v1alpha1.DiskSet) {
			s.MountDir = ""
			s.MountPath = "/mnt/data"
		}),
	))
	order := plan.MountOrder()
	mounts := make([]string, 0, len(order))
	for _, disk := range order {
		mounts = append(mounts, disk.MountPoint)
	}
	if got, want := strings.Join(mounts, ","), "/mnt/data,/mnt/data/inner"; got != want {
		t.Errorf("mount order = %q, want %q", got, want)
	}
}

func TestBuildRejectsUnsafeMountTargets(t *testing.T) {
	tests := []struct {
		name string
		set  v1alpha1.DiskSet
	}{
		{"relative", set(t, "bad", func(s *v1alpha1.DiskSet) { s.MountDir = "mnt/bad" })},
		{"unclean", set(t, "bad", func(s *v1alpha1.DiskSet) { s.MountDir = "/mnt/../bad" })},
		{"trailing slash", set(t, "bad", func(s *v1alpha1.DiskSet) { s.MountDir = "/mnt/bad/" })},
		{"single quote", set(t, "bad", func(s *v1alpha1.DiskSet) { s.MountDir = "/mnt/b'ad" })},
		{"double quote", set(t, "bad", func(s *v1alpha1.DiskSet) { s.MountDir = `/mnt/b"ad` })},
		{"backslash", set(t, "bad", func(s *v1alpha1.DiskSet) { s.MountDir = `/mnt/b\ad` })},
		{"newline", set(t, "bad", func(s *v1alpha1.DiskSet) { s.MountDir = "/mnt/b\nad" })},
		{"neither field", set(t, "bad", func(s *v1alpha1.DiskSet) { s.MountDir = "" })},
		{"both fields", set(t, "bad", func(s *v1alpha1.DiskSet) {
			s.MountDir = "/mnt/bad"
			s.MountPath = "/mnt/worse"
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Build("karpenter-default-x7k2p", nodeClass(t, tt.set)); err == nil {
				t.Error("Build accepted an unsafe mount target")
			}
		})
	}
}

func TestBuildRejectsTooManyDisks(t *testing.T) {
	_, err := Build("karpenter-default-x7k2p", nodeClass(t,
		set(t, "aaa", func(s *v1alpha1.DiskSet) { s.Count = 4 }),
		set(t, "bbb", func(s *v1alpha1.DiskSet) { s.Count = 4 }),
	))
	if err == nil {
		t.Fatal("Build accepted nine disks, over the limit of eight")
	}
	if !strings.Contains(err.Error(), "too many disks") {
		t.Errorf("error %q does not mention the disk limit", err)
	}
}

func TestBuildRejectsAnUnresolvedSource(t *testing.T) {
	nc := nodeClass(t, set(t, "sourced", func(s *v1alpha1.DiskSet) {
		s.ImageSelector = []v1alpha1.ImageSelector{{Name: "some-image"}}
	}))
	if _, err := Build("karpenter-default-x7k2p", nc); err == nil {
		t.Fatal("Build accepted a sourced diskSet whose selector had not resolved")
	}

	nc.Status.Storage = &v1alpha1.StorageStatus{
		DiskSets: []v1alpha1.DiskSetStatus{{Name: "sourced"}},
	}
	if _, err := Build("karpenter-default-x7k2p", nc); err == nil {
		t.Fatal("Build accepted a sourced diskSet that resolved to no images")
	}
}

func TestBuildRejectsADiskSmallerThanItsSource(t *testing.T) {
	nc := nodeClass(t, set(t, "sourced", func(s *v1alpha1.DiskSet) {
		s.Size = resource.MustParse("10Gi")
		s.ImageSelector = []v1alpha1.ImageSelector{{Name: "big"}}
	}))
	nc.Status.Storage = &v1alpha1.StorageStatus{
		DiskSets: []v1alpha1.DiskSetStatus{{
			Name:   "sourced",
			Images: []v1alpha1.OxideImage{{ID: "img", SizeBytes: 100 << 30}},
		}},
	}
	_, err := Build("karpenter-default-x7k2p", nc)
	if err == nil {
		t.Fatal("Build accepted a disk smaller than the image it is created from")
	}
	if !strings.Contains(err.Error(), "smaller than") {
		t.Errorf("error %q does not explain the size mismatch", err)
	}
}

// Local disks are excluded from the storage quota pre-check, because the Oxide API gives no way to
// know whether they are charged to it, and over-counting refuses launches the rack would accept.
func TestDistributedBytesExcludesLocalDisks(t *testing.T) {
	plan := mustBuild(t, "karpenter-default-x7k2p", nodeClass(t,
		set(t, "distributed", func(s *v1alpha1.DiskSet) {
			s.Size = resource.MustParse("100Gi")
		}),
		set(t, "local", func(s *v1alpha1.DiskSet) {
			s.Type = v1alpha1.DiskSetTypeLocal
			s.Count = 4
			s.Size = resource.MustParse("500Gi")
		}),
	))
	if got, want := plan.DistributedBytes(), int64(120)<<30; got != want {
		t.Errorf("DistributedBytes() = %d, want %d (20Gi boot + 100Gi distributed)", got, want)
	}
}

func TestSourcedDiskSetIsNeverFormatted(t *testing.T) {
	nc := nodeClass(t, set(t, "sourced", func(s *v1alpha1.DiskSet) {
		s.ImageSelector = []v1alpha1.ImageSelector{{Name: "img"}}
	}))
	nc.Status.Storage = &v1alpha1.StorageStatus{
		DiskSets: []v1alpha1.DiskSetStatus{{
			Name:   "sourced",
			Images: []v1alpha1.OxideImage{{ID: "img-id"}},
		}},
	}
	plan := mustBuild(t, "karpenter-default-x7k2p", nc)
	for _, disk := range plan.Data() {
		if disk.Filesystem != "" {
			t.Errorf("sourced disk %q would be formatted as %q", disk.Name, disk.Filesystem)
		}
		if disk.ImageID != "img-id" {
			t.Errorf("disk %q image = %q, want %q", disk.Name, disk.ImageID, "img-id")
		}
	}
}

func TestUnsetFilesystemMeansExt4WhereFormattingApplies(t *testing.T) {
	plan := mustBuild(t, "karpenter-default-x7k2p", nodeClass(t,
		set(t, "blank", func(s *v1alpha1.DiskSet) { s.Filesystem = "" }),
	))
	for _, disk := range plan.Data() {
		if got, want := disk.Filesystem, v1alpha1.FilesystemExt4; got != want {
			t.Errorf("disk %q filesystem = %q, want %q", disk.Name, got, want)
		}
	}
}
