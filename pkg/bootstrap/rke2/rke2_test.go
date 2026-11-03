package rke2

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/cloudinit"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// selfSignedCA returns a PEM CA bundle holding one self-signed certificate, as RKE2's server-ca.crt does.
func selfSignedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "rke2-server-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func bootstrapperFor(t *testing.T, kubeClient client.Client) *Bootstrapper {
	t.Helper()
	b, err := NewBootstrapper(kubeClient, kubeClient, selfSignedCA(t), cloudinit.New(kubeClient))
	if err != nil {
		t.Fatalf("NewBootstrapper() returned an unexpected error: %v", err)
	}
	return b
}

func kubeClientFor(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
}

func nodeClass(t *testing.T, sets ...v1alpha1.DiskSet) *v1alpha1.OxideNodeClass {
	t.Helper()
	return &v1alpha1.OxideNodeClass{
		Name: "default",
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot:     v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
				DiskSets: sets,
			},
		},
		Status: v1alpha1.OxideNodeClassStatus{
			Server: "https://10.0.0.1:9345",
			Images: []v1alpha1.OxideImage{{
				ID:        "11111111-1111-1111-1111-111111111111",
				SizeBytes: 4 << 30,
			}},
		},
	}
}

func userDataFor(t *testing.T, sets ...v1alpha1.DiskSet) string {
	t.Helper()
	nc := nodeClass(t, sets...)
	req := bootstrap.Request{
		NodeClaim: &karpv1.NodeClaim{Name: "node-1"},
		Hostname:  "karpenter-default-x7k2p",
	}
	if len(sets) > 0 {
		plan, err := storage.Build("karpenter-default-x7k2p", "launch-token", nc)
		if err != nil {
			t.Fatalf("unable to build a plan: %v", err)
		}
		req.Storage = &plan
	}
	out, err := bootstrapperFor(t, kubeClientFor(t)).UserData(context.Background(), nc, req)
	if err != nil {
		t.Fatalf("UserData() returned an unexpected error: %v", err)
	}
	return out
}

func localPVSet(t *testing.T) v1alpha1.DiskSet {
	t.Helper()
	return v1alpha1.DiskSet{
		Name: "local-pv", Type: v1alpha1.DiskSetTypeLocal, Count: 2,
		Size: resource.MustParse("500Gi"), Layout: v1alpha1.DiskSetLayoutIndividual,
		Filesystem: v1alpha1.FilesystemExt4, MountDir: "/mnt/local-pv",
	}
}

func TestUserDataWithoutStorageIsAPlainScript(t *testing.T) {
	out := userDataFor(t)
	if !strings.HasPrefix(
		out,
		"#!/usr/bin/env bash\nset -euo pipefail\ninstall -d -m 0755 '/etc/rancher/rke2'\n",
	) {
		t.Errorf("user-data does not begin with the preamble and the config install:\n%s", out)
	}
	if !strings.HasSuffix(out, "systemctl start rke2-agent.service\n") {
		t.Errorf("user-data does not end by starting the agent:\n%s", out)
	}
	for _, unwanted := range []string{"multipart/mixed", "#cloud-config", "mountpoint -q"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("user-data mentions %q for a nodeclass with no diskSets", unwanted)
		}
	}
}

func TestUserDataMergesCustomUserDataAndEnvironmentBeforeAgentStart(t *testing.T) {
	nc := nodeClass(t)
	nc.Spec.UserData = "echo custom-$NODE_ROLE\n"
	nc.Spec.Env = []v1alpha1.EnvVar{{Name: "NODE_ROLE", Value: "worker"}}

	out, err := bootstrapperFor(t, kubeClientFor(t)).UserData(
		context.Background(),
		nc,
		bootstrap.Request{
			NodeClaim: &karpv1.NodeClaim{Name: "node-1"},
			Hostname:  "karpenter-default-x7k2p",
		},
	)
	if err != nil {
		t.Fatalf("UserData() returned an unexpected error: %v", err)
	}

	environment := strings.Index(out, "export NODE_ROLE='worker'")
	custom := strings.Index(out, "echo custom-$NODE_ROLE")
	start := strings.Index(out, "systemctl start rke2-agent.service")
	if environment < 0 || custom < 0 || start < 0 || environment >= custom || custom >= start {
		t.Errorf(
			"user-data sections are missing or out of order: environment=%d custom=%d start=%d\n%s",
			environment,
			custom,
			start,
			out,
		)
	}
}

func TestUserDataSetsUpDisksBeforeJoining(t *testing.T) {
	out := userDataFor(t, localPVSet(t))

	disks := strings.Index(out, "fs_setup:")
	script := strings.Index(out, "text/x-shellscript")
	check := strings.Index(out, "mountpoint -q")
	config := strings.Index(out, "/etc/rancher/rke2/config.yaml")
	join := strings.Index(out, "systemctl start rke2-agent.service")

	for name, index := range map[string]int{
		"disk setup": disks, "shell part": script, "mount check": check,
		"config write": config, "agent start": join,
	} {
		if index < 0 {
			t.Fatalf("user-data does not contain the %s step:\n%s", name, out)
		}
	}
	if disks >= script || script >= check || check >= config || config >= join {
		t.Errorf(
			"user-data steps are out of order: disks=%d script=%d check=%d config=%d join=%d",
			disks, script, check, config, join,
		)
	}
}

func TestUserDataOrdersAgainstTheAgentUnit(t *testing.T) {
	out := userDataFor(t, localPVSet(t))
	if !strings.Contains(out, "/etc/systemd/system/rke2-agent.service.d/10-karpenter-disks.conf") {
		t.Error("the drop-in does not target rke2-agent.service")
	}
	if strings.Contains(out, "kubelet.service.d") {
		t.Error("the drop-in targets kubelet.service, which RKE2 does not manage as a unit")
	}
}

func TestUserDataWritesTheConfigThroughTheGuardedHeredoc(t *testing.T) {
	out := userDataFor(t, localPVSet(t))
	if !strings.Contains(out, "cat <<'KARPENTER_INSTALL_EOF' > '"+configPath+"'") {
		t.Errorf("the config is not written through the guarded heredoc:\n%s", out)
	}
}

func TestUserDataJoinsWithAPinnedBootstrapToken(t *testing.T) {
	kubeClient := kubeClientFor(t)
	ca := selfSignedCA(t)
	b, err := NewBootstrapper(kubeClient, kubeClient, ca, cloudinit.New(kubeClient))
	if err != nil {
		t.Fatal(err)
	}
	nc := nodeClass(t)
	out, err := b.UserData(context.Background(), nc, bootstrap.Request{
		NodeClaim: &karpv1.NodeClaim{Name: "node-1"},
		Hostname:  "karpenter-default-x7k2p",
	})
	if err != nil {
		t.Fatalf("UserData() returned an unexpected error: %v", err)
	}

	secrets := &corev1.SecretList{}
	if err := kubeClient.List(
		context.Background(), secrets, client.InNamespace("kube-system"),
	); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 1 {
		t.Fatalf("got %d bootstrap token secrets, want 1", len(secrets.Items))
	}
	secret := secrets.Items[0]
	if secret.Type != corev1.SecretTypeBootstrapToken {
		t.Fatalf("secret type %q is not a bootstrap token", secret.Type)
	}
	if secret.Labels[v1alpha1.LabelNodeClass] != nc.Name {
		t.Error("the token secret is not labeled with its nodeclass")
	}
	if secret.StringData["usage-bootstrap-authentication"] != "true" {
		t.Error("the supervisor only accepts tokens with authentication usage")
	}
	if _, ok := secret.StringData["auth-extra-groups"]; ok {
		t.Error("RKE2 agents need no group beyond system:bootstrappers")
	}

	sum := sha256.Sum256(ca)
	want := "token: K10" + hex.EncodeToString(sum[:]) + "::" +
		secret.StringData["token-id"] + "." + secret.StringData["token-secret"]
	if !strings.Contains(out, want) {
		t.Errorf("config.yaml does not carry the pinned token %q:\n%s", want, out)
	}
}

func TestUserDataRequiresAResolvedServer(t *testing.T) {
	nc := nodeClass(t)
	nc.Status.Server = ""
	_, err := bootstrapperFor(t, kubeClientFor(t)).UserData(
		context.Background(),
		nc,
		bootstrap.Request{NodeClaim: &karpv1.NodeClaim{Name: "node-1"}},
	)
	if err == nil || !strings.Contains(err.Error(), "no supervisor endpoint") {
		t.Fatalf("got %v", err)
	}
}

func TestResolveTargetsTheSupervisorPort(t *testing.T) {
	b := bootstrapperFor(t, kubeClientFor(t))
	for _, test := range []struct {
		name    string
		cluster bootstrap.Cluster
		want    string
	}{
		{
			name:    "the API server host joins through the supervisor",
			cluster: bootstrap.Cluster{Host: "10.0.0.1", Port: 6443},
			want:    "https://10.0.0.1:9345",
		},
		{
			name:    "an override is used verbatim",
			cluster: bootstrap.Cluster{ServerOverride: "https://rke2.example:9345"},
			want:    "https://rke2.example:9345",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			nc := nodeClass(t)
			if err := b.Resolve(context.Background(), nc, test.cluster); err != nil {
				t.Fatal(err)
			}
			if nc.Status.Server != test.want {
				t.Fatalf("got %q, want %q", nc.Status.Server, test.want)
			}
		})
	}
}

func TestNewBootstrapperRejectsAnEmptyCA(t *testing.T) {
	kubeClient := kubeClientFor(t)
	if _, err := NewBootstrapper(
		kubeClient,
		kubeClient,
		nil,
		cloudinit.New(kubeClient),
	); err == nil {
		t.Fatal("expected an error without a CA to pin")
	}
}

func TestHashCAOfABundleIsTheRootDER(t *testing.T) {
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
		SubjectKeyId: []byte{1},
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "server-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, rootCert, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}

	bundle := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...,
	)
	got, err := hashCA(bundle)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rootDER)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("got %s, want the root DER hash %s", got, want)
	}
}

func TestDetect(t *testing.T) {
	for _, test := range []struct {
		name    string
		objects []client.Object
		want    bool
	}{
		{
			name: "every node RKE2 manages carries its node-args annotation",
			objects: []client.Object{
				&corev1.Node{Name: "plain"},
				&corev1.Node{
					Name:        "agent",
					Annotations: map[string]string{nodeArgsAnnotation: "agent"}},
			},
			want: true,
		},
		{
			name:    "and a cluster of nodes without it is not RKE2",
			objects: []client.Object{&corev1.Node{Name: "plain"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := bootstrapperFor(t, kubeClientFor(t, test.objects...))
			got, err := b.Detect(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != test.want {
				t.Fatalf("got %t, want %t", got, test.want)
			}
		})
	}
}
