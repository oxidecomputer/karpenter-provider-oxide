package disk

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"k8s.io/apimachinery/pkg/api/resource"
)

// nodeClass is a nodeclass with one diskSet per shape the table can describe: formatted and mounted
// at a literal path, several disks beneath a directory, a raw block set, and a set created from an
// image so that it is never formatted.
func nodeClass(sets ...v1alpha1.DiskSet) *v1alpha1.OxideNodeClass {
	return &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot:     v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
				DiskSets: sets,
			},
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
	built, err := storage.Build("inst-test", nodeClass(sets...))
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}
	return &built
}

// A nodeclass with no diskSets must render nothing at all, so its user-data is exactly what it was
// before any of this existed.
func TestScriptIsEmptyWithoutDataDisks(t *testing.T) {
	out, err := Script(plan(t), Options{ConsumerUnits: []string{"kubelet.service"}})
	if err != nil {
		t.Fatalf("Script: %v", err)
	}
	if out != "" {
		t.Errorf("a plan with no data disks rendered %d bytes, want none", len(out))
	}
	out, err = Script(nil, Options{})
	if err != nil || out != "" {
		t.Errorf("a nil plan rendered (%q, %v), want (\"\", nil)", out, err)
	}
}

// The user-data block is spliced into a script running under "set -euo pipefail", so it has to parse
// as bash, and so does the body it installs.
func TestRenderedShellParses(t *testing.T) {
	out, err := Script(
		plan(t,
			diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/rancher", ""),
			diskSet("local-pv", 3, v1alpha1.FilesystemXFS, "", "/mnt/local-pv"),
			diskSet("raw", 2, v1alpha1.FilesystemNone, "", "/mnt/raw"),
		),
		Options{ConsumerUnits: []string{"kubelet.service", "rke2-agent.service"}},
	)
	if err != nil {
		t.Fatalf("Script: %v", err)
	}
	for name, script := range map[string]string{
		"user-data block": "set -euo pipefail\n" + out,
		"per-boot body":   setupScript,
	} {
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(script)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s is not valid bash: %v\n%s", name, err, output)
		}
	}
}

// shellcheck is the reason the body is a file rather than a Go string, so it is worth failing the
// build over where it is installed. Skipped rather than failed where it is not, so a contributor
// without it can still run the tests; CI has it.
func TestShellCheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck is not installed")
	}
	out, err := exec.Command("shellcheck", "--severity=style", "scripts/disk-setup.sh").
		CombinedOutput()
	if err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}

// The script hard-codes the paths the Go constants declare, because a static file cannot read them,
// and the fstab options name the unit. Nothing enforces either but this.
func TestScriptAgreesWithConstants(t *testing.T) {
	for _, want := range []string{
		`CONF="${KARPENTER_DISK_CONF:-` + ConfigPath + `}"`,
		`FSTAB_BLOCK="${KARPENTER_FSTAB_BLOCK:-` + FstabBlockPath + `}"`,
		`RUN_DIR="${KARPENTER_RUN_DIR:-` + RunDir + `}"`,
		`readonly ERRFILE="$RUN_DIR/disks.error"`,
		"systemctl enable " + UnitName,
		"systemctl start " + UnitName,
	} {
		if !strings.Contains(setupScript, want) {
			t.Errorf("scripts/disk-setup.sh no longer contains %q", want)
		}
	}
	if ErrorPath != RunDir+"/disks.error" {
		t.Error("the error path no longer agrees with RunDir")
	}
	if !strings.Contains(unitFile, "ExecStart="+ScriptPath) {
		t.Errorf("the unit no longer runs %s", ScriptPath)
	}
	if !strings.Contains(mountOptions, "x-systemd.requires="+UnitName) {
		t.Error("the fstab options no longer order the mounts after the unit")
	}
}

// The whole design rests on the mount units being ordered after the unit that formats the disks, and
// on that unit being able to run before local-fs.target without a dependency cycle. Both are one
// line each and neither fails visibly if it is dropped: systemd breaks a cycle by silently discarding
// an ordering, which would leave a mount racing a mkfs.
func TestUnitCanRunBeforeTheMounts(t *testing.T) {
	for _, want := range []string{
		"DefaultDependencies=no",
		"Before=local-fs.target",
		"After=systemd-udevd.service",
		"WantedBy=local-fs.target",
	} {
		if !strings.Contains(unitFile, want) {
			t.Errorf("the unit no longer carries %q, so its ordering is not what it claims", want)
		}
	}
}

// Everything the user-data installs has to survive the heredocs it is installed with, and the
// install step has to be invoked: it is what applies the rules, splices the fstab block in and
// mounts, and it is the first boot's ordering guarantee.
func TestInstallerWritesEverything(t *testing.T) {
	out, err := Script(
		plan(t, diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/rancher", "")),
		Options{ConsumerUnits: []string{"kubelet.service"}},
	)
	if err != nil {
		t.Fatalf("Script: %v", err)
	}
	for _, want := range []string{
		"cat <<'" + heredocMarker + "' > '" + ScriptPath + "'",
		"cat <<'" + heredocMarker + "' > '" + ConfigPath + "'",
		"cat <<'" + heredocMarker + "' > '" + FstabBlockPath + "'",
		"cat <<'" + heredocMarker + "' > '" + RulesPath + "'",
		"cat <<'" + heredocMarker + "' > '/etc/systemd/system/" + UnitName + "'",
		"cat <<'" + heredocMarker +
			"' > '/etc/systemd/system/kubelet.service.d/10-karpenter-disks.conf'",
		"Requires=" + UnitName,
		"RequiresMountsFor=/var/lib/rancher",
		"'" + ScriptPath + "' install",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered user-data does not contain %q", want)
		}
	}
	if !strings.Contains(out, setupScript) {
		t.Error("the node-side body is not embedded verbatim in the user-data")
	}
}

// The udev rule is the load-bearing piece: it is what turns a serial into a name everything else
// keys on. ENV{DEVTYPE}=="disk" keeps it off partitions, whose kernel names also match nvme*n*.
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
	// The boot disk is excluded by never being named, not by being matched and skipped.
	if strings.Contains(rules, built.Boot.Serial) {
		t.Error("the rules name the boot disk's serial")
	}
}

// A serial carrying an fnmatch metacharacter would silently become a pattern in a udev rule and could
// match a disk the nodeclass never named. Karpenter's names cannot contain one, which is why it is
// worth asserting rather than assuming.
func TestUdevRulesRefuseAGlobbingSerial(t *testing.T) {
	built := &storage.Plan{Sets: []storage.Set{
		{
			Name: "kubernetes",
			Disks: []storage.Disk{
				{Name: "kd-abc", Serial: "kd-abc*", Filesystem: v1alpha1.FilesystemExt4},
			},
		},
	}}
	if _, err := UdevRules(built); err == nil {
		t.Error("a serial containing a glob metacharacter was accepted")
	}
}

// fstab is rendered once, at launch, rather than rewritten on the node every boot. That is only
// possible because the device name is knowable here, so it is worth asserting on exactly.
func TestFstabBlock(t *testing.T) {
	built := plan(t,
		diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/rancher", ""),
		diskSet("raw", 1, v1alpha1.FilesystemNone, "", "/mnt/raw"),
	)
	fstab, err := FstabBlock(built)
	if err != nil {
		t.Fatalf("FstabBlock: %v", err)
	}
	want := "/dev/karpenter/kubernetes-0 /var/lib/rancher ext4 " + mountOptions + " 0 0\n"
	if fstab != want {
		t.Errorf("fstab block =\n%q\nwant\n%q", fstab, want)
	}
	if strings.Contains(fstab, "nofail") {
		t.Error("an fstab entry gained nofail, which would let the kubelet start without its disks")
	}
}

// A read-only attachment cannot be mounted rw whatever the mount options say, so the entry has to
// carry ro. It is known here rather than probed on the node: it is a property of the attachment.
func TestFstabBlockMarksReadOnlyAttachments(t *testing.T) {
	built := &storage.Plan{Sets: []storage.Set{{
		Name: "sourced",
		Disks: []storage.Disk{{
			Name: "kd-abc", Serial: "kd-abc", MountPoint: "/mnt/sourced", ReadOnly: true,
		}},
	}}}
	fstab, err := FstabBlock(built)
	if err != nil {
		t.Fatalf("FstabBlock: %v", err)
	}
	if !strings.Contains(fstab, ","+"ro ") {
		t.Errorf("a read-only attachment is not mounted ro:\n%s", fstab)
	}
	// It carries no filesystem in the plan, so the entry cannot name one either.
	if !strings.Contains(fstab, " "+fstabAuto+" ") {
		t.Errorf("a sourced disk's entry does not use %q:\n%s", fstabAuto, fstab)
	}
}

// The table is what is left for the script after udev and systemd have taken their halves: which
// device, what to format it as, and the paths it needs for seeding and linking.
func TestConfigTable(t *testing.T) {
	built := plan(t,
		diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/rancher", ""),
		diskSet("local-pv", 2, v1alpha1.FilesystemXFS, "", "/mnt/local-pv"),
		diskSet("raw", 1, v1alpha1.FilesystemNone, "", "/mnt/raw"),
	)
	config, err := Config(built)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}

	var rows []string
	for line := range strings.SplitSeq(strings.TrimSpace(config), "\n") {
		if !strings.HasPrefix(line, "#") {
			rows = append(rows, line)
		}
	}
	want := []string{
		"/dev/karpenter/kubernetes-0\text4\t/var/lib/rancher\t-",
		"/dev/karpenter/local-pv-0\txfs\t/mnt/local-pv/local-pv-0\t-",
		"/dev/karpenter/local-pv-1\txfs\t/mnt/local-pv/local-pv-1\t-",
		"/dev/karpenter/raw-0\tnone\t-\t/mnt/raw/raw-0",
	}
	if strings.Join(rows, "\n") != strings.Join(want, "\n") {
		t.Errorf("table rows =\n%s\nwant\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}

	// Nothing in the table identifies a disk: that is the udev rule's job, and having it in two
	// places is what the previous design got wrong.
	for _, disk := range built.Data() {
		if strings.Contains(config, disk.Serial) {
			t.Errorf(
				"the table carries the serial %q, which only the udev rule should",
				disk.Serial,
			)
		}
	}
}

// A disk created from an image or a snapshot carries no filesystem in the plan, and the table has to
// say "leave whatever is on it alone" rather than "format it as nothing".
func TestSourcedDiskIsNeverFormatted(t *testing.T) {
	set := diskSet("sourced", 1, "", "/mnt/sourced", "")
	set.ImageSelector = []v1alpha1.ImageSelector{{Name: "some-image"}}
	nc := nodeClass(set)
	nc.Status.Storage = &v1alpha1.StorageStatus{
		DiskSets: []v1alpha1.DiskSetStatus{{
			Name:   "sourced",
			Images: []v1alpha1.OxideImage{{ID: "11111111-1111-1111-1111-111111111111"}},
		}},
	}
	built, err := storage.Build("inst-test", nc)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}
	config, err := Config(&built)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if !strings.Contains(config, "\t"+fieldNone+"\t/mnt/sourced\t"+fieldNone+"\n") {
		t.Errorf("a sourced disk is not marked as already carrying a filesystem:\n%s", config)
	}
}

// The CRD's patterns and the storage planner both refuse these, so reaching Config with one means
// something got past both. It has to be an error rather than an escape: a space breaks the fstab
// entry the script writes, and a tab breaks the table's own framing.
func TestConfigRefusesUnrepresentableValues(t *testing.T) {
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
				Filesystem: v1alpha1.FilesystemExt4,
				MountPoint: mountPoint,
			}},
		}}}
		if _, err := Config(built); err == nil {
			t.Errorf("a mount point containing %s was accepted", name)
		}
	}
}

// The three things the script exists for. Each replaces something udev or systemd cannot do, and
// each would fail silently and late rather than loudly: an untuned mkfs only shows up as a slow
// first boot, a missing seed as an agent whose binaries have vanished, a missing relabel as a
// container runtime that cannot write.
func TestScriptKeepsWhatSystemdCannotDo(t *testing.T) {
	for name, want := range map[string]string{
		"tuned mkfs":  "-E nodiscard,lazy_itable_init=1,lazy_journal_init=1",
		"seeding":     `cp -a "$mp/." "$seed/"`,
		"relabelling": `restorecon -R -F "$mp"`,
	} {
		if !strings.Contains(setupScript, want) {
			t.Errorf("the script no longer does %s (looked for %q)", name, want)
		}
	}
}

// The install step is what makes the first boot synchronous, and every part of it has to be there:
// the rules are inert until udev is told to reload, and fstab is inert until systemd regenerates its
// mount units from it.
func TestInstallStepAppliesEverything(t *testing.T) {
	for _, want := range []string{
		"udevadm control --reload",
		"udevadm trigger --subsystem-match=block --action=change",
		"merge_fstab",
		"systemctl daemon-reload",
		`systemd-escape --path --suffix=mount`,
	} {
		if !strings.Contains(setupScript, want) {
			t.Errorf("the install step no longer runs %q", want)
		}
	}
}
