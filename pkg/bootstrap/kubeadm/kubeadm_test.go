package kubeadm

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/cloudinit"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

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
			Server: "10.0.0.1:6443",
			Images: []v1alpha1.OxideImage{{
				ID:        "11111111-1111-1111-1111-111111111111",
				SizeBytes: 4 << 30,
			}},
			Bootstrap: &v1alpha1.BootstrapStatus{
				Kubeadm: &v1alpha1.KubeadmStatus{
					CACertHashes: []string{"sha256:abcdef"},
					ClusterName:  "test",
				},
			},
		},
	}
}

// clusterInfo is kube-public/cluster-info as kubeadm writes it, around a
// throwaway CA.
func clusterInfo(t *testing.T) *corev1.ConfigMap {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	config := clientcmdapi.NewConfig()
	config.Clusters["test"] = &clientcmdapi.Cluster{
		Server:                   "https://10.0.0.1:6443",
		CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
	kubeconfig, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		Namespace: clusterInfoNamespace, Name: clusterInfoName,
		Data: map[string]string{clusterInfoKubeconfigKey: string(kubeconfig)},
	}
}

func userDataFor(t *testing.T, sets ...v1alpha1.DiskSet) string {
	t.Helper()
	nc := nodeClass(t, sets...)

	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(clusterInfo(t)).
		Build()

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
	out, err := NewBootstrapper(
		kubeClient,
		kubeClient,
		cloudinit.New(kubeClient),
	).UserData(context.Background(), nc, req)
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

func TestUserDataWithoutStorageIsUnchanged(t *testing.T) {
	out := userDataFor(t)
	if !strings.HasPrefix(
		out,
		"#!/usr/bin/env bash\nset -euo pipefail\ninstall -d -m 0755 '/etc/kubernetes'\n",
	) {
		t.Errorf("user-data does not begin with the original preamble:\n%s", out)
	}
	if !strings.HasSuffix(out, "kubeadm join --config /etc/kubernetes/karpenter-join.yaml\n") {
		t.Errorf("user-data does not end with the join:\n%s", out)
	}
	if strings.Contains(out, "#cloud-config") {
		t.Error("user-data contains a cloud-config part for a nodeclass with no diskSets")
	}
}

func TestUserDataMergesCustomUserDataAndEnvironmentBeforeJoin(t *testing.T) {
	nc := nodeClass(t)
	nc.Spec.UserData = "echo custom-$NODE_ROLE\n"
	nc.Spec.Env = []v1alpha1.EnvVar{{Name: "NODE_ROLE", Value: "worker"}}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(clusterInfo(t)).
		Build()

	out, err := NewBootstrapper(kubeClient, kubeClient, cloudinit.New(kubeClient)).UserData(
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
	join := strings.Index(out, "kubeadm join --config")
	if environment < 0 || custom < 0 || join < 0 || environment >= custom || custom >= join {
		t.Errorf(
			"user-data sections are missing or out of order: environment=%d custom=%d join=%d\n%s",
			environment,
			custom,
			join,
			out,
		)
	}
}

func TestUserDataSetsUpDisksBeforeJoining(t *testing.T) {
	out := userDataFor(t, localPVSet(t))

	disks := strings.Index(out, "fs_setup:")
	script := strings.Index(out, "text/x-shellscript")
	check := strings.Index(out, "mountpoint -q")
	config := strings.Index(out, "/etc/kubernetes/karpenter-join.yaml")
	join := strings.Index(out, "kubeadm join --config")

	for name, index := range map[string]int{
		"disk setup": disks, "shell part": script, "mount check": check,
		"config write": config, "join": join,
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

func TestUserDataOrdersAgainstTheKubeletUnit(t *testing.T) {
	out := userDataFor(t, localPVSet(t))
	dropIn := strings.Index(out, "/etc/systemd/system/kubelet.service.d/10-karpenter-disks.conf")
	if dropIn < 0 {
		t.Fatal("the drop-in does not target kubelet.service")
	}
	if join := strings.Index(out, "kubeadm join --config"); dropIn > join {
		t.Error("the kubelet drop-in is written after the join, too late to order anything")
	}
	if strings.Contains(out, "rke2-agent.service") {
		t.Error("the kubeadm flavor orders against the RKE2 agent unit")
	}
}

func TestUserDataWritesTheConfigThroughTheGuardedHeredoc(t *testing.T) {
	out := userDataFor(t, localPVSet(t))
	if !strings.Contains(out, "cat <<'KARPENTER_INSTALL_EOF' > '"+configPath+"'") {
		t.Errorf("the config is not written through the guarded heredoc:\n%s", out)
	}
	if !strings.Contains(out, "JoinConfiguration") {
		t.Error("the join configuration is missing from the rendered user-data")
	}
}

// Token discovery reads cluster-info anonymously, which a hosted control plane refuses.
func TestJoinDiscoversFromAKubeconfigCarryingTheToken(t *testing.T) {
	out := userDataFor(t)
	if strings.Contains(out, "bootstrapToken") || strings.Contains(out, "caCertHashes") {
		t.Errorf("the join uses token discovery:\n%s", out)
	}
	if !strings.Contains(out, "kubeConfigPath: "+discoveryPath) {
		t.Errorf("the join does not discover from %s:\n%s", discoveryPath, out)
	}
	_, rest, ok := strings.Cut(out, "cat <<'KARPENTER_INSTALL_EOF' > '"+discoveryPath+"'\n")
	if !ok {
		t.Fatalf("the discovery kubeconfig is not installed:\n%s", out)
	}
	body, rest, _ := strings.Cut(rest, "KARPENTER_INSTALL_EOF\n")
	if !strings.HasPrefix(rest, "chmod 0600 '"+discoveryPath+"'\n") {
		t.Errorf("the discovery kubeconfig, which holds the token, is not 0600:\n%s", out)
	}
	config, err := clientcmd.Load([]byte(body))
	if err != nil {
		t.Fatalf("the discovery kubeconfig does not parse: %v", err)
	}
	cluster := config.Clusters[config.Contexts[config.CurrentContext].Cluster]
	if cluster.Server != "https://10.0.0.1:6443" || len(cluster.CertificateAuthorityData) == 0 {
		t.Errorf(
			"cluster %q with %d bytes of CA",
			cluster.Server,
			len(cluster.CertificateAuthorityData),
		)
	}
	if user := config.AuthInfos[config.Contexts[config.CurrentContext].AuthInfo]; user.Token == "" {
		t.Error("the discovery kubeconfig carries no bootstrap token")
	}
}

// With an attestor the user-data holds nothing worth stealing: the node is
// issued its credential for proving which instance it is.
func TestUserDataAttestsRatherThanCarryAToken(t *testing.T) {
	nc := nodeClass(t)
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(clusterInfo(t)).
		Build()
	out, err := NewBootstrapper(kubeClient, kubeClient, cloudinit.New(kubeClient)).
		WithAttestor("https://team-dev.join.example:443").
		UserData(context.Background(), nc, bootstrap.Request{
			NodeClaim: &karpv1.NodeClaim{Name: "node-1"},
			Hostname:  "karpenter-default-x7k2p",
		})
	if err != nil {
		t.Fatalf("UserData() returned an unexpected error: %v", err)
	}

	var tokens corev1.SecretList
	if err := kubeClient.List(context.Background(), &tokens); err != nil || len(tokens.Items) != 0 {
		t.Errorf("%d secrets minted (%v), want none", len(tokens.Items), err)
	}
	if strings.Contains(out, "token:") || strings.Contains(out, "'"+discoveryPath+"'") {
		t.Errorf("the user-data carries a join credential:\n%s", out)
	}
	if !strings.Contains(
		out,
		"cat <<'KARPENTER_INSTALL_EOF' > '"+clusterCAPath+"'\n-----BEGIN CERTIFICATE-----",
	) {
		t.Errorf("the cluster CA is not installed:\n%s", out)
	}
	want := "oxide-node-join --attestor 'https://team-dev.join.example:443'" +
		" --cluster-server 'https://10.0.0.1:6443' --cluster-ca-file " + clusterCAPath +
		" --kubeconfig " + discoveryPath + " --node-name 'karpenter-default-x7k2p'\n" +
		"kubeadm join --config " + configPath + "\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("the user-data does not end by attesting and then joining:\n%s", out)
	}
	if !strings.Contains(out, "kubeConfigPath: "+discoveryPath) {
		t.Errorf("the join does not discover from the kubeconfig the node writes:\n%s", out)
	}
}

func TestJoinConfigurationCarriesKubeletReservations(t *testing.T) {
	nc := nodeClass(t)
	nc.Status.KubeletArgs = []string{"cloud-provider=external"}

	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(clusterInfo(t)).
		Build()
	out, err := NewBootstrapper(
		kubeClient,
		kubeClient,
		cloudinit.New(kubeClient),
	).UserData(context.Background(), nc, bootstrap.Request{
		NodeClaim:    &karpv1.NodeClaim{Name: "node-1"},
		Hostname:     "karpenter-default-x7k2p",
		InstanceType: instancetype.Specs{Ncpus: 9, MemInGiB: 5},
	})
	if err != nil {
		t.Fatalf("UserData() returned an unexpected error: %v", err)
	}

	for _, want := range []string{
		"name: kube-reserved",
		"value: cpu=92m,memory=1288490188",
		"name: eviction-hard",
		"value: memory.available<100Mi",
		"name: cloud-provider",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("join configuration is missing %q:\n%s", want, out)
		}
	}
}

func TestJoinConfigurationWithoutInstanceTypeOmitsReservations(t *testing.T) {
	out := userDataFor(t)
	if strings.Contains(out, "kube-reserved") {
		t.Fatalf("unexpected reservations without a shape:\n%s", out)
	}
}

func TestDetect(t *testing.T) {
	for _, test := range []struct {
		name    string
		objects []client.Object
		want    bool
	}{
		{
			name: "a kubeadm cluster keeps its cluster configuration in kube-system",
			objects: []client.Object{&corev1.ConfigMap{
				Namespace: clusterConfigNamespace,
				Name:      clusterConfigName}},
			want: true,
		},
		{name: "and nothing else does"},
	} {
		t.Run(test.name, func(t *testing.T) {
			kubeClient := fake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(test.objects...).
				Build()

			got, err := NewBootstrapper(
				kubeClient,
				kubeClient,
				cloudinit.New(kubeClient),
			).Detect(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != test.want {
				t.Fatalf("got %t, want %t", got, test.want)
			}
		})
	}
}
