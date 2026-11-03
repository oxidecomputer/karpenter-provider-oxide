package bootstrap

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	tokenNamespace    = "kube-system"
	tokenSecretPrefix = "bootstrap-token-"

	// TokenTTL matches Karpenter's registration timeout: a node that has not joined by then is torn down anyway.
	TokenTTL = 15 * time.Minute

	tokenIDLength     = 6
	tokenSecretLength = 16
	tokenAlphabet     = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// MintToken creates a bootstrap token Secret for one NodeClaim and returns the token as "<id>.<secret>".
// Both kubeadm and the RKE2 supervisor authenticate joins with this standard Kubernetes token type.
func MintToken(
	ctx context.Context,
	writer client.Writer,
	nodeClass *v1alpha1.OxideNodeClass,
	nodeClaim *karpcorev1.NodeClaim,
	extraGroups ...string,
) (string, error) {
	tokenID, err := randomString(tokenIDLength)
	if err != nil {
		return "", err
	}
	tokenSecret, err := randomString(tokenSecretLength)
	if err != nil {
		return "", err
	}

	secret := &corev1.Secret{
		Name:      tokenSecretPrefix + tokenID,
		Namespace: tokenNamespace,
		Labels: map[string]string{
			v1alpha1.LabelNodeClass: nodeClass.Name,
		},
		Type: corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			"description": fmt.Sprintf(
				"Karpenter bootstrap token for NodeClaim %s",
				nodeClaim.Name,
			),
			"token-id":                       tokenID,
			"token-secret":                   tokenSecret,
			"expiration":                     time.Now().Add(TokenTTL).UTC().Format(time.RFC3339),
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
		},
	}
	if len(extraGroups) > 0 {
		secret.StringData["auth-extra-groups"] = strings.Join(extraGroups, ",")
	}
	if err := writer.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("unable to create the bootstrap token secret: %w", err)
	}
	return tokenID + "." + tokenSecret, nil
}

func randomString(length int) (string, error) {
	out := make([]byte, length)
	if _, err := rand.Read(out); err != nil {
		return "", fmt.Errorf("unable to generate a random bootstrap token: %w", err)
	}
	for i, b := range out {
		out[i] = tokenAlphabet[int(b)%len(tokenAlphabet)]
	}
	return string(out), nil
}
