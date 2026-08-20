package affinity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/oxide.go/oxide"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	instanceID = "11111111-0000-4000-8000-000000000001"
	groupID    = "22222222-0000-4000-8000-000000000002"
)

// memberAPI is enough of the Oxide API to serve a membership removal. gone makes the group answer
// 404, which is what the group already having been deleted looks like.
type memberAPI struct {
	gone bool
	// deletes are the "<group>/<instance>" pairs the controller asked to remove.
	deletes []string
}

func (f *memberAPI) server(t *testing.T) *oxide.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		if r.Method == http.MethodDelete && strings.Contains(path, "/members/instance/") {
			parts := strings.Split(path, "/")
			f.deletes = append(f.deletes, parts[1]+"/"+parts[len(parts)-1])
			if f.gone {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).
					Encode(map[string]string{"error_code": "ObjectNotFound", "message": "not found"})
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	oxideClient, err := oxide.NewClient(oxide.WithHost(srv.URL), oxide.WithToken("token"))
	if err != nil {
		t.Fatalf("unable to build a client against the fake API: %v", err)
	}
	return oxideClient
}

// testScheme is the client-go global scheme, which both this provider's types and karpenter's
// register themselves with in their package inits.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	return scheme.Scheme
}

// nodeClaim is a launched NodeClaim recording one anti-affinity group membership.
func nodeClaim(groups string) *karpv1.NodeClaim {
	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: "karpenter.oxide.computer",
				Kind:  "OxideNodeClass",
				Name:  "default",
			},
		},
		Status: karpv1.NodeClaimStatus{ProviderID: "oxide://" + instanceID},
	}
	if groups != "" {
		claim.Annotations = map[string]string{v1alpha1.AnnotationAffinityGroups: groups}
	}
	return claim
}

// The scenario the managed group is removed from a nodeclass produces: the desired set is empty, the
// node's membership is dropped, and the record of it has to go with it. Anything left behind is a
// group ID asserted on a node that is not in it, which is what drift is decided against.
func TestReconcileClearsTheAnnotationWhenNothingIsDesired(t *testing.T) {
	api := &memberAPI{}
	nodeClass := &v1alpha1.OxideNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	claim := nodeClaim("aa:" + groupID)

	kubeClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(nodeClass, claim).
		Build()

	c := NewController(kubeClient, api.server(t))
	if _, err := c.Reconcile(context.Background(), claim); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if len(api.deletes) != 1 || api.deletes[0] != groupID+"/"+instanceID {
		t.Errorf("member deletes = %v, want the one membership removed", api.deletes)
	}

	updated := &karpv1.NodeClaim{}
	if err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: "node-1"},
		updated,
	); err != nil {
		t.Fatalf("unable to re-read the NodeClaim: %v", err)
	}
	if value, ok := updated.Annotations[v1alpha1.AnnotationAffinityGroups]; ok {
		t.Errorf(
			"the annotation is still present with value %q, want it removed entirely", value,
		)
	}
}

// The nodeclass controller deletes a managed group as soon as it is empty, so the group can be gone
// by the time this runs. A membership that cannot be found is the state being asked for, so the
// record still has to be cleared.
func TestReconcileClearsTheAnnotationWhenTheGroupIsAlreadyGone(t *testing.T) {
	api := &memberAPI{gone: true}
	nodeClass := &v1alpha1.OxideNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	claim := nodeClaim("aa:" + groupID)

	kubeClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(nodeClass, claim).
		Build()

	c := NewController(kubeClient, api.server(t))
	if _, err := c.Reconcile(context.Background(), claim); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	updated := &karpv1.NodeClaim{}
	if err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: "node-1"},
		updated,
	); err != nil {
		t.Fatalf("unable to re-read the NodeClaim: %v", err)
	}
	if value, ok := updated.Annotations[v1alpha1.AnnotationAffinityGroups]; ok {
		t.Errorf("the annotation is still present with value %q, want it removed entirely", value)
	}
}

// A group still in the desired set is left alone. Additions are the launch's business, and this
// controller never touches a membership the nodeclass still wants.
func TestReconcileKeepsAMembershipStillDesired(t *testing.T) {
	api := &memberAPI{}
	nodeClass := &v1alpha1.OxideNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Status: v1alpha1.OxideNodeClassStatus{
			AffinityGroups: []v1alpha1.OxideAffinityGroup{{
				ID:      groupID,
				Name:    "default-aa-a",
				Type:    v1alpha1.AffinityGroupTypeAntiAffinity,
				Managed: true,
			}},
		},
	}
	claim := nodeClaim("aa:" + groupID)

	kubeClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(nodeClass, claim).
		Build()

	c := NewController(kubeClient, api.server(t))
	if _, err := c.Reconcile(context.Background(), claim); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if len(api.deletes) != 0 {
		t.Errorf("member deletes = %v, want none", api.deletes)
	}
	updated := &karpv1.NodeClaim{}
	if err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: "node-1"},
		updated,
	); err != nil {
		t.Fatalf("unable to re-read the NodeClaim: %v", err)
	}
	if got := updated.Annotations[v1alpha1.AnnotationAffinityGroups]; got != "aa:"+groupID {
		t.Errorf("annotation = %q, want it untouched", got)
	}
}
