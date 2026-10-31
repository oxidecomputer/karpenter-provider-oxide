package ippool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/selection"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/oxide.go/oxide"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const (
	ipPoolID  = "66666666-0000-4000-8000-000000000001"
	ipPoolIDB = "66666666-0000-4000-8000-000000000002"
)

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

func pool(id, name string, isDefault bool, version oxide.IpVersion) oxide.SiloIpPool {
	return oxide.SiloIpPool{
		Id:        id,
		Name:      oxide.Name(name),
		IsDefault: &isDefault,
		IpVersion: version,
	}
}

func createdAt(pool oxide.SiloIpPool, year int) oxide.SiloIpPool {
	pool.TimeCreated = new(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC))
	return pool
}

func fakePools() []oxide.SiloIpPool {
	return []oxide.SiloIpPool{
		pool(ipPoolID, "public-v4", true, oxide.IpVersionV4),
		pool(ipPoolIDB, "public-v6", false, oxide.IpVersionV6),
	}
}

func nodeClass(ephemeralIPs ...v1alpha1.EphemeralIP) *v1alpha1.OxideNodeClass {
	return &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{EphemeralIPs: ephemeralIPs},
	}
}

func byName(name string) *v1alpha1.IPPoolRef {
	return &v1alpha1.IPPoolRef{Name: selection.WildcardSelector(name)}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name         string
		ephemeralIPs []v1alpha1.EphemeralIP
		want         []v1alpha1.OxideEphemeralIP
		wantReason   string
		wantViews    int
	}{
		{
			name:         "a pool named by name is viewed and takes its version from the pool",
			ephemeralIPs: []v1alpha1.EphemeralIP{{Pool: byName("public-v6")}},
			want: []v1alpha1.OxideEphemeralIP{
				{IPVersion: "v6", PoolID: ipPoolIDB, PoolName: "public-v6"},
			},
			wantViews: 1,
		},
		{
			name:         "a pool named by ID resolves the same way",
			ephemeralIPs: []v1alpha1.EphemeralIP{{Pool: &v1alpha1.IPPoolRef{ID: ipPoolID}}},
			want: []v1alpha1.OxideEphemeralIP{
				{IPVersion: "v4", PoolID: ipPoolID, PoolName: "public-v4"},
			},
			wantViews: 1,
		},
		{

			name:         "an entry with no pool takes the silo default without an API call",
			ephemeralIPs: []v1alpha1.EphemeralIP{{IPVersion: "v4"}},
			want:         []v1alpha1.OxideEphemeralIP{{IPVersion: "v4"}},
		},
		{
			name: "an entry setting both a version and a pool that agree resolves",
			ephemeralIPs: []v1alpha1.EphemeralIP{
				{IPVersion: "v4", Pool: byName("public-v4")},
			},
			want: []v1alpha1.OxideEphemeralIP{
				{IPVersion: "v4", PoolID: ipPoolID, PoolName: "public-v4"},
			},
			wantViews: 1,
		},
		{
			name: "an entry whose pool allocates the other family is reported",
			ephemeralIPs: []v1alpha1.EphemeralIP{
				{IPVersion: "v6", Pool: byName("public-v4")},
			},
			wantReason: v1alpha1.ConditionReasonIPVersionMismatch,
			wantViews:  1,
		},
		{
			name: "two entries spanning both families resolve",
			ephemeralIPs: []v1alpha1.EphemeralIP{
				{IPVersion: "v4"},
				{Pool: byName("public-v6")},
			},
			want: []v1alpha1.OxideEphemeralIP{
				{IPVersion: "v4"},
				{IPVersion: "v6", PoolID: ipPoolIDB, PoolName: "public-v6"},
			},
			wantViews: 1,
		},
		{

			name: "two entries resolving to one family are refused",
			ephemeralIPs: []v1alpha1.EphemeralIP{
				{IPVersion: "v4"},
				{Pool: byName("public-v4")},
			},
			wantReason: v1alpha1.ConditionReasonDuplicateIPVersion,
			wantViews:  1,
		},
		{
			name:         "a pool that does not exist is reported rather than retried",
			ephemeralIPs: []v1alpha1.EphemeralIP{{Pool: byName("missing")}},
			wantReason:   v1alpha1.ConditionReasonIPPoolNotFound,
			wantViews:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{pools: fakePools()}
			provider := NewProvider(api.server(t), time.Hour)

			got, err := provider.Resolve(context.Background(), nodeClass(tt.ephemeralIPs...))
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got.Reason != tt.wantReason {
				t.Fatalf("Resolve() reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if tt.wantReason != "" {
				if got.Message == "" {
					t.Error("Resolve() recorded no message alongside its reason")
				}
				if len(got.Unmatched) == 0 {
					t.Error("Resolve() named none of the entries that failed")
				}
				if got.Resolved != nil {
					t.Errorf(
						"Resolve() published %v, want nothing on an incomplete resolution",
						got.Resolved,
					)
				}
			}
			if len(got.Resolved) != len(tt.want) {
				t.Fatalf("Resolve() = %v, want %v", got.Resolved, tt.want)
			}
			for i, want := range tt.want {
				if !reflect.DeepEqual(got.Resolved[i], want) {
					t.Errorf("Resolve()[%d] = %+v, want %+v", i, got.Resolved[i], want)
				}
			}
			if api.views != tt.wantViews {
				t.Errorf("the API was viewed %d times, want %d", api.views, tt.wantViews)
			}

			if api.lists != 0 {
				t.Errorf("the API was listed %d times, want none", api.lists)
			}
		})
	}
}

func TestAWildcardTakesTheOldestPoolAndKeepsTheRestAsFallbacks(t *testing.T) {
	const (
		ipPoolIDC = "66666666-0000-4000-8000-000000000003"
		ipPoolIDD = "66666666-0000-4000-8000-000000000004"
	)
	pools := []oxide.SiloIpPool{
		createdAt(pool(ipPoolID, "public-v4-2024", false, oxide.IpVersionV4), 2024),
		createdAt(pool(ipPoolIDC, "public-v4-2023", false, oxide.IpVersionV4), 2023),
		createdAt(pool(ipPoolIDB, "public-v6", false, oxide.IpVersionV6), 2022),
		pool(ipPoolIDD, "public-v4-undated", false, oxide.IpVersionV4),
	}
	tests := []struct {
		name    string
		entry   v1alpha1.EphemeralIP
		want    v1alpha1.OxideEphemeralIP
		reason  string
		message string
	}{
		{
			name:  "one match has no fallbacks",
			entry: v1alpha1.EphemeralIP{Pool: byName("*-v6")},
			want: v1alpha1.OxideEphemeralIP{
				IPVersion: "v6",
				PoolID:    ipPoolIDB,
				PoolName:  "public-v6",
			},
		},
		{
			name:  "several matches are ordered oldest first, undated last",
			entry: v1alpha1.EphemeralIP{IPVersion: "v4", Pool: byName("public-v4-*")},
			want: v1alpha1.OxideEphemeralIP{
				IPVersion: "v4", PoolID: ipPoolIDC, PoolName: "public-v4-2023",
				Fallbacks: []v1alpha1.OxideIPPool{
					{ID: ipPoolID, Name: "public-v4-2024"},
					{ID: ipPoolIDD, Name: "public-v4-undated"},
				},
			},
		},
		{
			name:  "without a version the oldest match sets the family and the other family is left out",
			entry: v1alpha1.EphemeralIP{Pool: byName("public-*")},
			want: v1alpha1.OxideEphemeralIP{
				IPVersion: "v6",
				PoolID:    ipPoolIDB,
				PoolName:  "public-v6",
			},
		},
		{
			name:    "a version keeps only the pools of that family",
			entry:   v1alpha1.EphemeralIP{IPVersion: "v6", Pool: byName("public-v4-*")},
			reason:  v1alpha1.ConditionReasonIPVersionMismatch,
			message: "public-v4-2023, public-v4-2024, public-v4-undated",
		},
		{
			name:   "no match is not found",
			entry:  v1alpha1.EphemeralIP{Pool: byName("private-*")},
			reason: v1alpha1.ConditionReasonIPPoolNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{pools: pools}
			provider := NewProvider(api.server(t), time.Hour)

			got, err := provider.Resolve(context.Background(), nodeClass(tt.entry))
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got.Reason != tt.reason {
				t.Errorf("Resolve() reason = %q, want %q: %s", got.Reason, tt.reason, got.Message)
			}
			if !strings.Contains(got.Message, tt.message) {
				t.Errorf("Resolve() message = %q, want it to name %q", got.Message, tt.message)
			}
			if tt.reason == "" &&
				(len(got.Resolved) != 1 || !reflect.DeepEqual(got.Resolved[0], tt.want)) {
				t.Errorf("Resolve() = %+v, want %+v", got.Resolved, tt.want)
			}
			if api.lists != 1 || api.views != 0 {
				t.Errorf(
					"the API was listed %d times and viewed %d, want 1 and 0",
					api.lists,
					api.views,
				)
			}
		})
	}
}

func TestResolveWithoutEntries(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), time.Hour)

	got, err := provider.Resolve(context.Background(), nodeClass())
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 0 || !got.Complete() {
		t.Errorf("Resolve() = %+v, want an empty complete resolution", got)
	}
	if api.views != 0 || api.lists != 0 {
		t.Errorf("the API was called %d times, want none", api.views+api.lists)
	}
}

func TestResolveServesACacheHit(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), time.Hour)
	class := nodeClass(v1alpha1.EphemeralIP{Pool: byName("public-v4")})

	for range 3 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.views != 1 {
		t.Errorf("the API was viewed %d times across three resolutions, want 1", api.views)
	}
}

func TestResolvePastTheTTLViewsAgain(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), 0)
	class := nodeClass(v1alpha1.EphemeralIP{Pool: byName("public-v4")})

	for range 2 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.views != 2 {
		t.Errorf("the API was viewed %d times, want one per resolution", api.views)
	}
}

func TestAnEditedEntryResolvesAgain(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), time.Hour)

	for _, name := range []string{"public-v4", "public-v6"} {
		class := nodeClass(v1alpha1.EphemeralIP{Pool: byName(name)})
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.views != 2 {
		t.Errorf("the API was viewed %d times for two different entries, want 2", api.views)
	}
}

func TestTwoNodeClassesShareOneReading(t *testing.T) {
	api := &fakeAPI{pools: fakePools()}
	provider := NewProvider(api.server(t), time.Hour)

	second := nodeClass(v1alpha1.EphemeralIP{Pool: byName("public-v4")})
	second.ObjectMeta = metav1.ObjectMeta{Name: "another"}
	classes := []*v1alpha1.OxideNodeClass{
		nodeClass(v1alpha1.EphemeralIP{Pool: byName("public-v4")}),
		second,
	}
	for _, class := range classes {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.views != 1 {
		t.Errorf(
			"the API was viewed %d times for two nodeclasses declaring the same entries, want 1",
			api.views,
		)
	}
}

func TestDriftReason(t *testing.T) {
	tests := []struct {
		name     string
		recorded *string
		desired  []v1alpha1.OxideEphemeralIP
		want     karpcloudprovider.DriftReason
	}{
		{
			name:     "a node with an address from the declared pool has not drifted",
			recorded: new("v4=192.0.2.10@" + ipPoolID),
			desired:  []v1alpha1.OxideEphemeralIP{{IPVersion: "v4", PoolID: ipPoolID}},
		},
		{
			name:     "an entry added is an address the node cannot be given in place",
			recorded: new("v4=192.0.2.10@" + ipPoolID),
			desired: []v1alpha1.OxideEphemeralIP{
				{IPVersion: "v4", PoolID: ipPoolID},
				{IPVersion: "v6", PoolID: ipPoolIDB},
			},
			want: providers.DriftReasonEphemeralIP,
		},
		{
			name:     "and an entry removed is an address it still holds",
			recorded: new("v4=192.0.2.10@" + ipPoolID + ",v6=2001:db8::1@" + ipPoolIDB),
			desired:  []v1alpha1.OxideEphemeralIP{{IPVersion: "v4", PoolID: ipPoolID}},
			want:     providers.DriftReasonEphemeralIP,
		},
		{

			name:     "swapping which pool serves which family is drift",
			recorded: new("v4=192.0.2.10@" + ipPoolID + ",v6=2001:db8::1@" + ipPoolIDB),
			desired: []v1alpha1.OxideEphemeralIP{
				{IPVersion: "v4", PoolID: ipPoolIDB},
				{IPVersion: "v6", PoolID: ipPoolID},
			},
			want: providers.DriftReasonEphemeralIP,
		},
		{
			name:     "a pool swapped for another of the same family is drift",
			recorded: new("v4=192.0.2.10@" + ipPoolID),
			desired:  []v1alpha1.OxideEphemeralIP{{IPVersion: "v4", PoolID: ipPoolIDB}},
			want:     providers.DriftReasonEphemeralIP,
		},
		{
			name:     "a node that fell back to a later pool has not drifted",
			recorded: new("v4=192.0.2.10@" + ipPoolIDB),
			desired: []v1alpha1.OxideEphemeralIP{{
				IPVersion: "v4",
				PoolID:    ipPoolID,
				Fallbacks: []v1alpha1.OxideIPPool{{ID: ipPoolIDB}},
			}},
		},
		{

			name:     "an entry left to the silo default compares on family alone",
			recorded: new("v4=192.0.2.10@" + ipPoolIDB),
			desired:  []v1alpha1.OxideEphemeralIP{{IPVersion: "v4"}},
		},
		{
			name:     "but it still drifts when the family it defaults for changes",
			recorded: new("v4=192.0.2.10@" + ipPoolID),
			desired:  []v1alpha1.OxideEphemeralIP{{IPVersion: "v6"}},
			want:     providers.DriftReasonEphemeralIP,
		},
		{
			name:     "a launch that recorded no addresses and a nodeclass declaring none agree",
			recorded: new(""),
		},
		{
			name:     "a node launched with no address drifts once one is declared",
			recorded: new(""),
			desired:  []v1alpha1.OxideEphemeralIP{{IPVersion: "v4"}},
			want:     providers.DriftReasonEphemeralIP,
		},
		{
			name:    "a NodeClaim launched before the record existed cannot be compared",
			desired: []v1alpha1.OxideEphemeralIP{{IPVersion: "v4", PoolID: ipPoolID}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim := &karpv1.NodeClaim{
				Annotations: map[string]string{},
			}
			if tt.recorded != nil {
				nodeClaim.Annotations[v1alpha1.AnnotationEphemeralIPs] = *tt.recorded
			}
			class := &v1alpha1.OxideNodeClass{
				Status: v1alpha1.OxideNodeClassStatus{EphemeralIPs: tt.desired},
			}
			if got := (&Provider{}).DriftReason(nodeClaim, class); got != tt.want {
				t.Errorf("DriftReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

//go:fix inline
