package rke2

import (
	"context"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/disk"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func tokenSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rancher-agent-token", Namespace: "karpenter"},
		Data:       map[string][]byte{"token": []byte("secret-join-token")},
	}
}

func readerFor(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
}

func nodeClass(t *testing.T, sets ...v1alpha1.DiskSet) *v1alpha1.OxideNodeClass {
	t.Helper()
	return &v1alpha1.OxideNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.OxideNodeClassSpec{
			Bootstrap: &v1alpha1.Bootstrap{
				RKE2: &v1alpha1.RKE2Bootstrap{
					TokenRefSecret: v1alpha1.SecretRef{
						Name: "rancher-agent-token", Namespace: "karpenter", Key: "token",
					},
				},
			},
			Storage: v1alpha1.Storage{
				Boot:     v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
				DiskSets: sets,
			},
		},
		Status: v1alpha1.OxideNodeClassStatus{Server: "https://10.0.0.1:9345"},
	}
}

func userDataFor(t *testing.T, sets ...v1alpha1.DiskSet) string {
	t.Helper()
	nc := nodeClass(t, sets...)
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
	out, err := NewBootstrapper(readerFor(t, tokenSecret(t))).
		UserData(context.Background(), nc, req)
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
		"#!/usr/bin/env bash\nset -euo pipefail\nmkdir -p /etc/rancher/rke2\n",
	) {
		t.Errorf("user-data does not begin with the original preamble:\n%s", out)
	}
	if !strings.HasSuffix(out, "systemctl start rke2-agent.service\n") {
		t.Errorf("user-data does not end by starting the agent:\n%s", out)
	}
	for _, unwanted := range []string{"karpenter disk setup", "karpenter-disks.service"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("user-data mentions %q for a nodeclass with no diskSets", unwanted)
		}
	}
}

func TestUserDataSetsUpDisksBeforeJoining(t *testing.T) {
	out := userDataFor(t, localPVSet(t))

	setup := strings.Index(out, "### BEGIN karpenter disk setup")
	runUnit := strings.Index(out, "'"+disk.ScriptPath+"' install")
	config := strings.Index(out, "/etc/rancher/rke2/config.yaml")
	join := strings.Index(out, "systemctl start rke2-agent.service")

	for name, index := range map[string]int{
		"disk setup": setup, "disk install": runUnit, "config write": config, "agent start": join,
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

func TestUserDataOrdersAgainstTheAgentUnit(t *testing.T) {
	out := userDataFor(t, localPVSet(t))
	if !strings.Contains(out, "/etc/systemd/system/rke2-agent.service.d/10-karpenter-disks.conf") {
		t.Error("the drop-in does not target rke2-agent.service")
	}
	if strings.Contains(out, "kubelet.service.d") {
		t.Error("the drop-in targets kubelet.service, which RKE2 does not manage as a unit")
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

	if !strings.Contains(out, "secret-join-token") {
		t.Error("the join token is missing from the rendered user-data")
	}
}
