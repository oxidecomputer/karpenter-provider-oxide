package bootstrap

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Spec is what a bootstrap flavor hands a Renderer: the disks the node was
// launched with, one config document and the command that consumes it. It says
// nothing about how the node's init system receives any of it.
type Spec struct {
	// Storage is the resolved disk layout, or nil when the nodeclass declares no disks.
	Storage *storage.Plan
	// ConsumerUnit is the systemd unit that must not start before the disks are mounted.
	ConsumerUnit string
	// ConfigPath is where Config is installed on the node.
	ConfigPath string
	// Config is the flavor's config document.
	Config string
	// StartCmd joins the node using the installed config.
	StartCmd string
}

// Renderer turns a Spec into the user-data an image's init system consumes.
// cloud-init is the only implementation today; an image family that boots
// differently, such as Flatcar or NixOS, gets its own.
type Renderer interface {
	Render(ctx context.Context, nodeClass *v1alpha1.OxideNodeClass, spec Spec) (string, error)
}

// EnvVar is one resolved spec.env entry.
type EnvVar struct {
	Name  string
	Value string
}

var envVarName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ResolveEnv reads the nodeclass's environment, fetching the Secret-backed values.
func ResolveEnv(
	ctx context.Context,
	reader client.Reader,
	nodeClass *v1alpha1.OxideNodeClass,
) ([]EnvVar, error) {
	env := make([]EnvVar, 0, len(nodeClass.Spec.Env))
	for _, variable := range nodeClass.Spec.Env {
		if !envVarName.MatchString(variable.Name) {
			return nil, fmt.Errorf(
				"environment variable name %q is not a valid shell variable name",
				variable.Name,
			)
		}
		if variable.Value != "" && variable.ValueFrom != nil {
			return nil, fmt.Errorf(
				"environment variable %q has both value and valueFrom set",
				variable.Name,
			)
		}
		value := variable.Value
		if variable.ValueFrom != nil {
			resolved, err := secretValue(ctx, reader, variable)
			if err != nil {
				return nil, err
			}
			value = resolved
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("environment variable %q contains a NUL byte", variable.Name)
		}
		env = append(env, EnvVar{Name: variable.Name, Value: value})
	}
	return env, nil
}

func secretValue(
	ctx context.Context,
	reader client.Reader,
	variable v1alpha1.EnvVar,
) (string, error) {
	ref := variable.ValueFrom.SecretKeyRef
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := reader.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf(
			"unable to retrieve Secret %s for environment variable %q: %w",
			key, variable.Name, err,
		)
	}
	value, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf(
			"secret %s has no key %q for environment variable %q",
			key, ref.Key, variable.Name,
		)
	}
	return string(value), nil
}
