package sshkey

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	sshKeyID  = "55555555-0000-4000-8000-000000000001"
	sshKeyIDB = "55555555-0000-4000-8000-000000000002"
)

type fakeAPI struct {
	// keys is what a list serves; a view serves whatever name it was asked for.
	keys  []oxide.SshKey
	views int
	lists int
}

func (f *fakeAPI) server(t *testing.T) *oxide.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		if r.Method == http.MethodGet && path == "me/ssh-keys" {
			f.lists++
			writeJSON(t, w, http.StatusOK, map[string]any{"items": f.keys})
			return
		}
		if r.Method != http.MethodGet || !strings.HasPrefix(path, "me/ssh-keys/") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.views++
		name := strings.TrimPrefix(path, "me/ssh-keys/")
		for _, key := range f.keys {
			if string(key.Name) == name {
				writeJSON(t, w, http.StatusOK, key)
				return
			}
		}
		writeJSON(t, w, http.StatusOK, oxide.SshKey{
			Id:   uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String(),
			Name: oxide.Name(name),
		})
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

func nodeClass(names ...string) *v1alpha1.OxideNodeClass {
	class := &v1alpha1.OxideNodeClass{}
	for _, name := range names {
		class.Spec.SSHKeySelector = append(
			class.Spec.SSHKeySelector,
			v1alpha1.SSHKeySelector{Name: selection.WildcardSelector(name)},
		)
	}
	return class
}

func TestResolveReturnsTheNamedKeys(t *testing.T) {
	api := &fakeAPI{}
	provider := NewProvider(api.server(t), time.Hour)

	got, err := provider.Resolve(context.Background(), nodeClass("ops", "ci"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 2 {
		t.Fatalf("Resolve() matched %d keys, want 2", len(got.Resolved))
	}
	if got.Resolved[0].Name != "ops" || got.Resolved[1].Name != "ci" {
		t.Errorf("Resolve() = %v, want the keys in the order they were selected", got.Resolved)
	}
}

func TestAWildcardSelectsEveryMatchingKeyFromOneListing(t *testing.T) {
	api := &fakeAPI{keys: []oxide.SshKey{
		{Id: sshKeyID, Name: "ops-alice"},
		{Id: sshKeyIDB, Name: "ops-bob"},
		{Id: "55555555-0000-4000-8000-000000000003", Name: "ci"},
	}}
	provider := NewProvider(api.server(t), time.Hour)

	got, err := provider.Resolve(context.Background(), nodeClass("ops-*", "ops-alice", "*-bob"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	names := make([]string, 0, len(got.Resolved))
	for _, key := range got.Resolved {
		names = append(names, key.Name)
	}
	if len(names) != 2 || names[0] != "ops-alice" || names[1] != "ops-bob" {
		t.Errorf("Resolve() = %v, want the two ops keys once each", names)
	}
	if api.lists != 1 || api.views != 1 {
		t.Errorf(
			"the API was listed %d times and viewed %d, want one listing shared and one view",
			api.lists, api.views,
		)
	}
}

func TestResolveServesACacheHit(t *testing.T) {
	api := &fakeAPI{}
	provider := NewProvider(api.server(t), time.Hour)
	class := nodeClass("ops")

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
	api := &fakeAPI{}
	provider := NewProvider(api.server(t), 0)
	class := nodeClass("ops")

	for range 2 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.views != 2 {
		t.Errorf("the API was viewed %d times, want one per resolution", api.views)
	}
}

func TestAnEditedSelectorResolvesAgain(t *testing.T) {
	api := &fakeAPI{}
	provider := NewProvider(api.server(t), time.Hour)

	for _, name := range []string{"ops", "ci"} {
		if _, err := provider.Resolve(context.Background(), nodeClass(name)); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.views != 2 {
		t.Errorf("the API was viewed %d times for two different selectors, want 2", api.views)
	}
}

func TestTwoNodeClassesShareOneReading(t *testing.T) {
	api := &fakeAPI{}
	provider := NewProvider(api.server(t), time.Hour)

	second := nodeClass("ops")
	second.ObjectMeta = metav1.ObjectMeta{Name: "another"}
	for _, class := range []*v1alpha1.OxideNodeClass{nodeClass("ops"), second} {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.views != 1 {
		t.Errorf(
			"the API was viewed %d times for two nodeclasses selecting the same key, want 1",
			api.views,
		)
	}
}

func TestAKeyThatCannotBeViewedIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
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

	if _, err := NewProvider(
		client,
		time.Hour,
	).Resolve(context.Background(), nodeClass("ops")); err == nil {
		t.Error("Resolve() error = nil, want the failed lookup returned")
	}
}

func TestDriftReason(t *testing.T) {
	tests := []struct {
		name     string
		recorded *string
		desired  []v1alpha1.OxideSSHKey
		want     karpcloudprovider.DriftReason
	}{
		{
			name:     "a node carrying the selected keys has not drifted",
			recorded: new(sshKeyID),
			desired:  []v1alpha1.OxideSSHKey{{ID: sshKeyID}},
		},
		{

			name:     "a key added to the set replaces the nodes without it",
			recorded: new(sshKeyID),
			desired:  []v1alpha1.OxideSSHKey{{ID: sshKeyID}, {ID: sshKeyIDB}},
			want:     providers.DriftReasonSSHKey,
		},
		{
			name:     "and a key removed replaces the nodes that still carry it",
			recorded: new(sshKeyID + "," + sshKeyIDB),
			desired:  []v1alpha1.OxideSSHKey{{ID: sshKeyID}},
			want:     providers.DriftReasonSSHKey,
		},
		{

			name:     "a launch that recorded no keys and a nodeclass selecting none agree",
			recorded: new(""),
		},
		{
			name:    "a NodeClaim launched before the record existed cannot be compared",
			desired: []v1alpha1.OxideSSHKey{{ID: sshKeyID}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim := &karpv1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			}
			if tt.recorded != nil {
				nodeClaim.Annotations[v1alpha1.AnnotationSSHKeys] = *tt.recorded
			}
			class := &v1alpha1.OxideNodeClass{
				Status: v1alpha1.OxideNodeClassStatus{SSHKeys: tt.desired},
			}
			if got := (&Provider{}).DriftReason(nodeClaim, class); got != tt.want {
				t.Errorf("DriftReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

//go:fix inline
