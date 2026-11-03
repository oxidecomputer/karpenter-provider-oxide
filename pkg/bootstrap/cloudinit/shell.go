package cloudinit

import (
	"fmt"
	"path"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/disk"
)

const (
	preamble = "#!/usr/bin/env bash\nset -euo pipefail\n"

	heredocMarker = "KARPENTER_INSTALL_EOF"
)

// shellScript renders the part that joins the node, in this order: the
// environment, the nodeclass's own user-data, the disk checks, the config, the
// start command. cloud-init runs it after the cloud-config modules, so the
// disks are already mounted when the user's fragment runs.
func shellScript(
	env []bootstrap.EnvVar,
	userData string,
	spec bootstrap.Spec,
	layout *disk.Layout,
) (string, error) {
	var out strings.Builder
	out.WriteString(preamble)
	for _, variable := range env {
		fmt.Fprintf(&out, "export %s=%s\n", variable.Name, shellQuote(variable.Value))
	}
	if userData != "" {
		out.WriteString(userData)
		if !strings.HasSuffix(userData, "\n") {
			out.WriteByte('\n')
		}
	}
	if layout != nil {
		writeDiskChecks(&out, layout)
	}
	fmt.Fprintf(&out, "install -d -m 0755 %s\n", shellQuote(path.Dir(spec.ConfigPath)))
	// 0600: every flavor's config carries the join credential.
	if err := installFile(&out, spec.ConfigPath, "0600", spec.Config); err != nil {
		return "", err
	}
	out.WriteString(spec.StartCmd)
	if !strings.HasSuffix(spec.StartCmd, "\n") {
		out.WriteString("\n")
	}
	return out.String(), nil
}

func writeDiskChecks(out *strings.Builder, layout *disk.Layout) {
	if len(layout.Mounts) == 0 {
		return
	}
	var all, formatted []string
	for _, mount := range layout.Mounts {
		all = append(all, shellQuote(mount.MountPoint))
		if mount.Formatted {
			formatted = append(formatted, shellQuote(mount.MountPoint))
		}
	}
	out.WriteString(
		"# cloud-init mounted the data disks before this ran; never join without them.\n",
	)
	fmt.Fprintf(out, "for mp in %s; do\n", strings.Join(all, " "))
	out.WriteString(`  mountpoint -q "$mp" || ` +
		`{ echo "karpenter: $mp is not mounted, see cloud-init-output.log" >&2; exit 1; }` + "\n")
	out.WriteString("done\n")
	if len(formatted) > 0 {
		out.WriteString(
			"# A new filesystem carries no SELinux labels; apply the ones its path calls for.\n",
		)
		fmt.Fprintf(out,
			"if command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled; then "+
				"restorecon -R -F %s; fi\n",
			strings.Join(formatted, " "),
		)
	}
}

// installFile appends a guarded heredoc that installs content at path with mode.
// The marker is checked against the content so a document that happens to
// contain it fails here instead of truncating the script on the node.
func installFile(out *strings.Builder, filePath, mode, content string) error {
	for line := range strings.Lines(content) {
		if strings.TrimRight(line, "\n") == heredocMarker {
			return fmt.Errorf(
				"the content of %s contains a line equal to the heredoc marker %s",
				filePath, heredocMarker,
			)
		}
	}
	fmt.Fprintf(out, "cat <<'%s' > %s\n", heredocMarker, shellQuote(filePath))
	out.WriteString(content)
	if content != "" && !strings.HasSuffix(content, "\n") {
		out.WriteString("\n")
	}
	fmt.Fprintf(out, "%s\n", heredocMarker)
	fmt.Fprintf(out, "chmod %s %s\n", mode, shellQuote(filePath))
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
