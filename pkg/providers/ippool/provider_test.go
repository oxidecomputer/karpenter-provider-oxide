package ippool

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
	ipPoolID  = "66666666-0000-4000-8000-000000000001"
	ipPoolIDB = "66666666-0000-4000-8000-000000000002"
)

// fakeAPI is the Oxide IP pool API, counting the calls so that a cache hit can be asserted as the
// absence of one.
type fakeAPI struct {
	pools []oxide.SiloIpPool
	lists int
	views int
}

func (f *fakeAPI) server(t *testing.T) *oxide.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case r.Method == http.MethodGet && path == "ip-pools":
			f.lists++
			writeJSON(t, w, http.StatusOK, map[string]any{"items": f.pools})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "ip-pools/"):
			f.views++
			name := strings.TrimPrefix(path, "ip-pools/")
			for _, pool := range f.pools {
				if string(pool.Name) == name || pool.Id == name {
					writeJSON(t, w, http.StatusOK, pool)
					return
				}
			}
			writeJSON(t, w, http.StatusNotFound, map[string]string{"error_code": "ObjectNotFound"})

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

func pool(id, name string, isDefault bool, version oxide.IpVersion) oxide.SiloIpPool {
	return oxide.SiloIpPool{
		Id:        id,
		Name:      oxide.Name(name),
		IsDefault: &isDefault,
		IpVersion: version,
	}
}

func fakePools() []oxide.SiloIpPool {
	return []oxide.SiloIpPool{
		pool(ipPoolID, "public-v4", true, oxide.IpVersionV4),
		pool(ipPoolIDB, "public-v6", false, oxide.IpVersionV6),
	}
}

func nodeClass(selectors ...v1alpha1.IPPoolSelector) *v1alpha1.OxideNodeClass {
	return &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{IPPoolSelector: selectors},
	}
}

func TestResolveSelectsTheDefaultPool(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), time.Hour)

	got, err := provider.Resolve(
		context.Background(),
		nodeClass(v1alpha1.IPPoolSelector{Default: true}),
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 1 || got.Resolved[0].ID != ipPoolID {
		t.Errorf("Resolve() = %v, want only the default pool", got.Resolved)
	}
}

// A pool named in a selector is looked up directly rather than listed, since that is the one form
// the API can resolve on its own.
func TestANamedPoolIsViewedRatherThanListed(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), time.Hour)

	if _, err := provider.Resolve(
		context.Background(),
		nodeClass(v1alpha1.IPPoolSelector{Name: "public-v4"}),
	); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if api.views != 1 || api.lists != 0 {
		t.Errorf(
			"the API was viewed %d times and listed %d, want one view and no list",
			api.views,
			api.lists,
		)
	}
}

func TestResolveServesACacheHit(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), time.Hour)
	class := nodeClass(v1alpha1.IPPoolSelector{Default: true})

	for range 3 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 1 {
		t.Errorf("the API was listed %d times across three resolutions, want 1", api.lists)
	}
}

// A zero TTL makes every reading immediately due, which is the same code path as a reading that has
// aged out without a test having to wait for it.
func TestResolvePastTheTTLListsAgain(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), 0)
	class := nodeClass(v1alpha1.IPPoolSelector{Default: true})

	for range 2 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 2 {
		t.Errorf("the API was listed %d times, want one per resolution", api.lists)
	}
}

func TestAnEditedSelectorResolvesAgain(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), time.Hour)

	for _, version := range []string{"", "v6"} {
		class := nodeClass(v1alpha1.IPPoolSelector{Default: true, IPVersion: version})
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 2 {
		t.Errorf("the API was listed %d times for two different selectors, want 2", api.lists)
	}
}

func TestTwoNodeClassesShareOneReading(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), time.Hour)

	second := nodeClass(v1alpha1.IPPoolSelector{Default: true})
	second.ObjectMeta = metav1.ObjectMeta{Name: "another"}
	for _, class := range []*v1alpha1.OxideNodeClass{nodeClass(v1alpha1.IPPoolSelector{Default: true}), second} {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 1 {
		t.Errorf(
			"the API was listed %d times for two nodeclasses selecting the same pools, want 1",
			api.lists,
		)
	}
}

func TestDriftReason(t *testing.T) {
	tests := []struct {
		name     string
		recorded *string
		desired  []v1alpha1.OxideIPPool
		want     karpcloudprovider.DriftReason
	}{
		{
			name:     "a node with addresses from the selected pools has not drifted",
			recorded: new(ipPoolID),
			desired:  []v1alpha1.OxideIPPool{{ID: ipPoolID}},
		},
		{
			// Both directions: one ephemeral address is attached per selected pool at launch, and
			// nothing attaches or detaches one afterwards.
			name:     "a pool added to the set replaces the nodes without an address from it",
			recorded: new(ipPoolID),
			desired:  []v1alpha1.OxideIPPool{{ID: ipPoolID}, {ID: ipPoolIDB}},
			want:     providers.DriftReasonIPPool,
		},
		{
			name:     "and a pool removed replaces the nodes still holding one of its addresses",
			recorded: new(ipPoolID + "," + ipPoolIDB),
			desired:  []v1alpha1.OxideIPPool{{ID: ipPoolID}},
			want:     providers.DriftReasonIPPool,
		},
		{
			name:     "a launch that recorded no pools and a nodeclass selecting none agree",
			recorded: new(""),
		},
		{
			name:    "a NodeClaim launched before the record existed cannot be compared",
			desired: []v1alpha1.OxideIPPool{{ID: ipPoolID}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim := &karpv1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			}
			if tt.recorded != nil {
				nodeClaim.Annotations[v1alpha1.AnnotationIPPools] = *tt.recorded
			}
			class := &v1alpha1.OxideNodeClass{
				Status: v1alpha1.OxideNodeClassStatus{IPPools: tt.desired},
			}
			if got := (&Provider{}).DriftReason(nodeClaim, class); got != tt.want {
				t.Errorf("DriftReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

//go:fix inline
