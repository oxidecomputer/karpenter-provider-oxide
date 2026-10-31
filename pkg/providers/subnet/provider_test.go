package subnet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/selection"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/oxide.go/oxide"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const (
	projectName    = "karpenter"
	projectID      = "11111111-0000-4000-8000-000000000001"
	otherProjectID = "11111111-0000-4000-8000-000000000002"
	vpcID          = "22222222-0000-4000-8000-000000000001"
	otherVPCID     = "22222222-0000-4000-8000-000000000002"
	subnetID       = "44444444-0000-4000-8000-000000000001"
	subnetIDB      = "44444444-0000-4000-8000-000000000002"
)

type fakeAPI struct {
	subnets      []oxide.VpcSubnet
	subnetsByVPC map[string][]oxide.VpcSubnet
	// vpcs, when set, is what the VPC view serves by ID instead of the one VPC named prod.
	vpcs map[string]oxide.Vpc
	// vpcList is what a VPC listing serves.
	vpcList  []oxide.Vpc
	lists    int
	vpcLists int
	vpcViews int
	// viewQuery is the query string of the last subnet view, to check what was sent alongside an ID.
	viewQuery string
}

func newProvider(t *testing.T, api *fakeAPI, ttl time.Duration) *Provider {
	t.Helper()
	client := api.server(t)
	return NewProvider(client, project.NewResolver(client, projectName, ttl), ttl)
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

		case r.Method == http.MethodGet && strings.HasPrefix(path, "vpc-subnets/"):
			f.viewQuery = r.URL.RawQuery
			id := strings.TrimPrefix(path, "vpc-subnets/")
			for _, subnet := range f.subnets {
				if subnet.Id == id {
					writeJSON(t, w, http.StatusOK, subnet)
					return
				}
			}
			writeJSON(t, w, http.StatusNotFound, map[string]string{"error_code": "ObjectNotFound"})

		case r.Method == http.MethodGet && path == "vpcs":
			f.vpcLists++
			writeJSON(t, w, http.StatusOK, map[string]any{"items": f.vpcList})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "vpcs/"):
			f.vpcViews++
			vpc := oxide.Vpc{Id: vpcID, Name: "prod", ProjectId: projectID}
			if f.vpcs != nil {
				vpc = f.vpcs[strings.TrimPrefix(path, "vpcs/")]
			}
			writeJSON(t, w, http.StatusOK, vpc)

		case r.Method == http.MethodGet && path == "projects/"+projectName:
			writeJSON(t, w, http.StatusOK, oxide.Project{Id: projectID, Name: projectName})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := oxide.NewClient(
		oxide.WithHost(srv.URL),
		oxide.WithToken("token"),
		projecttest.WithGuard(t),
	)
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
	provider := newProvider(t, api, time.Hour)

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
	if got != "no subnets matched {name=web, vpcName=prod}" {
		t.Errorf("DescribeSelectors() = %q", got)
	}
	if got := DescribeSelectors(nil); got != "no subnet selectors are configured" {
		t.Errorf("DescribeSelectors(nil) = %q", got)
	}
}

func TestASelectorNamingTheVPCNeedsNoLookup(t *testing.T) {
	subnets := fakeSubnets()
	subnets[1].VpcId = "22222222-0000-4000-8000-000000000002"
	api := &fakeAPI{subnets: subnets}
	provider := newProvider(t, api, time.Hour)

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
			provider := newProvider(t, api, 0)
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
	provider := newProvider(t, api, 0)
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

func TestASubnetIDSendsOnlyTheIDAndChecksTheRestAfterwards(t *testing.T) {
	tests := []struct {
		name     string
		selector v1alpha1.SubnetSelector
		vpcs     map[string]oxide.Vpc
		want     int
		reason   string
	}{
		{
			name:     "a subnet in the controller's project needs nothing else written",
			selector: v1alpha1.SubnetSelector{ID: subnetID},
			want:     1,
		},
		{
			name:     "a subnet in the VPC the selector named",
			selector: v1alpha1.SubnetSelector{ID: subnetID, VPCName: "prod"},
			want:     1,
		},
		{
			name:     "a subnet in the VPC and project the selector identified",
			selector: v1alpha1.SubnetSelector{ID: subnetID, VPCID: vpcID, Project: projectID},
			want:     1,
		},
		{
			name:     "a subnet outside the VPC the selector named is a mismatch",
			selector: v1alpha1.SubnetSelector{ID: subnetID, VPCName: "staging"},
			reason:   v1alpha1.ConditionReasonProjectMismatch,
		},
		{
			name:     "a subnet outside the VPC the selector identified is a mismatch",
			selector: v1alpha1.SubnetSelector{ID: subnetID, VPCID: otherVPCID},
			reason:   v1alpha1.ConditionReasonProjectMismatch,
		},
		{
			name:     "a subnet outside the controller's project is a mismatch",
			selector: v1alpha1.SubnetSelector{ID: subnetID},
			vpcs: map[string]oxide.Vpc{
				vpcID: {Id: vpcID, Name: "prod", ProjectId: otherProjectID},
			},
			reason: v1alpha1.ConditionReasonProjectMismatch,
		},
		{
			name:     "a subnet outside the project the selector named is a mismatch",
			selector: v1alpha1.SubnetSelector{ID: subnetID, Project: otherProjectID},
			reason:   v1alpha1.ConditionReasonProjectMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{subnets: fakeSubnets(), vpcs: tt.vpcs}
			provider := newProvider(t, api, time.Hour)

			got, err := provider.Resolve(context.Background(), &v1alpha1.OxideNodeClass{
				Spec: v1alpha1.OxideNodeClassSpec{
					SubnetSelector: []v1alpha1.SubnetSelector{tt.selector},
				},
			})
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if api.viewQuery != "" {
				t.Errorf("the view sent %q alongside the ID, want nothing", api.viewQuery)
			}
			if len(got.Resolved) != tt.want {
				t.Errorf("Resolve() matched %d subnets, want %d", len(got.Resolved), tt.want)
			}
			if got.Reason != tt.reason {
				t.Errorf("Resolve() reason = %q, want %q: %s", got.Reason, tt.reason, got.Message)
			}
		})
	}
}

func TestAWildcardSubnetNameFiltersTheVPCListing(t *testing.T) {
	api := &fakeAPI{subnets: fakeSubnets()}
	provider := newProvider(t, api, time.Hour)

	got, err := provider.Resolve(context.Background(), &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{SubnetSelector: []v1alpha1.SubnetSelector{
			{VPCName: "prod", Name: "*-b"},
		}},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 1 || got.Resolved[0].Name != "web-b" {
		t.Errorf("Resolve() = %v, want only web-b", got.Resolved)
	}
	if api.lists != 1 || api.vpcViews != 0 {
		t.Errorf("subnets listed %d times, VPC viewed %d, want 1 and 0", api.lists, api.vpcViews)
	}
}

func TestAWildcardVPCNameListsTheProjectVPCs(t *testing.T) {
	other := fakeSubnets()
	other[0].Id, other[0].VpcId, other[0].Name = "44444444-0000-4000-8000-000000000003", otherVPCID, "db-a"
	api := &fakeAPI{
		subnetsByVPC: map[string][]oxide.VpcSubnet{vpcID: fakeSubnets(), otherVPCID: other[:1]},
		vpcList: []oxide.Vpc{
			{Id: vpcID, Name: "prod", ProjectId: projectID},
			{Id: otherVPCID, Name: "prod-db", ProjectId: projectID},
			{Id: "22222222-0000-4000-8000-000000000003", Name: "staging", ProjectId: projectID},
		},
	}
	provider := newProvider(t, api, time.Hour)

	got, err := provider.Resolve(context.Background(), &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{SubnetSelector: []v1alpha1.SubnetSelector{
			{VPCName: "prod*"},
		}},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 3 {
		t.Fatalf("Resolve() matched %d subnets, want 3 across both prod VPCs", len(got.Resolved))
	}
	byID := map[string]string{}
	for _, subnet := range got.Resolved {
		byID[subnet.ID] = subnet.VPCName
	}
	if byID[subnetID] != "prod" || byID[other[0].Id] != "prod-db" {
		t.Errorf("VPC names = %v, want them taken from the listing", byID)
	}
	if api.vpcLists != 1 || api.vpcViews != 0 || api.lists != 2 {
		t.Errorf(
			"VPCs listed %d times, viewed %d, subnets listed %d; want 1, 0, 2",
			api.vpcLists, api.vpcViews, api.lists,
		)
	}
}

func TestAVPCIDOutsideTheProjectIsAMismatch(t *testing.T) {
	api := &fakeAPI{
		subnets: fakeSubnets(),
		vpcs:    map[string]oxide.Vpc{vpcID: {Id: vpcID, Name: "prod", ProjectId: otherProjectID}},
	}
	provider := newProvider(t, api, time.Hour)

	got, err := provider.Resolve(context.Background(), byVPCID())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Reason != v1alpha1.ConditionReasonProjectMismatch || len(got.Resolved) != 0 {
		t.Errorf(
			"Resolve() = %d subnets with reason %q, want none and a project mismatch",
			len(got.Resolved), got.Reason,
		)
	}
	if !strings.Contains(got.Message, otherProjectID) {
		t.Errorf("Resolve() message = %q, want the project the VPC is in", got.Message)
	}
}

func TestResolveServesACacheHit(t *testing.T) {
	api := &fakeAPI{subnets: fakeSubnets()}
	provider := newProvider(t, api, time.Hour)
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

	provider := newProvider(t, api, 0)
	provider.vpcs = newVPCCache(time.Hour)
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
	provider := newProvider(t, api, time.Hour)

	for _, vpc := range []string{vpcID, "22222222-0000-4000-8000-000000000002"} {
		class := byVPCID()
		class.Spec.SubnetSelector[0].VPCID = selection.IDSelector(vpc)
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
	provider := newProvider(t, api, time.Hour)

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
				Annotations: map[string]string{},
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
