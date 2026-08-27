package bootstrap

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const bashPreamble = "#!/usr/bin/env bash\nset -euo pipefail\n"

var shellVariableName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// MergeUserData adds the user-defined environment and user-data to the provider-generated
// bootstrap script. The provider preamble stays first, user-data runs next, and the
// provider-controlled bootstrap steps run last.
func MergeUserData(
	ctx context.Context,
	reader client.Reader,
	nodeClass *v1alpha1.OxideNodeClass,
	systemUserData string,
) (string, error) {
	if nodeClass.Spec.UserData == "" && len(nodeClass.Spec.Env) == 0 {
		return systemUserData, nil
	}
	if !strings.HasPrefix(systemUserData, bashPreamble) {
		return "", fmt.Errorf("system user-data does not begin with the expected bash preamble")
	}

	environment, err := resolveEnvironment(ctx, reader, nodeClass.Spec.Env)
	if err != nil {
		return "", err
	}

	var merged strings.Builder
	merged.Grow(len(systemUserData) + len(nodeClass.Spec.UserData) + len(environment))
	merged.WriteString(bashPreamble)
	merged.WriteString(environment)
	writeSection(&merged, nodeClass.Spec.UserData)
	merged.WriteString(strings.TrimPrefix(systemUserData, bashPreamble))
	return merged.String(), nil
}

func resolveEnvironment(
	ctx context.Context,
	reader client.Reader,
	env []v1alpha1.EnvVar,
) (string, error) {
	var rendered strings.Builder
	for _, variable := range env {
		if !shellVariableName.MatchString(variable.Name) {
			return "", fmt.Errorf(
				"environment variable name %q is not a valid shell variable name",
				variable.Name,
			)
		}
		if variable.Value != "" && variable.ValueFrom != nil {
			return "", fmt.Errorf(
				"environment variable %q has both value and valueFrom set",
				variable.Name,
			)
		}

		value := variable.Value
		if variable.ValueFrom != nil {
			ref := variable.ValueFrom.SecretKeyRef
			secret := &corev1.Secret{}
			key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
			if err := reader.Get(ctx, key, secret); err != nil {
				return "", fmt.Errorf(
					"unable to retrieve Secret %s for environment variable %q: %w",
					key,
					variable.Name,
					err,
				)
			}
			secretValue, ok := secret.Data[ref.Key]
			if !ok {
				return "", fmt.Errorf(
					"secret %s has no key %q for environment variable %q",
					key,
					ref.Key,
					variable.Name,
				)
			}
			value = string(secretValue)
		}
		if strings.ContainsRune(value, '\x00') {
			return "", fmt.Errorf("environment variable %q contains a NUL byte", variable.Name)
		}

		fmt.Fprintf(&rendered, "export %s=%s\n", variable.Name, shellQuote(value))
	}
	return rendered.String(), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func writeSection(out *strings.Builder, section string) {
	if section == "" {
		return
	}
	out.WriteString(section)
	if !strings.HasSuffix(section, "\n") {
		out.WriteByte('\n')
	}
}
