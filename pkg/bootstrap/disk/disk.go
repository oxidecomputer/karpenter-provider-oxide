// Package disk renders the part of a node's user-data that prepares the disks a launch created.
//
// It is shared by every bootstrap provider rather than duplicated per flavor. Exactly one thing
// differs between them: the name of the unit that must not start before the mounts exist. That is a
// field in Options.
//
// Almost none of the work is done by code this package writes. udev and systemd do it:
//
//   - A generated udev rule matches each disk on the NVMe serial Nexus derives from the Oxide disk
//     name and gives it a stable /dev/karpenter/<set>-<ordinal>. Guest device numbering is not
//     deterministic, so every disk is matched positively on its serial and anything that does not
//     match is left strictly alone -- which is how the boot disk is excluded without ever being
//     identified. udev re-creates the symlinks on every boot, which is what makes the name usable in
//     a static fstab and what makes a raw block link stable.
//
//   - Those fstab entries carry x-systemd.requires=karpenter-disks.service, so systemd runs the
//     node-side script before it mounts anything, and RequiresMountsFor on the consumer's drop-in
//     keeps the kubelet down if a mount fails.
//
// What is left for scripts/disk-setup.sh is the three things neither will do: format a blank disk
// with the options we want, seed a mount point whose contents would otherwise be hidden, and relabel
// a fresh filesystem for SELinux. It is a static file, embedded verbatim, identical on every node and
// linted as the shell it is. Everything that varies with the OxideNodeClass is data: a udev rules
// file, an fstab block, and a small table the script reads.
//
// The node writes no manifest. Its job was telling a consumer which device is which, and the udev
// name answers that from the nodeclass alone.
package disk

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
)

// setupScript is the node-side body, embedded verbatim. See scripts/disk-setup.sh.
//
//go:embed scripts/disk-setup.sh
var setupScript string

// unitFile is the unit that runs the body on every boot. Static: it neither names the consumer units,
// which are ordered from their own side by the drop-ins dropInFile renders, nor the mounts, which are
// ordered from theirs by x-systemd.requires=.
//
//go:embed scripts/karpenter-disks.service
var unitFile string

// Options is the per-flavor part of rendering.
type Options struct {
	// ConsumerUnits are the units that must not start before the disks are ready: rke2-agent.service
	// for RKE2, which runs kubelet as a child of the agent, and kubelet.service for kubeadm.
	//
	// Each gets a drop-in requiring the disk unit and requiring the mounts. Requires rather than
	// Wants, deliberately: a kubelet that starts without its data disks advertises ephemeral storage
	// the node does not have and writes pod data onto the root filesystem, quietly filling the boot
	// disk. Better that it stays down and the node fails to register.
	ConsumerUnits []string
}

// Paths this package owns. They are exported because the README documents them and tests assert on
// them, and the script hard-codes the same values, which a test keeps in step.
const (
	// InstallDir holds the script and the data it reads. /var rather than /usr/local, which is
	// read-only on more images than /var is, and rather than /etc, which is for configuration a
	// human edits.
	InstallDir = "/var/lib/karpenter"
	// ScriptPath is where the node-side body is installed.
	ScriptPath = InstallDir + "/disk-setup.sh"
	// ConfigPath is the table of disks the body reads.
	ConfigPath = InstallDir + "/disks.conf"
	// FstabBlockPath holds the lines the install step splices into /etc/fstab. It is staged as a file
	// rather than passed as an argument so that the awk that merges it lives in the linted script
	// rather than in a Go string.
	FstabBlockPath = InstallDir + "/fstab.karpenter"
	// RulesPath is the udev rules file naming this instance's disks. 99- so it runs after the rules
	// the image ships, though SYMLINK+= only adds names and so cannot conflict with them.
	RulesPath = "/etc/udev/rules.d/99-karpenter-disks.rules"
	// DeviceDir is where the udev rules put the stable names. Its contents are the whole of the
	// node's interface to its data disks.
	DeviceDir = "/dev/karpenter"
	// UnitName is the unit that runs the body on every boot, before systemd mounts anything.
	UnitName = "karpenter-disks.service"
	// RunDir holds the error file. It is tmpfs, so it is writable even where the root filesystem is
	// not, and its contents are per-boot.
	RunDir = "/run/karpenter"
	// ErrorPath holds the one-line reason the last run failed. There is no success file to match it:
	// the success contract is the unit's own state and the mount units', which systemd already
	// reports.
	ErrorPath = RunDir + "/disks.error"
)

// heredocMarker delimits every file the installer writes. It is quoted at the point of use, so
// nothing in the content is expanded, and content containing a line equal to it is refused rather
// than escaped.
const heredocMarker = "KARPENTER_INSTALL_EOF"

// Script renders the disk-setup block a bootstrap provider splices into its user-data, immediately
// after "set -euo pipefail" and before it writes its own configuration and joins.
//
// The block writes files and then hands over to the script's install step, which is synchronous and
// exits nonzero on any failure, so under set -e the join is either reached with every disk mounted or
// never reached at all.
//
// A plan with no data disks renders the empty string, so a nodeclass that configures no diskSets gets
// exactly the user-data it got before this existed.
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

// writeFile renders the shell that installs one file, as a quoted heredoc so that nothing in the
// content is expanded by the shell running the user-data.
//
// An empty file is still written: FstabBlockPath is empty for a plan whose disks are all raw block,
// and the install step reads it rather than testing for it.
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

// dropInFile is the drop-in that keeps a consumer unit from starting before the disks are ready.
//
// It is the same content for every consumer, so it is rendered once per plan rather than once per
// unit.
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

// shellQuote renders a value as a single-quoted shell literal.
//
// Every value that reaches here is a constant or has already been refused by checkField, so this
// cannot produce a broken literal. It panics rather than escaping if that ever stops being true,
// because silently escaping would turn a validation hole into a shell injection.
func shellQuote(value string) string {
	if strings.ContainsAny(value, "'\\\n\r") {
		panic(fmt.Sprintf(
			"disk: %q reached shell rendering with a character that should have been refused",
			value,
		))
	}
	return "'" + value + "'"
}
