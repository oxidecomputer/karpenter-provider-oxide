// Package kubeadm joins nodes to a kubeadm-managed cluster.
//
// Discovery information comes from the kube-public/cluster-info ConfigMap, which is world-readable
// by design and holds the cluster CA plus the kube-apiserver endpoint. Rather than distributing a
// long-lived join credential, a bootstrap token is minted per NodeClaim as a Secret in kube-system
// with a short TTL, handed to exactly one node through user-data, and left to expire. The cluster CA
// is pinned by hash so the joining node authenticates the control plane it is told to trust.
package kubeadm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/oxidecomputer/karpenter/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter/pkg/bootstrap"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// clusterInfoNamespace and clusterInfoName identify the ConfigMap kubeadm publishes for
	// unauthenticated discovery. It is intentionally readable by anonymous users.
	clusterInfoNamespace = "kube-public"
	clusterInfoName      = "cluster-info"
	// clusterInfoKubeconfigKey is the key holding the discovery kubeconfig.
	clusterInfoKubeconfigKey = "kubeconfig"

	// tokenNamespace is where the kube-apiserver's bootstrap token authenticator looks for tokens.
	// This location is fixed by Kubernetes and is not configurable.
	tokenNamespace = "kube-system"
	// tokenSecretPrefix is the mandatory name prefix for bootstrap token Secrets.
	tokenSecretPrefix = "bootstrap-token-"
	// tokenSecretType is the mandatory type for bootstrap token Secrets.
	tokenSecretType = corev1.SecretTypeBootstrapToken
	// nodeBootstrapGroup is the group kubeadm's node-bootstrapper RBAC is bound to. Without it the
	// joining node cannot create its own CSR.
	nodeBootstrapGroup = "system:bootstrappers:kubeadm:default-node-token"

	// defaultTokenTTL bounds the window in which a leaked token is usable.
	defaultTokenTTL = 15 * time.Minute
	// defaultAPIServerPort is used when neither cluster-info nor discovery yields a port.
	defaultAPIServerPort = 6443

	// tokenIDLength and tokenSecretLength are fixed by the bootstrap token format, "[a-z0-9]{6}.[a-z0-9]{16}".
	tokenIDLength     = 6
	tokenSecretLength = 16
	// tokenAlphabet is the only alphabet the bootstrap token format permits.
	tokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

	configVersionV1beta3 = "kubeadm.k8s.io/v1beta3"
	configVersionV1beta4 = "kubeadm.k8s.io/v1beta4"

	userDataTemplateText = `#!/usr/bin/env bash
set -euo pipefail
mkdir -p /etc/kubernetes
cat <<'EOF' > /etc/kubernetes/karpenter-join.yaml
{{ . }}
EOF
kubeadm join --config /etc/kubernetes/karpenter-join.yaml
`
)

var userDataTemplate = template.Must(template.New("userdata").Parse(userDataTemplateText))

// Bootstrapper implements bootstrap.Bootstrapper for kubeadm.
type Bootstrapper struct {
	// reader is deliberately the manager's uncached reader. Reading cluster-info through the cached
	// client would make controller-runtime start an informer that lists and watches every ConfigMap
	// in the cluster, which would require far broader RBAC than reading one ConfigMap by name.
	reader client.Reader
	// writer creates bootstrap token Secrets. Writes never go through the cache, so this grants no
	// read access to kube-system.
	writer client.Writer
}

func NewBootstrapper(reader client.Reader, writer client.Writer) *Bootstrapper {
	return &Bootstrapper{reader: reader, writer: writer}
}

func (b *Bootstrapper) Name() string {
	return v1alpha1.BootstrapKindKubeadm
}

// Resolve reads cluster-info and records the endpoint and CA pins nodes will use to discover the
// control plane. No token is minted here; tokens are per-node and short-lived, so they are created
// in UserData instead.
func (b *Bootstrapper) Resolve(ctx context.Context, nodeClass *v1alpha1.OxideNodeClass, cluster bootstrap.Cluster) error {
	clusterInfo, err := b.clusterInfo(ctx)
	if err != nil {
		// cluster-info is expected to exist on every kubeadm cluster, so treat this as user-visible
		// misconfiguration rather than a transient failure to retry against.
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeClusterInfoAvailable, "ClusterInfoUnavailable", err.Error())
		return nil
	}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeClusterInfoAvailable)

	// Prefer the endpoint cluster-info advertises: it is what the control plane says it is reachable
	// at, which may differ from an individual apiserver pod address in the EndpointSlice.
	server := cluster.ServerOverride
	if server == "" {
		server = clusterInfo.endpoint
	}
	if server == "" {
		port := cluster.Port
		if port == 0 {
			port = defaultAPIServerPort
		}
		server = fmt.Sprintf("%s:%d", cluster.Host, port)
	}
	nodeClass.Status.Server = server

	if nodeClass.Status.Bootstrap == nil {
		nodeClass.Status.Bootstrap = &v1alpha1.BootstrapStatus{}
	}
	nodeClass.Status.Bootstrap.Kubeadm = &v1alpha1.KubeadmStatus{
		CACertHashes: clusterInfo.caCertHashes,
		ClusterName:  clusterInfo.clusterName,
	}
	return nil
}

// UserData mints a short-lived bootstrap token for this NodeClaim and renders a JoinConfiguration
// that uses it. The token is created here rather than during Resolve so that each node gets its own
// credential and so that no token exists while no node is waiting to consume it.
func (b *Bootstrapper) UserData(ctx context.Context, nodeClass *v1alpha1.OxideNodeClass, req bootstrap.Request) (string, error) {
	if nodeClass.Status.Bootstrap == nil || nodeClass.Status.Bootstrap.Kubeadm == nil {
		return "", fmt.Errorf("kubeadm discovery information has not been resolved for OxideNodeClass %q", nodeClass.Name)
	}
	if nodeClass.Status.Server == "" {
		return "", fmt.Errorf("no API server endpoint resolved for OxideNodeClass %q", nodeClass.Name)
	}

	token, err := b.createToken(ctx, nodeClass, req)
	if err != nil {
		return "", err
	}

	config, err := b.joinConfiguration(nodeClass, req, token)
	if err != nil {
		return "", err
	}

	var userData strings.Builder
	if err := userDataTemplate.Execute(&userData, config); err != nil {
		return "", fmt.Errorf("unable to execute user-data template: %w", err)
	}
	return userData.String(), nil
}

// clusterInfoData is the discovery information parsed out of kube-public/cluster-info.
type clusterInfoData struct {
	endpoint     string
	clusterName  string
	caCertHashes []string
}

// clusterInfo reads and parses the cluster-info ConfigMap.
func (b *Bootstrapper) clusterInfo(ctx context.Context) (*clusterInfoData, error) {
	configMap := &corev1.ConfigMap{}
	objKey := client.ObjectKey{Namespace: clusterInfoNamespace, Name: clusterInfoName}
	if err := b.reader.Get(ctx, objKey, configMap); err != nil {
		return nil, fmt.Errorf("unable to retrieve the %s/%s ConfigMap: %w", clusterInfoNamespace, clusterInfoName, err)
	}

	kubeconfigData, ok := configMap.Data[clusterInfoKubeconfigKey]
	if !ok {
		return nil, fmt.Errorf("%s/%s ConfigMap has no %q key", clusterInfoNamespace, clusterInfoName, clusterInfoKubeconfigKey)
	}

	kubeconfig, err := clientcmd.Load([]byte(kubeconfigData))
	if err != nil {
		return nil, fmt.Errorf("unable to parse the cluster-info kubeconfig: %w", err)
	}

	// The discovery kubeconfig carries exactly one cluster, but its name is not fixed, so take the
	// first entry rather than looking one up by name.
	for name, cluster := range kubeconfig.Clusters {
		if cluster == nil {
			continue
		}
		hashes, err := caCertHashes(cluster.CertificateAuthorityData)
		if err != nil {
			return nil, err
		}
		return &clusterInfoData{
			endpoint:     apiServerEndpoint(cluster.Server),
			clusterName:  name,
			caCertHashes: hashes,
		}, nil
	}
	return nil, fmt.Errorf("cluster-info kubeconfig contains no clusters")
}

// apiServerEndpoint reduces a server URL to the "host:port" form kubeadm discovery expects.
func apiServerEndpoint(server string) string {
	if server == "" {
		return ""
	}
	parsed, err := url.Parse(server)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Port() == "" {
		return fmt.Sprintf("%s:%d", parsed.Host, defaultAPIServerPort)
	}
	return parsed.Host
}

// caCertHashes computes the "sha256:<hex>" pins kubeadm uses to verify the control plane during
// token discovery. The hash covers the certificate's SubjectPublicKeyInfo, so it survives CA
// certificate renewal that reuses the same key.
func caCertHashes(caCertData []byte) ([]string, error) {
	var hashes []string
	for block, rest := pem.Decode(caCertData); block != nil; block, rest = pem.Decode(rest) {
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("unable to parse the cluster CA certificate: %w", err)
		}
		sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		hashes = append(hashes, "sha256:"+hex.EncodeToString(sum[:]))
	}
	if len(hashes) == 0 {
		return nil, fmt.Errorf("cluster-info kubeconfig contains no CA certificate")
	}
	return hashes, nil
}

// createToken mints a bootstrap token Secret scoped to a single node join.
//
// The token is granted only usage-bootstrap-authentication: signing the cluster-info JWS is
// unnecessary because the node pins the CA by hash instead of trusting a signed cluster-info. The
// Secret is never read back, and expired tokens are garbage collected by the control plane's
// tokencleaner controller, so the controller needs no read or delete access to kube-system Secrets.
func (b *Bootstrapper) createToken(ctx context.Context, nodeClass *v1alpha1.OxideNodeClass, req bootstrap.Request) (string, error) {
	tokenID, err := randomString(tokenIDLength)
	if err != nil {
		return "", err
	}
	tokenSecret, err := randomString(tokenSecretLength)
	if err != nil {
		return "", err
	}

	ttl := defaultTokenTTL
	if nodeClass.Spec.Bootstrap.Kubeadm.TokenTTL != nil {
		ttl = nodeClass.Spec.Bootstrap.Kubeadm.TokenTTL.Duration
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tokenSecretPrefix + tokenID,
			Namespace: tokenNamespace,
			Labels: map[string]string{
				v1alpha1.LabelNodeClass: nodeClass.Name,
			},
		},
		Type: tokenSecretType,
		StringData: map[string]string{
			"description":                    fmt.Sprintf("Karpenter bootstrap token for NodeClaim %s", req.NodeClaim.Name),
			"token-id":                       tokenID,
			"token-secret":                   tokenSecret,
			"expiration":                     time.Now().Add(ttl).UTC().Format(time.RFC3339),
			"usage-bootstrap-authentication": "true",
			"auth-extra-groups":              nodeBootstrapGroup,
		},
	}
	if err := b.writer.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("unable to create the bootstrap token secret: %w", err)
	}
	return fmt.Sprintf("%s.%s", tokenID, tokenSecret), nil
}

// randomString returns a cryptographically random string over the bootstrap token alphabet.
func randomString(length int) (string, error) {
	out := make([]byte, length)
	if _, err := rand.Read(out); err != nil {
		return "", fmt.Errorf("unable to generate a random bootstrap token: %w", err)
	}
	// len(tokenAlphabet) is 36 and does not divide 256, so the modulo below is very slightly biased.
	// Over 22 characters of a token that lives for minutes this is not a meaningful reduction in
	// entropy, and rejection sampling is not worth the complexity here.
	for i, b := range out {
		out[i] = tokenAlphabet[int(b)%len(tokenAlphabet)]
	}
	return string(out), nil
}

// joinConfiguration renders the kubeadm JoinConfiguration document.
func (b *Bootstrapper) joinConfiguration(nodeClass *v1alpha1.OxideNodeClass, req bootstrap.Request, token string) (string, error) {
	configVersion := nodeClass.Spec.Bootstrap.Kubeadm.ConfigVersion
	if configVersion == "" {
		configVersion = configVersionV1beta4
	}

	nodeRegistration := map[string]any{
		"taints": nodeTaints(req.NodeTaints),
	}
	if req.Hostname != "" {
		nodeRegistration["name"] = req.Hostname
	}
	if kubeletArgs := kubeletExtraArgs(nodeClass.Status.KubeletArgs, configVersion); kubeletArgs != nil {
		nodeRegistration["kubeletExtraArgs"] = kubeletArgs
	}

	config := map[string]any{
		"apiVersion": configVersion,
		"kind":       "JoinConfiguration",
		"discovery": map[string]any{
			"bootstrapToken": map[string]any{
				"token":             token,
				"apiServerEndpoint": nodeClass.Status.Server,
				"caCertHashes":      nodeClass.Status.Bootstrap.Kubeadm.CACertHashes,
			},
		},
		"nodeRegistration": nodeRegistration,
	}

	configYAML, err := yaml.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("unable to marshal the kubeadm JoinConfiguration to YAML: %w", err)
	}
	return string(configYAML), nil
}

// nodeTaints projects taints down to the three fields kubeadm accepts. Marshaling corev1.Taint
// directly would also emit its zero TimeAdded field, which kubeadm rejects as unknown.
func nodeTaints(taints []corev1.Taint) []map[string]string {
	out := make([]map[string]string, 0, len(taints))
	for _, taint := range taints {
		out = append(out, map[string]string{
			"key":    taint.Key,
			"value":  taint.Value,
			"effect": string(taint.Effect),
		})
	}
	return out
}

// kubeletExtraArgs converts "key=value" args into the shape the given kubeadm config version wants.
// v1beta4 takes a list of name/value pairs so that an argument can be repeated; v1beta3 takes a map.
func kubeletExtraArgs(args []string, configVersion string) any {
	if len(args) == 0 {
		return nil
	}
	if configVersion == configVersionV1beta3 {
		out := map[string]string{}
		for _, arg := range args {
			name, value, _ := strings.Cut(arg, "=")
			out[name] = value
		}
		return out
	}
	out := make([]map[string]string, 0, len(args))
	for _, arg := range args {
		name, value, _ := strings.Cut(arg, "=")
		out = append(out, map[string]string{"name": name, "value": value})
	}
	return out
}
