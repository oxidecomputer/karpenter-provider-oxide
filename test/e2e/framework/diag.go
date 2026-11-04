package framework

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/test/e2e/framework/host"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/yaml"
)

// diagnose writes what a failed test's objects looked like to the run's artifact
// directory: the nodeclass, nodepool, nodeclaims and nodes as YAML, the controller's
// log lines about the nodeclaim, and cloud-init's account of the boot.
func (e *Env) diagnose(
	ctx context.Context, t *testing.T, nc *v1alpha1.OxideNodeClass, pool *karpv1.NodePool,
) {
	t.Helper()
	dir := filepath.Join(e.Artifacts, t.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("diagnostics: %v", err)
		return
	}
	t.Logf("diagnostics in %s", dir)
	write := func(name string, content []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Logf("diagnostics: writing %s: %v", name, err)
		}
	}
	dump := func(name string, obj client.Object) {
		if err := e.Kube.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			write(name, []byte(err.Error()+"\n"))
			return
		}
		obj.SetManagedFields(nil)
		out, err := yaml.Marshal(obj)
		if err != nil {
			out = []byte(err.Error() + "\n")
		}
		write(name, out)
	}
	if nc != nil {
		dump("oxidenodeclass.yaml", &v1alpha1.OxideNodeClass{
			ObjectMeta: nc.ObjectMeta,
		})
	}
	if pool == nil {
		return
	}
	dump("nodepool.yaml", &karpv1.NodePool{ObjectMeta: pool.ObjectMeta})
	var pods corev1.PodList
	if err := e.Kube.List(ctx, &pods, client.InNamespace(e.Namespace), client.MatchingLabels{
		LabelPool: pool.Name,
	}); err == nil {
		for i := range pods.Items {
			dump("pod-"+pods.Items[i].Name+".yaml", &pods.Items[i])
		}
	}

	var claims karpv1.NodeClaimList
	if err := e.Kube.List(ctx, &claims, client.MatchingLabels{
		karpv1.NodePoolLabelKey: pool.Name,
	}); err != nil {
		write("nodeclaims.txt", []byte(err.Error()+"\n"))
		return
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		dump("nodeclaim-"+claim.Name+".yaml", claim)
		write("karpenter-"+claim.Name+".log", e.controllerLog(ctx, claim.Name))

		if claim.Status.NodeName != "" {
			node := &corev1.Node{}
			if err := e.Kube.Get(
				ctx,
				types.NamespacedName{Name: claim.Status.NodeName},
				node,
			); err == nil {
				dump("node-"+node.Name+".yaml", node)
				e.cloudInitViaProbe(ctx, t, node, claim, write)
				continue
			}
		}
		e.cloudInitViaSSH(ctx, t, claim, write)
	}
}

func (e *Env) cloudInitViaProbe(
	ctx context.Context, t *testing.T, node *corev1.Node, claim *karpv1.NodeClaim,
	write func(string, []byte),
) {
	t.Helper()
	h := host.Node(t, e.hostEnv(t), node,
		instanceID(claim.Status.ProviderID), claim.Annotations[v1alpha1.AnnotationDiskPrefix])
	if log, err := h.File(ctx, "/var/log/cloud-init-output.log"); err == nil {
		write("cloud-init-output.log", []byte(log))
	} else {
		write("cloud-init-output.log", []byte(err.Error()+"\n"))
	}
	if st, err := h.CloudInit(ctx); err == nil {
		out, _ := yaml.Marshal(st)
		write("cloud-init-status.yaml", out)
	} else {
		write("cloud-init-status.yaml", []byte(err.Error()+"\n"))
	}
}

// cloudInitViaSSH is the last resort for a node that never registered: reach its
// ephemeral address with the run's key and read what cloud-init left.
func (e *Env) cloudInitViaSSH(
	ctx context.Context, t *testing.T, claim *karpv1.NodeClaim, write func(string, []byte),
) {
	t.Helper()
	var address string
	for _, rec := range v1alpha1.ParseEphemeralIPs(claim.Annotations[v1alpha1.AnnotationEphemeralIPs]) {
		if rec.IPVersion == "v4" {
			address = rec.Address
		}
	}
	if address == "" {
		write("cloud-init-output.log", []byte("node never registered and has no ephemeral IP\n"))
		return
	}
	config := &ssh.ClientConfig{
		User:            envOr("E2E_SSH_USER", "ubuntu"),
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(e.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // diagnostics only
		Timeout:         15 * time.Second,
	}
	dialer := net.Dialer{Timeout: config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, "22"))
	if err != nil {
		write("cloud-init-output.log", []byte(fmt.Sprintf("ssh %s: %v\n", address, err)))
		return
	}
	defer func() { _ = conn.Close() }()
	c, chans, reqs, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		write("cloud-init-output.log", []byte(fmt.Sprintf("ssh %s: %v\n", address, err)))
		return
	}
	cl := ssh.NewClient(c, chans, reqs)
	defer func() { _ = cl.Close() }()
	run := func(name, command string) {
		session, err := cl.NewSession()
		if err != nil {
			write(name, []byte(err.Error()+"\n"))
			return
		}
		defer func() { _ = session.Close() }()
		out, err := session.CombinedOutput(command)
		if err != nil {
			out = append(out, []byte("\n"+err.Error()+"\n")...)
		}
		write(name, out)
	}
	run("cloud-init-output.log", "sudo cat /var/log/cloud-init-output.log")
	run("cloud-init-status.txt", "cloud-init status --long")
}

// controllerLog is every controller log line mentioning the nodeclaim.
func (e *Env) controllerLog(ctx context.Context, nodeClaim string) []byte {
	var pods corev1.PodList
	if err := e.Kube.List(ctx, &pods, client.InNamespace(e.KarpenterNamespace),
		client.MatchingLabels{"app.kubernetes.io/name": "karpenter"}); err != nil {
		return []byte(err.Error() + "\n")
	}
	var out strings.Builder
	for _, pod := range pods.Items {
		stream, err := e.Clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name,
			&corev1.PodLogOptions{Container: "controller"}).Stream(ctx)
		if err != nil {
			fmt.Fprintf(&out, "%s: %v\n", pod.Name, err)
			continue
		}
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), nodeClaim) {
				out.WriteString(scanner.Text())
				out.WriteByte('\n')
			}
		}
		_ = stream.Close()
	}
	return []byte(out.String())
}
