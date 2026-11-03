package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func envReader(objects ...client.Object) client.Reader {
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
}

func secretRef() *v1alpha1.ValueFrom {
	return &v1alpha1.ValueFrom{SecretKeyRef: v1alpha1.SecretRef{
		Name: "bootstrap-env", Namespace: "karpenter", Key: "token",
	}}
}

func TestResolveEnvKeepsOrderAndLiteralValues(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{Env: []v1alpha1.EnvVar{
			{Name: "HTTP_PROXY", Value: "http://proxy.example:3128/a'b"},
			{Name: "NODE_ROLE", Value: "worker"},
		}},
	}
	got, err := ResolveEnv(context.Background(), envReader(), nodeClass)
	if err != nil {
		t.Fatalf("ResolveEnv() error = %v", err)
	}
	want := []EnvVar{
		{Name: "HTTP_PROXY", Value: "http://proxy.example:3128/a'b"},
		{Name: "NODE_ROLE", Value: "worker"},
	}
	if len(got) != len(want) {
		t.Fatalf("ResolveEnv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ResolveEnv()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestResolveEnvResolvesSecretValues(t *testing.T) {
	secret := &corev1.Secret{
		Name: "bootstrap-env", Namespace: "karpenter",
		Data: map[string][]byte{"token": []byte("secret\nvalue")},
	}
	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Env: []v1alpha1.EnvVar{{Name: "BOOTSTRAP_TOKEN", ValueFrom: secretRef()}},
		},
	}
	got, err := ResolveEnv(context.Background(), envReader(secret), nodeClass)
	if err != nil {
		t.Fatalf("ResolveEnv() error = %v", err)
	}
	if len(got) != 1 || got[0].Value != "secret\nvalue" {
		t.Errorf("ResolveEnv() = %v, want the Secret value", got)
	}
}

func TestResolveEnvRejectsInvalidEnvironment(t *testing.T) {
	tests := []struct {
		name string
		env  v1alpha1.EnvVar
		want string
	}{
		{
			name: "invalid name",
			env:  v1alpha1.EnvVar{Name: "NOT-VALID", Value: "value"},
			want: "not a valid shell variable name",
		},
		{
			name: "value and valueFrom",
			env: v1alpha1.EnvVar{
				Name:      "CONFLICT",
				Value:     "value",
				ValueFrom: &v1alpha1.ValueFrom{},
			},
			want: "both value and valueFrom",
		},
		{
			name: "NUL byte",
			env:  v1alpha1.EnvVar{Name: "BINARY", Value: "a\x00b"},
			want: "NUL byte",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClass := &v1alpha1.OxideNodeClass{
				Spec: v1alpha1.OxideNodeClassSpec{Env: []v1alpha1.EnvVar{tt.env}},
			}
			_, err := ResolveEnv(context.Background(), envReader(), nodeClass)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ResolveEnv() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestResolveEnvReportsMissingSecretKey(t *testing.T) {
	secret := &corev1.Secret{Name: "bootstrap-env", Namespace: "karpenter"}
	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Env: []v1alpha1.EnvVar{{Name: "BOOTSTRAP_TOKEN", ValueFrom: secretRef()}},
		},
	}
	_, err := ResolveEnv(context.Background(), envReader(secret), nodeClass)
	if err == nil || !strings.Contains(err.Error(), `has no key "token"`) {
		t.Fatalf("ResolveEnv() error = %v, want a missing Secret key error", err)
	}
}
