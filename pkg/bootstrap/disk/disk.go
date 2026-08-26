// Package disk prepares attached data disks.
package disk

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
)

//go:embed scripts/disk-setup.sh
var setupScript string

//go:embed scripts/karpenter-disks.service
var unitFile string

// Options is the per-flavor part of rendering.
type Options struct {
	// ConsumerUnits are the units that must not start before the disks are ready.
	ConsumerUnits []string
}

// Paths this package owns.
const (
	// InstallDir holds the script and the data it reads.
	InstallDir = "/var/lib/karpenter"
	// ScriptPath is where the node-side body is installed.
	ScriptPath = InstallDir + "/disk-setup.sh"
	// ConfigPath is the table of disks the body reads.
	ConfigPath = InstallDir + "/disks.conf"
	// FstabBlockPath holds the lines the install step splices into /etc/fstab.
	FstabBlockPath = InstallDir + "/fstab.karpenter"
	// RulesPath is the udev rules file naming this instance's disks.
	RulesPath = "/etc/udev/rules.d/99-karpenter-disks.rules"
	// DeviceDir is where the udev rules put the stable names.
	DeviceDir = "/dev/karpenter"
	// UnitName is the unit that runs the body on every boot, before systemd mounts anything.
	UnitName = "karpenter-disks.service"
	// RunDir holds the error file.
	RunDir = "/run/karpenter"
	// ErrorPath holds the one-line reason the last run failed.
	ErrorPath = RunDir + "/disks.error"
)

const heredocMarker = "KARPENTER_INSTALL_EOF"

// Script renders the disk-setup block a bootstrap provider splices into its user-data.
func Script(plan *storage.Plan, opts Options) (string, error) {
	if plan == nil || len(plan.Data()) == 0 {
		return "", nil
	}

	config, err := Config(plan)
	if err != nil {
		return "", err
	}
	rules, err := UdevRules(plan)
	if err != nil {
		return "", err
	}
	fstab, err := FstabBlock(plan)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	out.WriteString("### BEGIN karpenter disk setup (generated; edit the OxideNodeClass instead)\n")
	fmt.Fprintf(&out, "install -d -m 0755 %s\n", shellQuote(InstallDir))

	files := []struct{ path, mode, content string }{
		{ScriptPath, "0755", setupScript},
		{ConfigPath, "0644", config},
		{FstabBlockPath, "0644", fstab},
		{RulesPath, "0644", rules},
		{"/etc/systemd/system/" + UnitName, "0644", unitFile},
	}
	for _, unit := range opts.ConsumerUnits {
		dir := "/etc/systemd/system/" + unit + ".d"
		fmt.Fprintf(&out, "install -d -m 0755 %s\n", shellQuote(dir))
		files = append(files, struct{ path, mode, content string }{
			dir + "/10-karpenter-disks.conf", "0644", dropInFile(plan),
		})
	}
	for _, file := range files {
		if err := writeFile(&out, file.path, file.mode, file.content); err != nil {
			return "", err
		}
	}

	out.WriteString(
		"# Applies the udev rules, splices the fstab block in, formats what needs formatting and\n" +
			"# mounts everything, in that order and synchronously. Nothing below this line in the\n" +
			"# user-data runs on a node whose disks did not come up.\n",
	)
	fmt.Fprintf(&out, "%s install\n", shellQuote(ScriptPath))
	out.WriteString("### END karpenter disk setup\n")
	return out.String(), nil
}

func writeFile(out *strings.Builder, path, mode, content string) error {
	for line := range strings.Lines(content) {
		if strings.TrimRight(line, "\n") == heredocMarker {
			return fmt.Errorf(
				"disk: the content of %s contains a line equal to the heredoc marker %s",
				path, heredocMarker,
			)
		}
	}
	fmt.Fprintf(out, "cat <<'%s' > %s\n", heredocMarker, shellQuote(path))
	out.WriteString(content)
	if content != "" && !strings.HasSuffix(content, "\n") {
		out.WriteString("\n")
	}
	fmt.Fprintf(out, "%s\n", heredocMarker)
	fmt.Fprintf(out, "chmod %s %s\n\n", mode, shellQuote(path))
	return nil
}

func dropInFile(plan *storage.Plan) string {
	var out strings.Builder
	out.WriteString("[Unit]\n")
	out.WriteString(
		"# Requires rather than Wants: a kubelet that starts without its data disks advertises\n" +
			"# ephemeral storage the node does not have and writes pod data onto the root filesystem,\n" +
			"# quietly filling the boot disk. Better that it stays down and the node never registers.\n",
	)
	fmt.Fprintf(&out, "Requires=%s\n", UnitName)
	fmt.Fprintf(&out, "After=%s\n", UnitName)

	mounts := make([]string, 0, len(plan.MountOrder()))
	for _, disk := range plan.MountOrder() {
		mounts = append(mounts, disk.MountPoint)
	}
	if len(mounts) > 0 {
		out.WriteString(
			"# Each path becomes a dependency on the .mount unit systemd generates from our fstab\n" +
				"# entries, so a disk that cannot be mounted keeps this unit down instead of letting it\n" +
				"# start on an empty directory. This is why those entries must never carry nofail.\n",
		)
		fmt.Fprintf(&out, "RequiresMountsFor=%s\n", strings.Join(mounts, " "))
	}
	return out.String()
}

func shellQuote(value string) string {
	if strings.ContainsAny(value, "'\\\n\r") {
		panic(fmt.Sprintf(
			"disk: %q reached shell rendering with a character that should have been refused",
			value,
		))
	}
	return "'" + value + "'"
}
