// Package framework provides shared clients, fixtures, and cleanup for e2e tests.
package framework

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/selection"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/oxide.go/oxide"
	"golang.org/x/crypto/ssh"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// LabelRun marks everything a run creates so a sweep can find leftovers from any run.
const LabelRun = "e2e.karpenter.oxide.computer/run"

// LabelPool is put on a workload pod so a wait can find the pod meant for a nodepool.
const LabelPool = "e2e.karpenter.oxide.computer/pool"

// Env is everything a test needs: clients, the resolved defaults, and the run's identity.
type Env struct {
	Run string

	Config    *rest.Config
	Kube      client.Client
	Clientset *kubernetes.Clientset
	Oxide     *oxide.Client

	Project            string
	Namespace          string
	KarpenterNamespace string

	Image       v1alpha1.ImageSelector
	Subnets     []v1alpha1.SubnetSelector
	EphemeralIP bool

	SSHKeyName string
	privateKey ed25519.PrivateKey
	signer     ssh.Signer

	// Keep leaves a failed test's objects behind and writes the private key to disk.
	Keep bool

	ReadyTimeout  time.Duration
	DeleteTimeout time.Duration
	Poll          time.Duration

	Artifacts string

	defaultPool *karpv1.NodePool
}

// Labels is the label set every object of this run carries.
func (e *Env) Labels() map[string]string {
	return map[string]string{LabelRun: e.Run}
}

// Name is the name of the object a test creates: e2e-<test>-<run>.
func (e *Env) Name(test string) string {
	return "e2e-" + strings.ToLower(test) + "-" + e.Run
}

// Load builds the Env from explicit env vars, then discovery, then hard defaults.
func Load(ctx context.Context) (*Env, error) {
	runID := make([]byte, 3)
	if _, err := rand.Read(runID); err != nil {
		return nil, err
	}
	e := &Env{
		Run:           hex.EncodeToString(runID),
		Namespace:     envOr("E2E_NAMESPACE", "default"),
		EphemeralIP:   envOr("E2E_EPHEMERAL_IP", "true") != "false",
		Keep:          os.Getenv("E2E_KEEP") == "true",
		ReadyTimeout:  10 * time.Minute,
		DeleteTimeout: 10 * time.Minute,
		Poll:          5 * time.Second,
		Image: v1alpha1.ImageSelector{
			OS:      envOr("E2E_IMAGE_OS", "k8s-ubuntu"),
			Version: selection.VersionSelector(envOr("E2E_IMAGE_VERSION", "1.37.*-26.04.*")),
		},
	}
	e.SSHKeyName = sshKeyPrefix + e.Run
	if raw := os.Getenv("E2E_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("E2E_TIMEOUT: %w", err)
		}
		e.ReadyTimeout, e.DeleteTimeout = d, d
	}

	artifacts := os.Getenv("ARTIFACTS")
	if artifacts == "" {
		root, err := repoRoot()
		if err != nil {
			return nil, err
		}
		artifacts = filepath.Join(root, "_artifacts")
	}
	e.Artifacts = filepath.Join(artifacts, "e2e", e.Run)

	if err := e.connectKube(); err != nil {
		return nil, err
	}
	if err := e.connectOxide(ctx); err != nil {
		return nil, err
	}
	if err := e.resolveSubnets(ctx); err != nil {
		return nil, err
	}
	if err := e.generateKey(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Env) connectKube() error {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: os.Getenv("E2E_KUBE_CONTEXT")}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).
		ClientConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	config.QPS, config.Burst = 50, 100
	// Both the karpenter.sh and karpenter.oxide.computer types register with client-go's
	// scheme at init.
	kube, err := client.New(config, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		return fmt.Errorf("kube client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	e.Config, e.Kube, e.Clientset = config, kube, clientset
	return nil
}

// connectOxide builds the client the way the CLI would, then checks it points at the
// same rack as the controller, so a test never launches against a different silo.
func (e *Env) connectOxide(ctx context.Context) error {
	ox, err := oxide.NewClient()
	if err != nil {
		return fmt.Errorf("oxide client: %w", err)
	}
	e.Oxide = ox

	controllerHost, controllerProject, err := e.controllerEnv(ctx)
	if err != nil {
		return err
	}
	if !sameHost(ox.Host(), controllerHost) {
		return fmt.Errorf(
			"oxide host %q is not the controller's %q, refusing to run", ox.Host(), controllerHost,
		)
	}
	e.Project = envOr("OXIDE_PROJECT", controllerProject)
	if e.Project == "" {
		return errors.New("no Oxide project: set OXIDE_PROJECT")
	}
	if _, err := ox.ProjectView(ctx, oxide.ProjectViewParams{
		Project: oxide.NameOrId(e.Project),
	}); err != nil {
		return fmt.Errorf("oxide project %q: %w", e.Project, err)
	}
	return nil
}

func sameHost(a, b string) bool {
	trim := func(s string) string { return strings.ToLower(strings.TrimRight(s, "/")) }
	return trim(a) == trim(b)
}

// controllerEnv finds the Karpenter Deployment and reads its OXIDE_HOST and OXIDE_PROJECT.
func (e *Env) controllerEnv(ctx context.Context) (string, string, error) {
	var deployments appsv1.DeploymentList
	if err := e.Kube.List(ctx, &deployments, client.MatchingLabels{
		"app.kubernetes.io/name": "karpenter",
	}); err != nil {
		return "", "", fmt.Errorf("listing karpenter deployments: %w", err)
	}
	for _, d := range deployments.Items {
		for _, c := range d.Spec.Template.Spec.Containers {
			var host, project string
			for _, v := range c.Env {
				switch v.Name {
				case "OXIDE_HOST":
					host = v.Value
				case "OXIDE_PROJECT":
					project = v.Value
				}
			}
			if host != "" {
				e.KarpenterNamespace = d.Namespace
				return host, project, nil
			}
		}
	}
	return "", "", errors.New("no karpenter Deployment with an OXIDE_HOST env found in the cluster")
}

// resolveSubnets copies the cluster's default nodeclass selector, or falls back to the
// project's only VPC and subnet.
func (e *Env) resolveSubnets(ctx context.Context) error {
	if vpc, subnet := os.Getenv("E2E_VPC"), os.Getenv("E2E_SUBNET"); vpc != "" && subnet != "" {
		e.Subnets = []v1alpha1.SubnetSelector{{
			VPCName: selection.WildcardSelector(vpc),
			Name:    selection.WildcardSelector(subnet),
			Project: e.Project,
		}}
		return nil
	}
	var defaultClass v1alpha1.OxideNodeClass
	err := e.Kube.Get(ctx, types.NamespacedName{Name: "default"}, &defaultClass)
	switch {
	case err == nil && len(defaultClass.Spec.SubnetSelector) > 0:
		e.Subnets = defaultClass.Spec.SubnetSelector
		return nil
	case err != nil && !apierrors.IsNotFound(err):
		return fmt.Errorf("reading the default OxideNodeClass: %w", err)
	}

	vpcs, err := e.Oxide.VpcListAllPages(ctx, oxide.VpcListParams{
		Project: oxide.NameOrId(e.Project),
	})
	if err != nil {
		return fmt.Errorf("listing VPCs: %w", err)
	}
	if len(vpcs) != 1 {
		return fmt.Errorf(
			"no default OxideNodeClass and project %s has %d VPCs: set E2E_VPC and E2E_SUBNET",
			e.Project, len(vpcs),
		)
	}
	subnets, err := e.Oxide.VpcSubnetListAllPages(ctx, oxide.VpcSubnetListParams{
		Project: oxide.NameOrId(e.Project),
		Vpc:     oxide.NameOrId(vpcs[0].Id),
	})
	if err != nil {
		return fmt.Errorf("listing subnets of VPC %s: %w", vpcs[0].Name, err)
	}
	if len(subnets) != 1 {
		return fmt.Errorf(
			"no default OxideNodeClass and VPC %s has %d subnets: set E2E_VPC and E2E_SUBNET",
			vpcs[0].Name, len(subnets),
		)
	}
	e.Subnets = []v1alpha1.SubnetSelector{{
		ID: selection.IDSelector(subnets[0].Id), Project: e.Project,
	}}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("not inside a Go module, set ARTIFACTS")
		}
		dir = parent
	}
}
