package framework

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestPoolPodsReadyRequiresTheExpectedWorkload(t *testing.T) {
	ready := &corev1.Pod{
		Name: "workload", Namespace: "test", Labels: map[string]string{LabelPool: "pool"},
		Spec: corev1.PodSpec{NodeName: "replacement"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	terminating := ready.DeepCopy()
	terminating.Name = "old-workload"
	terminating.Spec.NodeName = "old-node"
	terminating.DeletionTimestamp = new(metav1.Now())
	terminating.Finalizers = []string{"test"}

	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod) []client.Object
		want   string
	}{
		{name: "missing", mutate: func(*corev1.Pod) []client.Object { return nil }, want: "0 non-terminating pods"},
		{name: "terminating only", mutate: func(*corev1.Pod) []client.Object {
			return []client.Object{terminating}
		}, want: "0 non-terminating pods"},
		{name: "running but unready", mutate: func(p *corev1.Pod) []client.Object {
			p.Status.Conditions[0].Status = corev1.ConditionFalse
			return []client.Object{p}
		}, want: "not ready"},
		{name: "wrong node", mutate: func(p *corev1.Pod) []client.Object {
			p.Spec.NodeName = "old-node"
			return []client.Object{p}
		}, want: "not on an expected node"},
		{name: "wrong pool", mutate: func(p *corev1.Pod) []client.Object {
			p.Labels[LabelPool] = "other"
			return []client.Object{p}
		}, want: "0 non-terminating pods"},
		{name: "wrong namespace", mutate: func(p *corev1.Pod) []client.Object {
			p.Namespace = "other"
			return []client.Object{p}
		}, want: "0 non-terminating pods"},
		{name: "extra workload", mutate: func(p *corev1.Pod) []client.Object {
			extra := p.DeepCopy()
			extra.Name = "extra"
			return []client.Object{p, extra}
		}, want: "2 non-terminating pods"},
		{name: "replacement with terminating predecessor", mutate: func(p *corev1.Pod) []client.Object {
			return []client.Object{p, terminating}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Env{
				Namespace: "test",
				Kube: fake.NewClientBuilder().WithScheme(scheme.Scheme).
					WithObjects(tc.mutate(ready.DeepCopy())...).Build(),
			}
			err := e.poolPodsReady(t.Context(), &karpv1.NodePool{Name: "pool"}, 1,
				map[string]bool{"replacement": true})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("readiness error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLaunchCleanupChecksCapturedDisksAfterTheInstanceDisappears(t *testing.T) {
	const diskID = "11111111-0000-4000-8000-000000000001"
	status := http.StatusOK
	var diskReads int
	e := &Env{Oxide: cleanupOxide(t, func(r *http.Request) (*http.Response, error) {
		code := http.StatusNotFound
		switch r.URL.Path {
		case "/v1/instances/instance":
		case "/v1/disks/" + diskID:
			diskReads++
			code = status
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
		return cleanupResponse(r, code), nil
	})}
	launch := Launch{NodeClaim: "claim", InstanceID: "instance", DiskIDs: []string{diskID}}
	if err := e.launchGone(
		t.Context(),
		launch,
	); err == nil ||
		!strings.Contains(err.Error(), diskID) {
		t.Fatalf("cleanup error = %v, want the surviving disk", err)
	}
	status = http.StatusForbidden
	if err := e.launchGone(t.Context(), launch); err == nil {
		t.Fatal("an unreadable disk passed cleanup")
	}
	status = http.StatusNotFound
	if err := e.launchGone(t.Context(), launch); err != nil {
		t.Fatal(err)
	}
	if diskReads != 3 {
		t.Fatalf("read the captured disk %d times, want 3", diskReads)
	}
}

func TestCleanupSharesItsDeadlineAcrossKubernetesAndOxide(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	var kubeDeadline time.Time
	e := &Env{
		DeleteTimeout: time.Minute, Poll: time.Millisecond,
		Kube: fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
					kubeDeadline, _ = ctx.Deadline()
					return nil
				},
			}).Build(),
	}
	e.Oxide = cleanupOxide(t, func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || kubeDeadline.IsZero() || !deadline.Equal(kubeDeadline) {
			t.Errorf(
				"Oxide deadline %v does not match Kubernetes deadline %v",
				deadline,
				kubeDeadline,
			)
		}
		if time.Until(deadline) > e.DeleteTimeout {
			t.Errorf("cleanup did not apply its own timeout: %v", deadline)
		}
		cancel()
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		t.Error("Oxide request ignored cancellation")
		return nil, context.Canceled
	})
	err := e.waitForCleanup(ctx, &karpv1.NodePool{Name: "pool"}, []Launch{{InstanceID: "instance"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup error = %v, want cancellation", err)
	}
	if kubeDeadline.IsZero() {
		t.Fatal("cleanup never checked Kubernetes")
	}
}

type cleanupTransport func(*http.Request) (*http.Response, error)

func (f cleanupTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func cleanupOxide(t *testing.T, transport cleanupTransport) *oxide.Client {
	t.Helper()
	c, err := oxide.NewClient(oxide.WithHost("http://oxide.invalid"), oxide.WithToken("test"),
		oxide.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func cleanupResponse(r *http.Request, code int) *http.Response {
	body := `{"error_code":"ObjectNotFound"}`
	if code == http.StatusForbidden {
		body = `{"error_code":"Forbidden"}`
	}
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}
