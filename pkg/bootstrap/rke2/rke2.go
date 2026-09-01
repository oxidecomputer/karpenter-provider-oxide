// Package rke2 bootstraps RKE2 agents.
package rke2

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	supervisorDefaultPort = 9345

	configPath = "/etc/rancher/rke2/config.yaml"

	consumerUnit = "rke2-agent.service"
)

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
	reader client.Reader
}

func NewBootstrapper(reader client.Reader) *Bootstrapper {
	return &Bootstrapper{reader: reader}
}

func (b *Bootstrapper) Name() string {
	return v1alpha1.BootstrapKindRKE2
}

// Resolve finds the RKE2 supervisor and agent token.
func (b *Bootstrapper) Resolve(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	cluster bootstrap.Cluster,
) error {
	if cluster.ServerOverride != "" {
		nodeClass.Status.Server = cluster.ServerOverride
	} else {

		nodeClass.Status.Server = "https://" + net.JoinHostPort(
			cluster.Host,
			strconv.Itoa(supervisorDefaultPort),
		)
	}

	tokenRef := nodeClass.Spec.Bootstrap.RKE2.TokenRefSecret
	if tokenRef.Name == "" {
		return nil
	}

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
		KubeletArgs: bootstrap.KubeletArgs(req.InstanceType, nodeClass.Status.KubeletArgs),
		NodeTaints:  req.NodeTaints,
		Token:       token,
	})
	if err != nil {
		return "", fmt.Errorf("unable to marshal RKE2 config to YAML: %w", err)
	}

	return bootstrap.Assemble(ctx, b.reader, nodeClass, bootstrap.Spec{
		Storage:      req.Storage,
		ConsumerUnit: consumerUnit,
		ConfigPath:   configPath,
		Config:       string(configYAML),
		StartCmd:     "systemctl start " + consumerUnit,
	})
}

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
