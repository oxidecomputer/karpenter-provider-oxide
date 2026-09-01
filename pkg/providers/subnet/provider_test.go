package subnet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/oxide.go/oxide"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const (
	vpcID     = "22222222-0000-4000-8000-000000000001"
	subnetID  = "44444444-0000-4000-8000-000000000001"
	subnetIDB = "44444444-0000-4000-8000-000000000002"
)

type fakeAPI struct {
	subnets      []oxide.VpcSubnet
	subnetsByVPC map[string][]oxide.VpcSubnet
	lists        int
	vpcViews     int
}

func (f *fakeAPI) server(t *testing.T) *oxide.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case r.Method == http.MethodGet && path == "vpc-subnets":
			f.lists++
			subnets := f.subnets
			if f.subnetsByVPC != nil {
				subnets = f.subnetsByVPC[r.URL.Query().Get("vpc")]
			}
			writeJSON(t, w, http.StatusOK, map[string]any{"items": subnets})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "vpcs/"):
			f.vpcViews++
			writeJSON(t, w, http.StatusOK, oxide.Vpc{Id: vpcID, Name: "prod"})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := oxide.NewClient(oxide.WithHost(srv.URL), oxide.WithToken("token"))
	if err != nil {
		t.Fatalf("unable to build a client against the fake API: %v", err)
	}
	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("unable to encode the fake response: %v", err)
	}
}

func fakeSubnets() []oxide.VpcSubnet {
	created := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	return []oxide.VpcSubnet{
		{Id: subnetID, Name: "web-a", VpcId: vpcID, TimeCreated: &created, TimeModified: &created},
		{Id: subnetIDB, Name: "web-b", VpcId: vpcID, TimeCreated: &created, TimeModified: &created},
	}
}

func byVPCID() *v1alpha1.OxideNodeClass {
	return &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			SubnetSelector: []v1alpha1.SubnetSelector{{VPCID: vpcID}},
		},
	}
}

func TestResolveReturnsTheSubnetsWithTheirVPCName(t *testing.T) {
	api := &fakeAPI{subnets: fakeSubnets()}
	provider := NewProvider(api.server(t), time.Hour)

	got, err := provider.Resolve(context.Background(), byVPCID())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 2 {
		t.Fatalf("Resolve() matched %d subnets, want 2", len(got.Resolved))
	}
	for _, resolved := range got.Resolved {

		if resolved.VPCName != "prod" {
			t.Errorf(
				"Resolve() subnet %q VPC name = %q, want the VPC's name",
				resolved.Name,
				resolved.VPCName,
			)
		}
	}
}

func TestDescribeSelectors(t *testing.T) {
	got := DescribeSelectors([]v1alpha1.SubnetSelector{{Name: "web", VPCName: "prod"}})
	if got != "{name=web, vpcName=prod}" {
		t.Errorf("DescribeSelectors() = %q", got)
	}
}

func TestASelectorNamingTheVPCNeedsNoLookup(t *testing.T) {
	subnets := fakeSubnets()
	subnets[1].VpcId = "22222222-0000-4000-8000-000000000002"
	api := &fakeAPI{subnets: subnets}
	provider := NewProvider(api.server(t), time.Hour)

	class := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			SubnetSelector: []v1alpha1.SubnetSelector{{VPCName: "prod"}},
		},
	}
	if _, err := provider.Resolve(context.Background(), class); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if api.vpcViews != 0 {
		t.Errorf(
			"the VPC was viewed %d times, want none for a selector that named it",
			api.vpcViews,
		)
	}
}

func TestResolveHandlesEmptyVPCsInAnySelectorOrder(t *testing.T) {
	tests := []struct {
		name      string
		selectors []v1alpha1.SubnetSelector
		want      int
	}{
		{name: "one empty VPC", selectors: []v1alpha1.SubnetSelector{{VPCName: "empty"}}},
		{
			name:      "empty after non-empty",
			selectors: []v1alpha1.SubnetSelector{{VPCName: "prod"}, {VPCName: "empty"}},
			want:      2,
		},
		{
			name:      "empty before non-empty",
			selectors: []v1alpha1.SubnetSelector{{VPCName: "empty"}, {VPCName: "prod"}},
			want:      2,
		},
		{
			name:      "several empty VPCs",
			selectors: []v1alpha1.SubnetSelector{{VPCName: "empty"}, {VPCName: "also-empty"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{subnetsByVPC: map[string][]oxide.VpcSubnet{
				"prod": fakeSubnets(), "empty": nil, "also-empty": nil,
			}}
			provider := NewProvider(api.server(t), 0)
			got, err := provider.Resolve(context.Background(), &v1alpha1.OxideNodeClass{
				Spec: v1alpha1.OxideNodeClassSpec{SubnetSelector: tt.selectors},
			})
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if len(got.Resolved) != tt.want {
				t.Errorf("resolved %d subnets, want %d", len(got.Resolved), tt.want)
			}
		})
	}
}

func TestResolveDeduplicatesOverlappingSelectorsByID(t *testing.T) {
	api := &fakeAPI{subnetsByVPC: map[string][]oxide.VpcSubnet{
		vpcID: fakeSubnets(), "prod": fakeSubnets(),
	}}
	provider := NewProvider(api.server(t), 0)
	got, err := provider.Resolve(context.Background(), &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{SubnetSelector: []v1alpha1.SubnetSelector{
			{VPCID: vpcID}, {VPCName: "prod"},
		}},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 2 {
		t.Fatalf("Resolve() returned %d subnets, want two unique IDs", len(got.Resolved))
	}
	for _, resolved := range got.Resolved {
		if resolved.VPCName != "prod" {
			t.Errorf("subnet %s VPC name = %q, want prod", resolved.ID, resolved.VPCName)
		}
	}
}

func TestResolveServesACacheHit(t *testing.T) {
	api := &fakeAPI{subnets: fakeSubnets()}
	provider := NewProvider(api.server(t), time.Hour)
	class := byVPCID()

	for range 3 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 1 {
		t.Errorf("the API was listed %d times across three resolutions, want 1", api.lists)
	}
}

func TestTheVPCNameIsLookedUpOncePerVPC(t *testing.T) {
	api := &fakeAPI{subnets: fakeSubnets()}

	provider := NewProvider(api.server(t), 0)
	provider.vpcName = newVPCNameCache(time.Hour)
	class := byVPCID()

	for range 3 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 3 {
		t.Fatalf("the API was listed %d times, want one per resolution", api.lists)
	}
	if api.vpcViews != 1 {
		t.Errorf(
			"the VPC was viewed %d times across three resolutions of two subnets, want 1",
			api.vpcViews,
		)
	}
}

func TestAnEditedSelectorResolvesAgain(t *testing.T) {
	api := &fakeAPI{subnets: fakeSubnets()}
	provider := NewProvider(api.server(t), time.Hour)

	for _, vpc := range []string{vpcID, "22222222-0000-4000-8000-000000000002"} {
		class := byVPCID()
		class.Spec.SubnetSelector[0].VPCID = vpc
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 2 {
		t.Errorf("the API was listed %d times for two different selectors, want 2", api.lists)
	}
}

func TestTwoNodeClassesShareOneReading(t *testing.T) {
	api := &fakeAPI{subnets: fakeSubnets()}
	provider := NewProvider(api.server(t), time.Hour)

	first, second := byVPCID(), byVPCID()
	second.ObjectMeta = metav1.ObjectMeta{Name: "another"}
	for _, class := range []*v1alpha1.OxideNodeClass{first, second} {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 1 {
		t.Errorf(
			"the API was listed %d times for two nodeclasses selecting the same subnets, want 1",
			api.lists,
		)
	}
}

func TestDriftReason(t *testing.T) {
	tests := []struct {
		name     string
		recorded string
		desired  []v1alpha1.OxideSubnet
		want     karpcloudprovider.DriftReason
	}{
		{
			name:     "a node in a selected subnet has not drifted",
			recorded: subnetID,
			desired:  []v1alpha1.OxideSubnet{{ID: subnetID}},
		},
		{

			name:     "a subnet added to the set drifts nothing",
			recorded: subnetID,
			desired:  []v1alpha1.OxideSubnet{{ID: subnetID}, {ID: subnetIDB}},
		},
		{
			name:     "a subnet leaving the set replaces the nodes in it",
			recorded: subnetID,
			desired:  []v1alpha1.OxideSubnet{{ID: subnetIDB}},
			want:     providers.DriftReasonSubnet,
		},
		{
			name:     "an empty resolved set is not drift",
			recorded: subnetID,
		},
		{
			name:    "nor is a NodeClaim that recorded no subnet",
			desired: []v1alpha1.OxideSubnet{{ID: subnetIDB}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim := &karpv1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			}
			if tt.recorded != "" {
				nodeClaim.Annotations[v1alpha1.AnnotationSubnet] = tt.recorded
			}
			class := &v1alpha1.OxideNodeClass{
				Status: v1alpha1.OxideNodeClassStatus{Subnets: tt.desired},
			}
			if got := (&Provider{}).DriftReason(nodeClaim, class); got != tt.want {
				t.Errorf("DriftReason() = %q, want %q", got, tt.want)
			}
		})
	}
}
