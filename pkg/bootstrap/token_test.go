package bootstrap_test

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestMintTokenWritesAStandardBootstrapToken(t *testing.T) {
	kubeClient := fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	nodeClass := &v1alpha1.OxideNodeClass{Name: "default"}
	nodeClaim := &karpv1.NodeClaim{Name: "node-1"}

	token, err := bootstrap.MintToken(
		context.Background(),
		kubeClient,
		nodeClass,
		nodeClaim,
		"system:bootstrappers:x",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`).MatchString(token) {
		t.Fatalf("token %q is not in kubeadm's <id>.<secret> form", token)
	}

	secrets := &corev1.SecretList{}
	if err := kubeClient.List(
		context.Background(), secrets, client.InNamespace("kube-system"),
	); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 1 {
		t.Fatalf("got %d secrets, want 1", len(secrets.Items))
	}
	secret := secrets.Items[0]
	if secret.Name != "bootstrap-token-"+token[:6] {
		t.Errorf("secret %q is not named after the token id", secret.Name)
	}
	if secret.Type != corev1.SecretTypeBootstrapToken {
		t.Errorf("secret type %q", secret.Type)
	}
	if secret.Labels[v1alpha1.LabelNodeClass] != "default" {
		t.Error("secret is not labeled with its nodeclass")
	}
	if secret.StringData["auth-extra-groups"] != "system:bootstrappers:x" {
		t.Errorf("extra groups %q", secret.StringData["auth-extra-groups"])
	}
	expiration, err := time.Parse(time.RFC3339, secret.StringData["expiration"])
	if err != nil {
		t.Fatal(err)
	}
	remaining := time.Until(expiration)
	if remaining > bootstrap.TokenTTL || remaining < bootstrap.TokenTTL-time.Minute {
		t.Errorf("token expires in %s, want about %s", remaining, bootstrap.TokenTTL)
	}
}

func TestMintTokenOmitsExtraGroupsWhenNoneAreGiven(t *testing.T) {
	kubeClient := fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	if _, err := bootstrap.MintToken(
		context.Background(),
		kubeClient,
		&v1alpha1.OxideNodeClass{Name: "default"},
		&karpv1.NodeClaim{Name: "node-1"},
	); err != nil {
		t.Fatal(err)
	}
	secrets := &corev1.SecretList{}
	if err := kubeClient.List(context.Background(), secrets); err != nil {
		t.Fatal(err)
	}
	if _, ok := secrets.Items[0].StringData["auth-extra-groups"]; ok {
		t.Error("an empty auth-extra-groups value is rejected by the bootstrap authenticator")
	}
}
