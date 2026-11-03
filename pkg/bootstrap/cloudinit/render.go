// Package cloudinit renders user-data for images that boot with cloud-init: a
// cloud-config part that prepares the data disks and a shell part that joins
// the node, in one MIME multipart document when both are present.
package cloudinit

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/disk"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Renderer implements bootstrap.Renderer for cloud-init.
type Renderer struct {
	reader client.Reader
}

// New returns a Renderer that resolves Secret-backed environment through reader.
func New(reader client.Reader) *Renderer {
	return &Renderer{reader: reader}
}

// Render produces the user-data for one node. Without data disks it is the bare
// shell script, which cloud-init runs as-is.
func (r *Renderer) Render(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	spec bootstrap.Spec,
) (string, error) {
	if !path.IsAbs(spec.ConfigPath) || strings.ContainsAny(spec.ConfigPath, "'\\\n\r") {
		return "", fmt.Errorf("config path %q is not a plain absolute path", spec.ConfigPath)
	}
	env, err := bootstrap.ResolveEnv(ctx, r.reader, nodeClass)
	if err != nil {
		return "", err
	}
	layout, err := disk.Build(spec.Storage)
	if err != nil {
		return "", fmt.Errorf("unable to lay out the data disks: %w", err)
	}
	script, err := shellScript(env, nodeClass.Spec.UserData, spec, layout)
	if err != nil {
		return "", err
	}
	if layout == nil {
		return script, nil
	}
	config, err := renderCloudConfig(layout, spec.ConsumerUnit)
	if err != nil {
		return "", err
	}
	return assemble([]part{
		{contentType: "text/cloud-config", filename: "karpenter-disks.yaml", body: config},
		{contentType: "text/x-shellscript", filename: "karpenter-bootstrap.sh", body: script},
	})
}
