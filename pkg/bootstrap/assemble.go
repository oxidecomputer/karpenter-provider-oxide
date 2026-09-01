package bootstrap

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/disk"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Spec is the part of the node script a bootstrap provider owns: one config
// document and the command that consumes it.
type Spec struct {
	// Storage is the resolved disk layout, or nil when the nodeclass declares no disks.
	Storage *storage.Plan
	// ConsumerUnit is the unit that must not start before the disks are ready.
	ConsumerUnit string
	// ConfigPath is where Config is installed on the node.
	ConfigPath string
	// Config is the provider's config document.
	Config string
	// StartCmd joins the node using the installed config.
	StartCmd string
}

// Assemble renders the full node script: the preamble MergeUserData splices
// against, the disk setup, the config document, and the command that starts the
// node, then merges the nodeclass's own environment and user-data into it.
//
// Providers go through here rather than templating their own script so the
// preamble stays the one MergeUserData expects and the config heredoc keeps the
// marker guard.
func Assemble(
	ctx context.Context,
	reader client.Reader,
	nodeClass *v1alpha1.OxideNodeClass,
	spec Spec,
) (string, error) {
	if !path.IsAbs(spec.ConfigPath) || strings.ContainsAny(spec.ConfigPath, "'\\\n\r") {
		return "", fmt.Errorf("config path %q is not a plain absolute path", spec.ConfigPath)
	}

	var out strings.Builder
	out.WriteString(bashPreamble)

	diskSetup, err := disk.Script(spec.Storage, disk.Options{
		ConsumerUnits: []string{spec.ConsumerUnit},
	})
	if err != nil {
		return "", fmt.Errorf("unable to render disk setup: %w", err)
	}
	if diskSetup != "" {
		out.WriteString(diskSetup)
		out.WriteString("\n")
	}

	fmt.Fprintf(&out, "install -d -m 0755 '%s'\n", path.Dir(spec.ConfigPath))
	// 0600: every flavor's config carries the join credential.
	if err := disk.WriteFile(&out, spec.ConfigPath, "0600", spec.Config); err != nil {
		return "", err
	}

	out.WriteString(spec.StartCmd)
	if !strings.HasSuffix(spec.StartCmd, "\n") {
		out.WriteString("\n")
	}
	return MergeUserData(ctx, reader, nodeClass, out.String())
}
