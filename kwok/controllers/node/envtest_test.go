package node

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// This starts only local etcd and kube-apiserver. It never uses kubeconfig or Nexus.
func TestAPIServerRegistration(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run local API-server integration (no Omicron required)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../../pkg/apis/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	k, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	claim, r := fixture()
	status := claim.Status
	claim.UID = ""
	claim.Status = karpv1.NodeClaimStatus{}
	claim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
		{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
	}
	claim.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
	if err := k.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	for i := range status.Conditions {
		status.Conditions[i].LastTransitionTime = metav1.Now()
	}
	claim.Status = status
	if err := k.Status().Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	r.instance.Description = ownership.Marker{
		ClusterUID:    "cluster",
		NodeClaimUID:  claim.UID,
		NodeClaimName: claim.Name,
		Kind:          ownership.KindInstance,
	}.Description(
		"",
	)
	c := reconciler(k, r)
	c.Client = failStatusClient{k}
	if err := step(t, c); err == nil {
		t.Fatal("expected injected status conflict")
	}
	n := getNode(t, k)
	if len(n.Status.Capacity) != 0 {
		t.Fatal("node status unexpectedly survived Create")
	}
	if err := step(t, reconciler(k, r)); err != nil {
		t.Fatal(err)
	}
	n = getNode(t, k)
	if n.Status.Allocatable.StorageEphemeral().Cmp(resource.MustParse("36Gi")) != 0 {
		t.Fatal("status subresource not initialized")
	}
	// Apply the same strategic patch style used by KWOK and verify other conditions/resources survive.
	n.Status.Conditions = []corev1.NodeCondition{
		{
			Type:               "OxideSledHealthy",
			Status:             corev1.ConditionTrue,
			Reason:             "Healthy",
			Message:            "inventory",
			LastTransitionTime: metav1.Now(),
		},
	}
	if err := k.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := k.Status().
		Patch(ctx, n, client.RawPatch(types.StrategicMergePatchType, []byte(`{"status":{"conditions":[{
 "type":"Ready","status":"True","reason":"KWOK","message":"simulated",
 "lastTransitionTime":"2026-09-15T00:00:00Z"}]}}`))); err != nil {
		t.Fatal(err)
	}
	n = getNode(t, k)
	if len(n.Status.Conditions) != 2 || n.Status.Capacity.Cpu().String() != "4" {
		t.Fatal("KWOK patch lost inventory or resources")
	}
	if n.OwnerReferences[0].UID != claim.UID {
		t.Fatal("owner UID lost")
	}
	checkKWOK(t, cfg, k)
}

//nolint:gocyclo // Sequential integration assertions, each with explicit API error checks.
func checkKWOK(t *testing.T, cfg *rest.Config, k client.Client) {
	t.Helper()
	binary := os.Getenv("KWOK_BINARY")
	if binary == "" {
		t.Log("KWOK_BINARY unset: real KWOK stage integration skipped")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	kubeconfig := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"test": {Server: cfg.Host, CertificateAuthorityData: cfg.CAData},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"test": {
				ClientCertificateData: cfg.CertData,
				ClientKeyData:         cfg.KeyData,
				Impersonate:           "system:serviceaccount:default:test-kwok-simulator",
				ImpersonateGroups: []string{
					"system:serviceaccounts",
					"system:serviceaccounts:default",
					"system:authenticated",
				},
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"test": {Cluster: "test", AuthInfo: "test"},
		},
		CurrentContext: "test",
	}
	kubeconfigPath := filepath.Join(dir, "kubeconfig")
	if err := clientcmd.WriteToFile(kubeconfig, kubeconfigPath); err != nil {
		t.Fatal(err)
	}
	content := renderedKWOKConfig(t, ctx, k)
	configPath := filepath.Join(dir, "kwok.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kube-node-lease", "default"} {
		if err := k.Create(
			ctx,
			&corev1.Namespace{Name: name},
		); err != nil &&
			!apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	logFile, err := os.Create(filepath.Join(dir, "kwok.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, binary, "--config="+configPath, "--kubeconfig="+kubeconfigPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
		_ = logFile.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logFile.Name())
			t.Log(string(data))
		}
	}()
	eventually := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatal("KWOK condition did not converge")
	}
	nodeReady := func(status corev1.ConditionStatus) bool {
		n := getNode(t, k)
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				return c.Status == status
			}
		}
		return false
	}
	eventually(func() bool { return nodeReady(corev1.ConditionTrue) })
	// Neither an unmanaged real node nor a node with just one selector label may be managed.
	for _, name := range []string{"real", "one-label"} {
		labels := map[string]string{}
		if name == "one-label" {
			labels["kwok.x-k8s.io/node"] = "fake"
		}
		n := &corev1.Node{Name: name, Labels: labels}
		if err := k.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	pod := &corev1.Pod{
		Name: "simulated", Namespace: "default",
		Spec: corev1.PodSpec{
			NodeName:   "karpenter-example",
			Containers: []corev1.Container{{Name: "pause", Image: "registry.k8s.io/pause:3.10"}},
		},
	}
	if err := k.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	eventually(func() bool {
		if err := k.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			t.Fatal(err)
		}
		return pod.Status.Phase == corev1.PodRunning
	})
	for state, status := range map[string]corev1.ConditionStatus{
		"terminal": corev1.ConditionFalse,
		"unknown":  corev1.ConditionUnknown,
		"running":  corev1.ConditionTrue,
	} {
		n := getNode(t, k)
		before := n.DeepCopy()
		n.Labels[BackendState] = state
		if err := k.Patch(ctx, n, client.MergeFrom(before)); err != nil {
			t.Fatal(err)
		}
		eventually(func() bool { return nodeReady(status) })
	}
	n := getNode(t, k)
	if len(n.Status.Conditions) != 2 || n.Status.Capacity.Cpu().String() != "4" {
		t.Fatal("stages lost inventory or capacity")
	}
	for _, name := range []string{"real", "one-label"} {
		n := &corev1.Node{}
		if err := k.Get(ctx, client.ObjectKey{Name: name}, n); err != nil {
			t.Fatal(err)
		}
		if len(n.Status.Conditions) != 0 {
			t.Fatal("KWOK touched unmanaged node")
		}
	}
	// A normal deletion may complete, but KWOK must never remove arbitrary finalizers.
	pod.Finalizers = []string{"example.com/keep"}
	if err := k.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if err := k.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Finalizers) != 1 {
		t.Fatal("KWOK removed a user finalizer")
	}
	pod.Finalizers = nil
	if err := k.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	eventually(
		func() bool { return apierrors.IsNotFound(k.Get(ctx, client.ObjectKeyFromObject(pod), pod)) },
	)
}

// Validate every rendered Kubernetes object against the same local API server used by the bridge tests.
func renderedKWOKConfig(t *testing.T, ctx context.Context, k client.Client) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("Helm is required when KWOK_BINARY is set")
	}
	rendered, err := exec.Command(helm,
		"template", "test", "../../../charts/karpenter-provider-oxide-kwok",
		"--namespace", "default",
		"--set", "provider.settings.oxide.host=http://nexus.invalid",
		"--set", "provider.settings.oxide.project=test",
	).
		CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v: %s", err, rendered)
	}
	for _, name := range []string{"kube-system", "kube-public", "kube-node-lease"} {
		if err := k.Create(
			ctx,
			&corev1.Namespace{Name: name},
		); err != nil &&
			!apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	config := ""
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if obj.GetKind() == "" || obj.GetKind() == "CustomResourceDefinition" {
			continue
		}
		mapping, err := k.RESTMapper().
			RESTMapping(obj.GroupVersionKind().GroupKind(), obj.GroupVersionKind().Version)
		if err != nil {
			t.Fatal(err)
		}
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace && obj.GetNamespace() == "" {
			obj.SetNamespace("default")
		}
		if err := k.Create(ctx, obj); err != nil {
			t.Fatalf("rendered %s/%s invalid: %v", obj.GetKind(), obj.GetName(), err)
		}
		if obj.GetKind() == "ConfigMap" && obj.GetName() == "test-kwok" {
			config, _, err = unstructured.NestedString(obj.Object, "data", "kwok.yaml")
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if config == "" {
		t.Fatal("missing rendered KWOK configuration")
	}
	return config
}
