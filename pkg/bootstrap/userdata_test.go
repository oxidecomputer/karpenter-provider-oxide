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

const testSystemUserData = bashPreamble + "echo system bootstrap\n"

func userDataReader(objects ...client.Object) client.Reader {
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
}

func TestMergeUserDataLeavesUncustomizedSystemUserDataUnchanged(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{}
	got, err := MergeUserData(context.Background(), userDataReader(), nodeClass, testSystemUserData)
	if err != nil {
		t.Fatalf("MergeUserData() error = %v", err)
	}
	if got != testSystemUserData {
		t.Errorf("MergeUserData() = %q, want %q", got, testSystemUserData)
	}
}

func TestMergeUserDataOrdersEnvironmentUserAndSystemSections(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			UserData: "#!/bin/bash\necho custom setup",
			Env: []v1alpha1.EnvVar{
				{Name: "HTTP_PROXY", Value: "http://proxy.example:3128/a'b"},
			},
		},
	}
	got, err := MergeUserData(context.Background(), userDataReader(), nodeClass, testSystemUserData)
	if err != nil {
		t.Fatalf("MergeUserData() error = %v", err)
	}

	environment := strings.Index(got, "export HTTP_PROXY='http://proxy.example:3128/a'\\''b'")
	custom := strings.Index(got, "echo custom setup")
	system := strings.Index(got, "echo system bootstrap")
	if environment < len(bashPreamble) || custom < 0 || system < 0 {
		t.Fatalf("merged user-data is missing a section:\n%s", got)
	}
	if environment >= custom || custom >= system {
		t.Errorf(
			"merged sections are out of order: environment=%d custom=%d system=%d",
			environment,
			custom,
			system,
		)
	}
	if !strings.Contains(got, "echo custom setup\necho system bootstrap") {
		t.Errorf(
			"custom user-data without a trailing newline was not separated from system user-data:\n%s",
			got,
		)
	}
}

func TestMergeUserDataResolvesSecretEnvironmentValues(t *testing.T) {
	secret := &corev1.Secret{
		Name: "bootstrap-env", Namespace: "karpenter",
		Data: map[string][]byte{"token": []byte("secret\nvalue")},
	}
	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Env: []v1alpha1.EnvVar{{
				Name: "BOOTSTRAP_TOKEN",
				ValueFrom: &v1alpha1.ValueFrom{SecretKeyRef: v1alpha1.SecretRef{
					Name: "bootstrap-env", Namespace: "karpenter", Key: "token",
				}},
			}},
		},
	}
	got, err := MergeUserData(
		context.Background(),
		userDataReader(secret),
		nodeClass,
		testSystemUserData,
	)
	if err != nil {
		t.Fatalf("MergeUserData() error = %v", err)
	}
	if !strings.Contains(got, "export BOOTSTRAP_TOKEN='secret\nvalue'\n") {
		t.Errorf("merged user-data does not contain the Secret value:\n%s", got)
	}
}

func TestMergeUserDataRejectsInvalidEnvironment(t *testing.T) {
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClass := &v1alpha1.OxideNodeClass{
				Spec: v1alpha1.OxideNodeClassSpec{Env: []v1alpha1.EnvVar{tt.env}},
			}
			_, err := MergeUserData(
				context.Background(),
				userDataReader(),
				nodeClass,
				testSystemUserData,
			)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("MergeUserData() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestMergeUserDataReportsMissingSecretKey(t *testing.T) {
	secret := &corev1.Secret{
		Name: "bootstrap-env", Namespace: "karpenter",
	}
	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Env: []v1alpha1.EnvVar{{
				Name: "BOOTSTRAP_TOKEN",
				ValueFrom: &v1alpha1.ValueFrom{SecretKeyRef: v1alpha1.SecretRef{
					Name: "bootstrap-env", Namespace: "karpenter", Key: "token",
				}},
			}},
		},
	}
	_, err := MergeUserData(
		context.Background(),
		userDataReader(secret),
		nodeClass,
		testSystemUserData,
	)
	if err == nil || !strings.Contains(err.Error(), `has no key "token"`) {
		t.Fatalf("MergeUserData() error = %v, want a missing Secret key error", err)
	}
}
