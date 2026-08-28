// Package kubeadm bootstraps kubeadm nodes.
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

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/disk"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	clusterInfoNamespace = "kube-public"
	clusterInfoName      = "cluster-info"

	clusterInfoKubeconfigKey = "kubeconfig"

	tokenNamespace = "kube-system"

	tokenSecretPrefix = "bootstrap-token-"

	tokenSecretType = corev1.SecretTypeBootstrapToken

	nodeBootstrapGroup = "system:bootstrappers:kubeadm:default-node-token"

	defaultTokenTTL = 15 * time.Minute

	defaultAPIServerPort = 6443

	tokenIDLength     = 6
	tokenSecretLength = 16

	tokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

	configVersionV1beta3 = "kubeadm.k8s.io/v1beta3"
	configVersionV1beta4 = "kubeadm.k8s.io/v1beta4"

	userDataTemplateText = `#!/usr/bin/env bash
set -euo pipefail
{{ with .DiskSetup }}{{ . }}
{{ end }}mkdir -p /etc/kubernetes
cat <<'EOF' > /etc/kubernetes/karpenter-join.yaml
{{ .Config }}
EOF
kubeadm join --config /etc/kubernetes/karpenter-join.yaml
`

	consumerUnit = "kubelet.service"
)

var userDataTemplate = template.Must(template.New("userdata").Parse(userDataTemplateText))

// Bootstrapper implements bootstrap.Bootstrapper for kubeadm.
type Bootstrapper struct {
	reader client.Reader

	writer client.Writer
}

func NewBootstrapper(reader client.Reader, writer client.Writer) *Bootstrapper {
	return &Bootstrapper{reader: reader, writer: writer}
}

func (b *Bootstrapper) Name() string {
	return v1alpha1.BootstrapKindKubeadm
}

// Resolve reads cluster-info and records the endpoint and CA pins nodes will use to discover the control plane.
func (b *Bootstrapper) Resolve(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	cluster bootstrap.Cluster,
) error {
	clusterInfo, err := b.clusterInfo(ctx)
	if err != nil {

		nodeClass.StatusConditions().
			SetFalse(v1alpha1.ConditionTypeClusterInfoAvailable, "ClusterInfoUnavailable", err.Error())
		return nil
	}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeClusterInfoAvailable)

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

// UserData mints a short-lived bootstrap token for this NodeClaim and renders a JoinConfiguration that uses it.
func (b *Bootstrapper) UserData(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	req bootstrap.Request,
) (string, error) {
	if nodeClass.Status.Bootstrap == nil || nodeClass.Status.Bootstrap.Kubeadm == nil {
		return "", fmt.Errorf(
			"kubeadm discovery information has not been resolved for OxideNodeClass %q",
			nodeClass.Name,
		)
	}
	if nodeClass.Status.Server == "" {
		return "", fmt.Errorf(
			"no API server endpoint resolved for OxideNodeClass %q",
			nodeClass.Name,
		)
	}

	token, err := b.createToken(ctx, nodeClass, req)
	if err != nil {
		return "", err
	}

	config, err := b.joinConfiguration(nodeClass, req, token)
	if err != nil {
		return "", err
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
		Config:    config,
	}); err != nil {
		return "", fmt.Errorf("unable to execute user-data template: %w", err)
	}
	return bootstrap.MergeUserData(ctx, b.reader, nodeClass, rendered.String())
}

type clusterInfoData struct {
	endpoint     string
	clusterName  string
	caCertHashes []string
}

func (b *Bootstrapper) clusterInfo(ctx context.Context) (*clusterInfoData, error) {
	configMap := &corev1.ConfigMap{}
	objKey := client.ObjectKey{Namespace: clusterInfoNamespace, Name: clusterInfoName}
	if err := b.reader.Get(ctx, objKey, configMap); err != nil {
		return nil, fmt.Errorf(
			"unable to retrieve the %s/%s ConfigMap: %w",
			clusterInfoNamespace,
			clusterInfoName,
			err,
		)
	}

	kubeconfigData, ok := configMap.Data[clusterInfoKubeconfigKey]
	if !ok {
		return nil, fmt.Errorf(
			"%s/%s ConfigMap has no %q key",
			clusterInfoNamespace,
			clusterInfoName,
			clusterInfoKubeconfigKey,
		)
	}

	kubeconfig, err := clientcmd.Load([]byte(kubeconfigData))
	if err != nil {
		return nil, fmt.Errorf("unable to parse the cluster-info kubeconfig: %w", err)
	}

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

func (b *Bootstrapper) createToken(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	req bootstrap.Request,
) (string, error) {
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
			"description": fmt.Sprintf(
				"Karpenter bootstrap token for NodeClaim %s",
				req.NodeClaim.Name,
			),
			"token-id":                       tokenID,
			"token-secret":                   tokenSecret,
			"expiration":                     time.Now().Add(ttl).UTC().Format(time.RFC3339),
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
			"auth-extra-groups":              nodeBootstrapGroup,
		},
	}
	if err := b.writer.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("unable to create the bootstrap token secret: %w", err)
	}
	return fmt.Sprintf("%s.%s", tokenID, tokenSecret), nil
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

func (b *Bootstrapper) joinConfiguration(
	nodeClass *v1alpha1.OxideNodeClass,
	req bootstrap.Request,
	token string,
) (string, error) {
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
	if kubeletArgs := kubeletExtraArgs(
		bootstrap.KubeletArgs(req.InstanceType, nodeClass.Status.KubeletArgs),
		configVersion,
	); kubeletArgs != nil {
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

type userData struct {
	DiskSetup string
	Config    string
}
