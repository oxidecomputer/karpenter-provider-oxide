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
	corev1 "k8s.io/api/core/v1"
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

type memberAPI struct {
	gone bool

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

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	return scheme.Scheme
}

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

func TestReconcileClearsTheAnnotationWhenNothingIsDesired(t *testing.T) {
	api := &memberAPI{}
	nodeClass := &v1alpha1.OxideNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	claim := nodeClaim("aa:" + groupID)

	kubeClient := clientFor(t, nodeClass, claim)

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

func TestReconcileClearsTheAnnotationWhenTheGroupIsAlreadyGone(t *testing.T) {
	api := &memberAPI{gone: true}
	nodeClass := &v1alpha1.OxideNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	claim := nodeClaim("aa:" + groupID)

	kubeClient := clientFor(t, nodeClass, claim)

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

	kubeClient := clientFor(t, nodeClass, claim)

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

func node(groups string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec:       corev1.NodeSpec{ProviderID: "oxide://" + instanceID},
	}
	if groups != "" {
		n.Annotations = map[string]string{v1alpha1.AnnotationAffinityGroups: groups}
	}
	return n
}

func clientFor(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithIndex(&corev1.Node{}, "spec.providerID", func(o client.Object) []string {
			return []string{o.(*corev1.Node).Spec.ProviderID}
		}).
		Build()
}

func nodeAnnotation(t *testing.T, kubeClient client.Client) (string, bool) {
	t.Helper()
	updated := &corev1.Node{}
	if err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: "node-1"},
		updated,
	); err != nil {
		t.Fatalf("unable to re-read the Node: %v", err)
	}
	value, ok := updated.Annotations[v1alpha1.AnnotationAffinityGroups]
	return value, ok
}

func TestReconcileClearsTheAnnotationFromTheNodeToo(t *testing.T) {
	api := &memberAPI{}
	nodeClass := &v1alpha1.OxideNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	claim := nodeClaim("aa:" + groupID)

	kubeClient := clientFor(t, nodeClass, claim, node("aa:"+groupID))

	c := NewController(kubeClient, api.server(t))
	if _, err := c.Reconcile(context.Background(), claim); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if value, ok := nodeAnnotation(t, kubeClient); ok {
		t.Errorf("the node annotation is still present with value %q, want it removed", value)
	}
}

func TestReconcileClearsAStaleNodeAnnotationWithNothingOnTheNodeClaim(t *testing.T) {
	api := &memberAPI{}
	nodeClass := &v1alpha1.OxideNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	claim := nodeClaim("")

	kubeClient := clientFor(t, nodeClass, claim, node("aa:"+groupID))

	c := NewController(kubeClient, api.server(t))
	if _, err := c.Reconcile(context.Background(), claim); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if len(api.deletes) != 0 {
		t.Errorf(
			"member deletes = %v, want none: the NodeClaim recorded nothing to leave",
			api.deletes,
		)
	}
	if value, ok := nodeAnnotation(t, kubeClient); ok {
		t.Errorf("the stale node annotation is still present with value %q, want it removed", value)
	}
}

func TestReconcileLeavesTheNodeAloneWhenTheMembershipIsStillDesired(t *testing.T) {
	api := &memberAPI{}
	nodeClass := &v1alpha1.OxideNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Status: v1alpha1.OxideNodeClassStatus{
			AffinityGroups: []v1alpha1.OxideAffinityGroup{{
				ID:      groupID,
				Type:    v1alpha1.AffinityGroupTypeAntiAffinity,
				Managed: true,
			}},
		},
	}
	claim := nodeClaim("aa:" + groupID)

	kubeClient := clientFor(t, nodeClass, claim, node("aa:"+groupID))

	c := NewController(kubeClient, api.server(t))
	if _, err := c.Reconcile(context.Background(), claim); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	value, ok := nodeAnnotation(t, kubeClient)
	if !ok || value != "aa:"+groupID {
		t.Errorf("node annotation = %q (present %v), want it untouched", value, ok)
	}
}
