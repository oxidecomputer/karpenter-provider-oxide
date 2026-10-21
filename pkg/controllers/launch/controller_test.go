package launch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/oxide.go/oxide"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const instanceID = "11111111-0000-4000-8000-000000000001"

type instanceAPI struct {
	runState oxide.InstanceState
	views    int
}

func (f *instanceAPI) client(t *testing.T) *oxide.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.views++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(&oxide.Instance{
			Id:       instanceID,
			Name:     "karpenter-node-1",
			RunState: f.runState,
		}); err != nil {
			t.Errorf("unable to encode the fake response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	oxideClient, err := oxide.NewClient(
		oxide.WithHost(srv.URL),
		oxide.WithToken("token"),
		projecttest.WithGuard(t),
	)
	if err != nil {
		t.Fatalf("unable to build a client against the fake API: %v", err)
	}
	return oxideClient
}

func launchedClaim(launchedAgo time.Duration) *karpv1.NodeClaim {
	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status:     karpv1.NodeClaimStatus{ProviderID: "oxide://" + instanceID},
	}
	claim.StatusConditions().SetTrue(karpv1.ConditionTypeLaunched)
	for i := range claim.Status.Conditions {
		if claim.Status.Conditions[i].Type == karpv1.ConditionTypeLaunched {
			claim.Status.Conditions[i].LastTransitionTime = metav1.NewTime(
				time.Now().Add(-launchedAgo),
			)
		}
	}
	return claim
}

func runReconcile(
	t *testing.T,
	api *instanceAPI,
	claim *karpv1.NodeClaim,
) (client.Client, time.Duration) {
	t.Helper()

	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(claim).
		Build()
	c := NewController(kubeClient, api.client(t))
	result, err := c.Reconcile(context.Background(), claim)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return kubeClient, result.RequeueAfter
}

func deleted(t *testing.T, kubeClient client.Client) bool {
	t.Helper()
	err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: "node-1"},
		&karpv1.NodeClaim{},
	)
	if err != nil && !kerrors.IsNotFound(err) {
		t.Fatalf("unable to re-read the NodeClaim: %v", err)
	}
	return kerrors.IsNotFound(err)
}

func TestReconcileDeletesImmediatelyOnATerminalRunState(t *testing.T) {
	for _, state := range []oxide.InstanceState{
		oxide.InstanceStateFailed,
		oxide.InstanceStateDestroyed,
	} {
		t.Run(string(state), func(t *testing.T) {
			kubeClient, _ := runReconcile(
				t,
				&instanceAPI{runState: state},
				launchedClaim(10*time.Second),
			)
			if !deleted(t, kubeClient) {
				t.Errorf("the NodeClaim survived a %q instance, want it deleted", state)
			}
		})
	}
}

func TestReconcileLeavesARunningInstanceToCore(t *testing.T) {
	kubeClient, requeue := runReconcile(
		t,
		&instanceAPI{runState: oxide.InstanceStateRunning},

		launchedClaim(time.Hour),
	)
	if deleted(t, kubeClient) {
		t.Error("the NodeClaim was deleted for a running instance, want it left to core")
	}
	if requeue == 0 {
		t.Error("requeue = 0, want the NodeClaim looked at again")
	}
}

func TestReconcileWaitsOutAnInstanceTheAPIWillNotDelete(t *testing.T) {
	kubeClient, requeue := runReconcile(
		t,
		&instanceAPI{runState: oxide.InstanceStateCreating},

		launchedClaim(time.Hour),
	)
	if deleted(t, kubeClient) {
		t.Error("the NodeClaim was deleted for an instance stuck in creating, want it left alone")
	}
	if requeue == 0 {
		t.Error("requeue = 0, want the NodeClaim looked at again once the rack settles it")
	}
}

func TestReconcileLeavesAStartingInstanceToCore(t *testing.T) {
	kubeClient, requeue := runReconcile(
		t,
		&instanceAPI{runState: oxide.InstanceStateStarting},
		launchedClaim(time.Hour),
	)
	if deleted(t, kubeClient) {
		t.Error("the NodeClaim was deleted for a starting instance, want it left to core")
	}
	if requeue != resyncPeriod {
		t.Errorf("requeue = %s, want %s", requeue, resyncPeriod)
	}
}

func TestReconcileIgnoresRegisteredAndTerminatingNodeClaims(t *testing.T) {
	registered := launchedClaim(time.Hour)
	registered.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)

	terminating := launchedClaim(time.Hour)
	terminating.Finalizers = []string{"karpenter.sh/termination"}
	now := metav1.Now()
	terminating.DeletionTimestamp = &now

	for name, claim := range map[string]*karpv1.NodeClaim{
		"registered":  registered,
		"terminating": terminating,
	} {
		t.Run(name, func(t *testing.T) {
			api := &instanceAPI{runState: oxide.InstanceStateFailed}
			kubeClient, _ := runReconcile(t, api, claim)
			if deleted(t, kubeClient) {
				t.Errorf("the %s NodeClaim was deleted, want it left alone", name)
			}
			if api.views != 0 {
				t.Errorf("instance views = %d, want the API not consulted at all", api.views)
			}
		})
	}
}

func TestReconcileIgnoresANodeClaimThatHasNotLaunched(t *testing.T) {
	claim := launchedClaim(time.Hour)
	claim.Status.ProviderID = ""

	api := &instanceAPI{runState: oxide.InstanceStateFailed}
	kubeClient, _ := runReconcile(t, api, claim)
	if deleted(t, kubeClient) {
		t.Error("an unlaunched NodeClaim was deleted, want it left alone")
	}
	if api.views != 0 {
		t.Errorf("instance views = %d, want the API not consulted at all", api.views)
	}
}
