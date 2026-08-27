package kubeadm

import (
	"context"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/disk"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func nodeClass(t *testing.T, sets ...v1alpha1.DiskSet) *v1alpha1.OxideNodeClass {
	t.Helper()
	return &v1alpha1.OxideNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.OxideNodeClassSpec{
			Bootstrap: &v1alpha1.Bootstrap{Kubeadm: &v1alpha1.KubeadmBootstrap{}},
			Storage: v1alpha1.Storage{
				Boot:     v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
				DiskSets: sets,
			},
		},
		Status: v1alpha1.OxideNodeClassStatus{
			Server: "10.0.0.1:6443",
			Bootstrap: &v1alpha1.BootstrapStatus{
				Kubeadm: &v1alpha1.KubeadmStatus{
					CACertHashes: []string{"sha256:abcdef"},
					ClusterName:  "test",
				},
			},
		},
	}
}

func userDataFor(t *testing.T, sets ...v1alpha1.DiskSet) string {
	t.Helper()
	nc := nodeClass(t, sets...)

	kubeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	req := bootstrap.Request{
		NodeClaim: &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		Hostname:  "karpenter-default-x7k2p",
	}
	if len(sets) > 0 {
		plan, err := storage.Build("karpenter-default-x7k2p", "launch-token", nc)
		if err != nil {
			t.Fatalf("unable to build a plan: %v", err)
		}
		req.Storage = &plan
	}
	out, err := NewBootstrapper(kubeClient, kubeClient).UserData(context.Background(), nc, req)
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
		"#!/usr/bin/env bash\nset -euo pipefail\nmkdir -p /etc/kubernetes\n",
	) {
		t.Errorf("user-data does not begin with the original preamble:\n%s", out)
	}
	if !strings.HasSuffix(out, "kubeadm join --config /etc/kubernetes/karpenter-join.yaml\n") {
		t.Errorf("user-data does not end with the join:\n%s", out)
	}
	if strings.Contains(out, "karpenter disk setup") {
		t.Error("user-data contains a disk setup block for a nodeclass with no diskSets")
	}
}

func TestUserDataMergesCustomUserDataAndEnvironmentBeforeJoin(t *testing.T) {
	nc := nodeClass(t)
	nc.Spec.UserData = "echo custom-$NODE_ROLE\n"
	nc.Spec.Env = []v1alpha1.EnvVar{{Name: "NODE_ROLE", Value: "worker"}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	out, err := NewBootstrapper(kubeClient, kubeClient).UserData(
		context.Background(),
		nc,
		bootstrap.Request{
			NodeClaim: &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
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

	setup := strings.Index(out, "### BEGIN karpenter disk setup")
	runUnit := strings.Index(out, "'"+disk.ScriptPath+"' install")
	config := strings.Index(out, "/etc/kubernetes/karpenter-join.yaml")
	join := strings.Index(out, "kubeadm join --config")

	for name, index := range map[string]int{
		"disk setup": setup, "disk install": runUnit, "config write": config, "join": join,
	} {
		if index < 0 {
			t.Fatalf("user-data does not contain the %s step:\n%s", name, out)
		}
	}
	if setup >= runUnit || runUnit >= config || config >= join {
		t.Errorf(
			"user-data steps are out of order: setup=%d unit=%d config=%d join=%d",
			setup, runUnit, config, join,
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

func TestUserDataHeredocsAreNotShadowed(t *testing.T) {
	out := userDataFor(t, localPVSet(t))
	if got := strings.Count(out, "\nEOF\n"); got != 1 {
		t.Errorf(
			"a bare EOF line appears %d times, want exactly 1, closing the config heredoc",
			got,
		)
	}
	if !strings.Contains(out, "JoinConfiguration") {
		t.Error("the join configuration is missing from the rendered user-data")
	}
}
