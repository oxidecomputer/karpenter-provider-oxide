package storage

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

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
		Status: v1alpha1.OxideNodeClassStatus{
			Images: []v1alpha1.OxideImage{{ID: "boot-image", SizeBytes: 10 << 30}},
		},
	}
}

const testToken = "launch-token"

func mustBuild(t *testing.T, instance string, nc *v1alpha1.OxideNodeClass) Plan {
	t.Helper()
	plan, err := Build(instance, testToken, nc)
	if err != nil {
		t.Fatalf("Build(%q) returned an unexpected error: %v", instance, err)
	}
	return plan
}

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

func TestIndexAlwaysLandsInsideTheSerialWindow(t *testing.T) {
	const indexOffset = 16

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

func TestBuildRejectsCollidingSerials(t *testing.T) {

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
	if other.Prefix == first.Prefix {
		t.Errorf("two instances share the disk prefix %q", first.Prefix)
	}
}

func TestBuildNamesDisksPerLaunchAttempt(t *testing.T) {
	nc := nodeClass(t, set(t, "kubernetes", func(s *v1alpha1.DiskSet) { s.Count = 3 }))

	first, err := Build("karpenter-default-x7k2p", "attempt-1", nc)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := Build("karpenter-default-x7k2p", "attempt-2", nc)
	if err != nil {
		t.Fatal(err)
	}

	if retry.Prefix == first.Prefix {
		t.Fatalf("both attempts named their disks %q, want a prefix per attempt", first.Prefix)
	}
	reused := map[string]struct{}{}
	for _, name := range first.Names() {
		reused[name] = struct{}{}
	}
	for _, name := range retry.Names() {
		if _, ok := reused[name]; ok {
			t.Errorf("the retry would take over the first attempt's disk %q", name)
		}
	}
}

func TestBuildRequiresALaunchToken(t *testing.T) {
	if _, err := Build("karpenter-default-x7k2p", "", nodeClass(t)); err == nil {
		t.Error("Build() accepted an empty launch token, want it refused")
	}
}

func TestBootDiskIsNamedFromTheLaunchPrefix(t *testing.T) {
	plan := mustBuild(t, "karpenter-default-x7k2p", nodeClass(t, set(t, "kubernetes")))

	if !strings.HasPrefix(plan.Boot.Name, plan.Prefix) {
		t.Errorf("boot disk name %q is not under the launch prefix %q", plan.Boot.Name, plan.Prefix)
	}
	if strings.Contains(plan.Boot.Name, "karpenter-default-x7k2p") {
		t.Errorf("boot disk name %q still carries the instance name", plan.Boot.Name)
	}

	for _, disk := range plan.Data() {
		if disk.Name == plan.Boot.Name || disk.Serial == plan.Boot.Serial {
			t.Errorf("data disk %q collides with the boot disk %q", disk.Name, plan.Boot.Name)
		}
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
			if _, err := Build(
				"karpenter-default-x7k2p",
				testToken,
				nodeClass(t, tt.set),
			); err == nil {
				t.Error("Build accepted an unsafe mount target")
			}
		})
	}
}

func TestBuildRejectsTooManyDisks(t *testing.T) {
	_, err := Build("karpenter-default-x7k2p", testToken, nodeClass(t,
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
	if _, err := Build("karpenter-default-x7k2p", testToken, nc); err == nil {
		t.Fatal("Build accepted a sourced diskSet whose selector had not resolved")
	}

	nc.Status.Storage = &v1alpha1.StorageStatus{
		DiskSets: []v1alpha1.DiskSetStatus{{Name: "sourced"}},
	}
	if _, err := Build("karpenter-default-x7k2p", testToken, nc); err == nil {
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
	_, err := Build("karpenter-default-x7k2p", testToken, nc)
	if err == nil {
		t.Fatal("Build accepted a disk smaller than the image it is created from")
	}
	if !strings.Contains(err.Error(), "at least its source size") {
		t.Errorf("error %q does not explain the size mismatch", err)
	}
}

func TestBuildValidatesStaticDiskSizes(t *testing.T) {
	for _, tt := range []struct {
		name, size, diskType string
		valid                bool
	}{
		{name: "zero", size: "0", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "below minimum", size: "512Mi", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "minimum", size: "1Gi", diskType: v1alpha1.DiskSetTypeDistributed, valid: true},
		{name: "fractional GiB", size: "1536Mi", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "whole GiB", size: "2Gi", diskType: v1alpha1.DiskSetTypeDistributed, valid: true},
		{name: "distributed maximum", size: "1023Gi", diskType: v1alpha1.DiskSetTypeDistributed, valid: true},
		{name: "above distributed maximum", size: "1024Gi", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "negative", size: "-1Gi", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "overflow", size: "1000000000000000000000000000000Gi", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "large local", size: "1024Gi", diskType: v1alpha1.DiskSetTypeLocal, valid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nc := nodeClass(t, set(t, "data", func(set *v1alpha1.DiskSet) {
				set.Size = resource.MustParse(tt.size)
				set.Type = tt.diskType
			}))
			_, err := Build("karpenter-default-x7k2p", testToken, nc)
			if (err == nil) != tt.valid {
				t.Fatalf("Build() error = %v, valid = %t", err, tt.valid)
			}
			if !tt.valid {
				var sizeErr *v1alpha1.DiskSizeError
				if !errors.As(err, &sizeErr) {
					t.Fatalf("Build() error = %v, want a typed DiskSizeError", err)
				}
			}
		})
	}
}

func TestBuildValidatesBootImageSize(t *testing.T) {
	for _, tt := range []struct {
		name      string
		requested string
		source    int64
		valid     bool
	}{
		{name: "smaller", requested: "1Gi", source: 2 << 30},
		{name: "equal", requested: "2Gi", source: 2 << 30, valid: true},
		{name: "larger", requested: "3Gi", source: 2 << 30, valid: true},
		{name: "unknown", requested: "2Gi", source: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nc := nodeClass(t)
			nc.Spec.Storage.Boot.Size = resource.MustParse(tt.requested)
			nc.Status.Images[0].SizeBytes = tt.source
			_, err := Build("karpenter-default-x7k2p", testToken, nc)
			if (err == nil) != tt.valid {
				t.Fatalf("Build() error = %v, valid = %t", err, tt.valid)
			}
		})
	}
}

func TestBuildValidatesStaticBootSizes(t *testing.T) {
	for _, tt := range []struct {
		size  string
		valid bool
	}{
		{size: "0"},
		{size: "512Mi"},
		{size: "1Gi", valid: true},
		{size: "1536Mi"},
		{size: "1023Gi", valid: true},
		{size: "1024Gi"},
		{size: "-1Gi"},
		{size: "1000000000000000000000000000000Gi"},
	} {
		t.Run(tt.size, func(t *testing.T) {
			nc := nodeClass(t)
			nc.Spec.Storage.Boot.Size = resource.MustParse(tt.size)
			nc.Status.Images[0].SizeBytes = 1 << 30
			_, err := Build("karpenter-default-x7k2p", testToken, nc)
			if (err == nil) != tt.valid {
				t.Fatalf("Build() error = %v, valid = %t", err, tt.valid)
			}
		})
	}
}

func TestBuildValidatesSourcedDiskSizes(t *testing.T) {
	for _, kind := range []string{"image", "snapshot"} {
		for _, readOnly := range []bool{false, true} {
			for _, requested := range []string{"1Gi", "2Gi", "3Gi"} {
				name := kind + "/" + map[bool]string{false: "read-write", true: "read-only"}[readOnly] + "/" + requested
				t.Run(name, func(t *testing.T) {
					sourced := set(t, "sourced", func(set *v1alpha1.DiskSet) {
						set.Size = resource.MustParse(requested)
						set.ReadOnly = &readOnly
						if kind == "image" {
							set.ImageSelector = []v1alpha1.ImageSelector{{Name: "source"}}
						} else {
							set.SnapshotSelector = []v1alpha1.SnapshotSelector{{Name: "source"}}
						}
					})
					nc := nodeClass(t, sourced)
					status := v1alpha1.DiskSetStatus{Name: sourced.Name}
					if kind == "image" {
						status.Images = []v1alpha1.OxideImage{{ID: "image", SizeBytes: 2 << 30}}
					} else {
						status.Snapshots = []v1alpha1.OxideSnapshot{{ID: "snapshot", SizeBytes: 2 << 30}}
					}
					nc.Status.Storage = &v1alpha1.StorageStatus{DiskSets: []v1alpha1.DiskSetStatus{status}}

					_, err := Build("karpenter-default-x7k2p", testToken, nc)
					want := requested == "2Gi" || (!readOnly && requested == "3Gi")
					if (err == nil) != want {
						t.Fatalf("Build() error = %v, valid = %t", err, want)
					}
				})
			}
		}
	}
}

func TestBuildRejectsUnknownSourceSizes(t *testing.T) {
	nc := nodeClass(t, set(t, "sourced", func(set *v1alpha1.DiskSet) {
		set.Size = resource.MustParse("2Gi")
		set.ImageSelector = []v1alpha1.ImageSelector{{Name: "source"}}
	}))
	nc.Status.Storage = &v1alpha1.StorageStatus{DiskSets: []v1alpha1.DiskSetStatus{{
		Name: "sourced", Images: []v1alpha1.OxideImage{{ID: "image", SizeBytes: 0}},
	}}}

	_, err := Build("karpenter-default-x7k2p", testToken, nc)
	var sizeErr *v1alpha1.DiskSizeError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("Build() error = %v, want a typed DiskSizeError", err)
	}

	nc = nodeClass(t)
	nc.Status.Images = nil
	if _, err := Build("karpenter-default-x7k2p", testToken, nc); err == nil {
		t.Fatal("Build() accepted a boot disk with no resolved image")
	}
}

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
			Images: []v1alpha1.OxideImage{{ID: "img-id", SizeBytes: 100 << 30}},
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
