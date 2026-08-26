// Package rke2 joins nodes to an RKE2 cluster by writing an agent config.yaml through user-data and
// starting rke2-agent.
package rke2

import (
	"context"
	"fmt"
	"strings"
	"text/template"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/disk"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// supervisorDefaultPort is the port agents use to join an RKE2 cluster. It is not discoverable
	// through the Kubernetes API, so it is assumed unless the user overrides spec.server outright.
	supervisorDefaultPort = 9345

	userDataTemplateText = `#!/usr/bin/env bash
set -euo pipefail
{{ with .DiskSetup }}{{ . }}
{{ end }}mkdir -p /etc/rancher/rke2
cat <<'EOF' > /etc/rancher/rke2/config.yaml
{{ .Config }}
EOF
systemctl start rke2-agent.service
`

	// consumerUnit is the unit that must not start before the data disks are mounted. RKE2 runs the
	// kubelet as a child of the agent, so the agent is the only consumer to order against.
	consumerUnit = "rke2-agent.service"
)

var userDataTemplate = template.Must(template.New("userdata").Parse(userDataTemplateText))

// Config is the subset of /etc/rancher/rke2/config.yaml that Karpenter manages.
type Config struct {
	Server      string     `yaml:"server"`
	KubeletArgs []string   `yaml:"kubelet-arg"`
	NodeTaints  nodeTaints `yaml:"node-taint"`
	Token       string     `yaml:"token"`
}

type nodeTaints []corev1.Taint

func (n nodeTaints) MarshalYAML() (any, error) {
	out := make([]string, len(n))
	for i, x := range n {
		out[i] = x.ToString()
	}
	return out, nil
}

// Bootstrapper implements bootstrap.Bootstrapper for RKE2.
type Bootstrapper struct {
	// reader is deliberately the manager's uncached reader. Reading the token through the cached
	// client would make controller-runtime start an informer that lists and watches every Secret in
	// the cluster, which would require far broader RBAC than reading one Secret by name.
	reader client.Reader
}

func NewBootstrapper(reader client.Reader) *Bootstrapper {
	return &Bootstrapper{reader: reader}
}

func (b *Bootstrapper) Name() string {
	return v1alpha1.BootstrapKindRKE2
}

// Resolve renders the supervisor URL agents join through and reports whether the configured agent
// token Secret is readable.
func (b *Bootstrapper) Resolve(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	cluster bootstrap.Cluster,
) error {
	if cluster.ServerOverride != "" {
		nodeClass.Status.Server = cluster.ServerOverride
	} else {
		// Agents join through the supervisor, not the kube-apiserver, so the discovered apiserver
		// port is deliberately discarded in favour of the supervisor port.
		nodeClass.Status.Server = fmt.Sprintf("https://%s:%d", cluster.Host, supervisorDefaultPort)
	}

	tokenRef := nodeClass.Spec.Bootstrap.RKE2.TokenRefSecret
	if tokenRef.Name == "" {
		return nil
	}

	// Surface an unreadable or malformed token as a condition rather than an error: it is user
	// misconfiguration that requeueing will not fix.
	secret := &corev1.Secret{}
	tokenSecretObjKey := client.ObjectKey{Namespace: tokenRef.Namespace, Name: tokenRef.Name}
	if err := b.reader.Get(ctx, tokenSecretObjKey, secret); err != nil {
		nodeClass.StatusConditions().
			SetFalse(v1alpha1.ConditionTypeTokenRefSecretAccessible, "TokenKeyAccessErr", err.Error())
	} else if _, ok := secret.Data[tokenRef.Key]; !ok {
		nodeClass.StatusConditions().
			SetFalse(v1alpha1.ConditionTypeTokenRefSecretAccessible, "TokenKeyNotFound", "Token is accessible but key not found")
	} else {
		nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeTokenRefSecretAccessible)
	}
	return nil
}

// UserData writes the agent config.yaml and starts rke2-agent.
func (b *Bootstrapper) UserData(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	req bootstrap.Request,
) (string, error) {
	token, err := b.token(ctx, nodeClass)
	if err != nil {
		return "", err
	}

	configYAML, err := yaml.Marshal(Config{
		Server:      nodeClass.Status.Server,
		KubeletArgs: nodeClass.Status.KubeletArgs,
		NodeTaints:  req.NodeTaints,
		Token:       token,
	})
	if err != nil {
		return "", fmt.Errorf("unable to marshal RKE2 config to YAML: %w", err)
	}

	var rendered strings.Builder
	diskSetup, err := disk.Script(req.Storage, disk.Options{
		ConsumerUnits: []string{consumerUnit},
	})
	if err != nil {
		return "", fmt.Errorf("unable to render disk setup: %w", err)
	}

	if err := userDataTemplate.Execute(&rendered, userData{
		DiskSetup: diskSetup,
		Config:    string(configYAML),
	}); err != nil {
		return "", fmt.Errorf("unable to execute user-data template: %w", err)
	}
	return rendered.String(), nil
}

// token reads the long-lived agent join token from the referenced Secret.
func (b *Bootstrapper) token(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (string, error) {
	tokenRef := nodeClass.Spec.Bootstrap.RKE2.TokenRefSecret
	if tokenRef.Name == "" {
		return "", nil
	}

	secret := &corev1.Secret{}
	tokenSecretObjKey := client.ObjectKey{Namespace: tokenRef.Namespace, Name: tokenRef.Name}
	if err := b.reader.Get(ctx, tokenSecretObjKey, secret); err != nil {
		return "", fmt.Errorf("unable to retrieve the token secret ref, %w", err)
	}
	token, ok := secret.Data[tokenRef.Key]
	if !ok {
		return "", fmt.Errorf(
			"token was not found from secret reference %s key %s",
			tokenSecretObjKey,
			tokenRef.Key,
		)
	}
	return string(token), nil
}

// userData is what the user-data template is rendered from.
//
// DiskSetup is spliced in after "set -euo pipefail" and before the configuration is written, which is
// what guarantees the disks are mounted before the node joins: the block ends by running a oneshot
// unit synchronously, so under set -e the join is never reached on a node whose disks did not come up.
// It is empty for a nodeclass with no diskSets, which renders the script this template rendered
// before disk support existed.
type userData struct {
	DiskSetup string
	Config    string
}
