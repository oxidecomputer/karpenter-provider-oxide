// Package rke2 bootstraps RKE2 agents.
package rke2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	certutil "k8s.io/client-go/util/cert"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	supervisorDefaultPort = 9345

	configPath = "/etc/rancher/rke2/config.yaml"

	consumerUnit = "rke2-agent.service"

	nodeArgsAnnotation = "rke2.io/node-args"

	// tokenPrefix starts RKE2's secure token format, "K10<ca hash>::<credentials>".
	tokenPrefix = "K10"
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
	reader   client.Reader
	writer   client.Writer
	renderer bootstrap.Renderer

	// caHash pins the cluster CA in every token so agents verify the supervisor before presenting credentials.
	caHash string
}

// NewBootstrapper takes the PEM bundle of the cluster CA, which the in-cluster
// config carries and which RKE2 publishes from the same file the supervisor serves.
func NewBootstrapper(
	reader client.Reader,
	writer client.Writer,
	caBundle []byte,
	renderer bootstrap.Renderer,
) (*Bootstrapper, error) {
	hash, err := hashCA(caBundle)
	if err != nil {
		return nil, fmt.Errorf("unable to hash the cluster CA for RKE2 join tokens: %w", err)
	}
	return &Bootstrapper{reader: reader, writer: writer, renderer: renderer, caHash: hash}, nil
}

func (b *Bootstrapper) Name() string {
	return v1alpha1.BootstrapKindRKE2
}

// Resolve records the RKE2 supervisor endpoint.
func (b *Bootstrapper) Resolve(
	_ context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	cluster bootstrap.Cluster,
) error {
	if cluster.ServerOverride != "" {
		nodeClass.Status.Server = cluster.ServerOverride
		return nil
	}
	nodeClass.Status.Server = "https://" + net.JoinHostPort(
		cluster.Host,
		strconv.Itoa(supervisorDefaultPort),
	)
	return nil
}

// Detect reports an RKE2 cluster: every node RKE2 manages carries its node-args annotation.
func (b *Bootstrapper) Detect(ctx context.Context) (bool, error) {
	nodes := &corev1.NodeList{}
	if err := b.reader.List(ctx, nodes); err != nil {
		return false, fmt.Errorf("unable to list nodes: %w", err)
	}
	for _, node := range nodes.Items {
		if _, ok := node.Annotations[nodeArgsAnnotation]; ok {
			return true, nil
		}
	}
	return false, nil
}

// UserData mints a short-lived bootstrap token for this NodeClaim, writes the agent config.yaml and starts rke2-agent.
func (b *Bootstrapper) UserData(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	req bootstrap.Request,
) (string, error) {
	if nodeClass.Status.Server == "" {
		return "", fmt.Errorf(
			"no supervisor endpoint resolved for OxideNodeClass %q",
			nodeClass.Name,
		)
	}

	token, err := bootstrap.MintToken(ctx, b.writer, nodeClass, req.NodeClaim)
	if err != nil {
		return "", err
	}

	configYAML, err := yaml.Marshal(Config{
		Server: nodeClass.Status.Server,
		KubeletArgs: bootstrap.KubeletArgs(
			req.InstanceType,
			req.Kubelet,
			nodeClass.Status.KubeletArgs,
		),
		NodeTaints: req.NodeTaints,
		Token:      tokenPrefix + b.caHash + "::" + token,
	})
	if err != nil {
		return "", fmt.Errorf("unable to marshal RKE2 config to YAML: %w", err)
	}

	return b.renderer.Render(ctx, nodeClass, bootstrap.Spec{
		Storage:      req.Storage,
		ConsumerUnit: consumerUnit,
		ConfigPath:   configPath,
		Config:       string(configYAML),
		StartCmd:     "systemctl start " + consumerUnit,
	})
}

// hashCA mirrors the agent's check: a lone certificate is hashed as the literal PEM
// bytes, a bundle as the DER of the root the first certificate chains to.
func hashCA(bundle []byte) (string, error) {
	certs, err := certutil.ParseCertsPEM(bundle)
	if err != nil {
		return "", err
	}

	data := bundle
	if len(certs) > 1 {
		roots := x509.NewCertPool()
		intermediates := x509.NewCertPool()
		for i, cert := range certs {
			if i == 0 || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
				continue
			}
			selfIssued := len(cert.AuthorityKeyId) == 0 ||
				bytes.Equal(cert.AuthorityKeyId, cert.SubjectKeyId)
			if selfIssued {
				roots.AddCert(cert)
			} else {
				intermediates.AddCert(cert)
			}
		}
		opts := x509.VerifyOptions{Roots: roots, Intermediates: intermediates}
		if chains, err := certs[0].Verify(opts); err == nil {
			chain := chains[0]
			data = chain[len(chain)-1].Raw
		}
	}

	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
