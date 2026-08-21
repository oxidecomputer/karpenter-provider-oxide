package affinitygroup

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

func TestDescribeSelectors(t *testing.T) {
	tests := []struct {
		name      string
		selectors []v1alpha1.AffinityGroupSelector
		want      string
	}{
		{
			name:      "a name selector reports the name it looked for",
			selectors: []v1alpha1.AffinityGroupSelector{{Name: "rack-*"}},
			want:      "no groups matched {name=rack-*}",
		},
		{
			name: "selectors are OR'd, and read that way",
			selectors: []v1alpha1.AffinityGroupSelector{
				{ID: "444f4178-6bee-45f7-a654-c6ecafd4bb9e"},
				{Name: "control-plane-spread"},
			},
			want: "no groups matched {id=444f4178-6bee-45f7-a654-c6ecafd4bb9e} or {name=control-plane-spread}",
		},
		{
			name:      "an empty selector still renders",
			selectors: []v1alpha1.AffinityGroupSelector{{}},
			want:      "no groups matched {}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DescribeSelectors(tt.selectors); got != tt.want {
				t.Errorf("DescribeSelectors() = %q, want %q", got, tt.want)
			}
		})
	}
}

const (
	groupID  = "77777777-0000-4000-8000-000000000001"
	groupIDB = "77777777-0000-4000-8000-000000000002"
	project  = "prod"
)

// fakeAPI is both halves of the Oxide group API -- affinity and anti-affinity -- counting what was
// asked of it so that a cache hit can be asserted as the absence of a call.
type fakeAPI struct {
	// antiAffinity are the project's anti-affinity groups, which is the non-experimental half and
	// the one every case here uses.
	antiAffinity []oxide.AntiAffinityGroup
	lists        int
	views        int
}

func (f *fakeAPI) server(t *testing.T) *oxide.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case r.Method == http.MethodGet && path == "anti-affinity-groups":
			f.lists++
			writeJSON(t, w, http.StatusOK, map[string]any{"items": f.antiAffinity})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "anti-affinity-groups/"):
			f.views++
			name := strings.TrimPrefix(path, "anti-affinity-groups/")
			for _, group := range f.antiAffinity {
				if string(group.Name) == name || group.Id == name {
					writeJSON(t, w, http.StatusOK, group)
					return
				}
			}
			writeJSON(t, w, http.StatusNotFound, map[string]string{"error_code": "ObjectNotFound"})

		// Both kinds are resolved and swept, so the experimental affinity half is asked about too.
		// It has nothing in it in these cases.
		case r.Method == http.MethodGet && strings.Contains(path, "affinity-groups"):
			writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{}})

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

func fakeGroups() []oxide.AntiAffinityGroup {
	return []oxide.AntiAffinityGroup{
		{Id: groupID, Name: "rack-spread", Policy: oxide.AffinityPolicyAllow},
		{Id: groupIDB, Name: "rack-other", Policy: oxide.AffinityPolicyFail},
	}
}

// selecting is a nodeclass selecting anti-affinity groups by the given names, and nothing else. A
// wildcard is used so the resolution goes through the list rather than a direct view, which is the
// path that costs an API call worth caching.
func selecting(names ...string) *v1alpha1.OxideNodeClass {
	class := &v1alpha1.OxideNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: defaultNodeClass},
		Spec:       v1alpha1.OxideNodeClassSpec{Affinity: &v1alpha1.Affinity{}},
	}
	for _, name := range names {
		class.Spec.Affinity.AntiAffinityGroupSelector = append(
			class.Spec.Affinity.AntiAffinityGroupSelector,
			v1alpha1.AffinityGroupSelector{Name: name},
		)
	}
	return class
}

// A nodeclass configuring no affinity at all resolves to nothing without asking the API anything.
func TestResolveWithNoAffinityAsksNothing(t *testing.T) {
	api := &fakeAPI{antiAffinity: fakeGroups()}
	provider := NewProvider(api.server(t), project, time.Hour)

	got, err := provider.Resolve(context.Background(), &v1alpha1.OxideNodeClass{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 0 || api.lists != 0 || api.views != 0 {
		t.Errorf(
			"Resolve() = %+v after %d lists and %d views, want nothing asked",
			got,
			api.lists,
			api.views,
		)
	}
}

// Every group a wildcard matches is joined rather than one winner being picked, because an instance
// can belong to many groups.
func TestResolveMatchesEveryGroupAWildcardSelects(t *testing.T) {
	api := &fakeAPI{antiAffinity: fakeGroups()}
	provider := NewProvider(api.server(t), project, time.Hour)

	got, err := provider.Resolve(context.Background(), selecting("rack-*"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 2 {
		t.Fatalf("Resolve() matched %d groups, want both", len(got.Resolved))
	}
	if !got.Complete() {
		t.Errorf("Resolve() reason = %q, want a completed resolution", got.Reason)
	}
	// Sorted by ID so that a set that has not changed does not look like it has.
	if got.Resolved[0].ID != groupID || got.Resolved[1].ID != groupIDB {
		t.Errorf("Resolve() = %v, want the groups sorted by ID", got.Resolved)
	}
}

// A selector matching nothing is reported on the nodeclass rather than shrinking the desired set,
// since eviction is driven by that set and a group that is simply missing is an operator's to fix.
func TestASelectorThatMatchedNothingIsReportedRatherThanPublished(t *testing.T) {
	api := &fakeAPI{antiAffinity: fakeGroups()}
	provider := NewProvider(api.server(t), project, time.Hour)

	got, err := provider.Resolve(context.Background(), selecting("gone-*"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Complete() {
		t.Fatal("Resolve() completed, want the unmatched selector reported")
	}
	if got.Reason != v1alpha1.ConditionReasonAffinityGroupNotFound {
		t.Errorf(
			"Resolve() reason = %q, want %q",
			got.Reason,
			v1alpha1.ConditionReasonAffinityGroupNotFound,
		)
	}
	if got.Message != "no groups matched {name=gone-*}" {
		t.Errorf("Resolve() message = %q, want it to name the selector that missed", got.Message)
	}
	if len(got.Resolved) != 0 {
		t.Errorf(
			"Resolve() published %v, want nothing published for an incomplete resolution",
			got.Resolved,
		)
	}
}

func TestResolveServesACacheHit(t *testing.T) {
	api := &fakeAPI{antiAffinity: fakeGroups()}
	provider := NewProvider(api.server(t), project, time.Hour)
	class := selecting("rack-*")

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
	api := &fakeAPI{antiAffinity: fakeGroups()}
	provider := NewProvider(api.server(t), project, 0)
	class := selecting("rack-*")

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
	api := &fakeAPI{antiAffinity: fakeGroups()}
	provider := NewProvider(api.server(t), project, time.Hour)

	for _, name := range []string{"rack-*", "rack-s*"} {
		if _, err := provider.Resolve(context.Background(), selecting(name)); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 2 {
		t.Errorf("the API was listed %d times for two different selectors, want 2", api.lists)
	}
}

func TestTwoNodeClassesShareOneReading(t *testing.T) {
	api := &fakeAPI{antiAffinity: fakeGroups()}
	provider := NewProvider(api.server(t), project, time.Hour)

	second := selecting("rack-*")
	second.Name = "another"
	for _, class := range []*v1alpha1.OxideNodeClass{selecting("rack-*"), second} {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 1 {
		t.Errorf(
			"the API was listed %d times for two nodeclasses selecting the same groups, want 1",
			api.lists,
		)
	}
}

// The project is part of the question: the same name in two projects is two different groups, so a
// reading taken in one must not answer the other.
func TestTheProjectIsPartOfTheKey(t *testing.T) {
	api := &fakeAPI{antiAffinity: fakeGroups()}
	client := api.server(t)
	class := selecting("rack-*")

	for _, project := range []string{"prod", "staging"} {
		if _, err := NewProvider(
			client,
			project,
			time.Hour,
		).Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 2 {
		t.Errorf("the API was listed %d times across two projects, want 2", api.lists)
	}
}

func TestDriftReason(t *testing.T) {
	tests := []struct {
		name     string
		recorded []string
		desired  []v1alpha1.OxideAffinityGroup
		want     karpcloudprovider.DriftReason
	}{
		{
			name:     "a node in every desired group has not drifted",
			recorded: []string{groupID},
			desired:  []v1alpha1.OxideAffinityGroup{{ID: groupID}},
		},
		{
			// A group is joined at launch, where placement is decided, so joining one afterwards is
			// not something a running instance can do.
			name:     "a group added to the set replaces the nodes not in it",
			recorded: []string{groupID},
			desired:  []v1alpha1.OxideAffinityGroup{{ID: groupID}, {ID: groupIDB}},
			want:     providers.DriftReasonAffinityGroup,
		},
		{
			// Additions only: the affinity controller takes the instance out of a group that is no
			// longer wanted, in place.
			name:     "a group removed from the set drifts nothing",
			recorded: []string{groupID, groupIDB},
			desired:  []v1alpha1.OxideAffinityGroup{{ID: groupID}},
		},
		{
			// The empty set drifts exactly when something is desired, which is what makes a node
			// launched before the feature existed drift once groups are configured and not before.
			name:    "a node that recorded no groups drifts as soon as one is desired",
			desired: []v1alpha1.OxideAffinityGroup{{ID: groupID}},
			want:    providers.DriftReasonAffinityGroup,
		},
		{
			name: "and not before",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim := &karpv1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			}
			if tt.recorded != nil {
				refs := make([]v1alpha1.AffinityGroupRef, 0, len(tt.recorded))
				for _, id := range tt.recorded {
					refs = append(refs, v1alpha1.AffinityGroupRef{
						ID:   id,
						Type: v1alpha1.AffinityGroupTypeAntiAffinity,
					})
				}
				nodeClaim.Annotations[v1alpha1.AnnotationAffinityGroups] =
					v1alpha1.EncodeAffinityGroups(refs)
			}
			class := &v1alpha1.OxideNodeClass{
				Status: v1alpha1.OxideNodeClassStatus{AffinityGroups: tt.desired},
			}
			if got := (&Provider{}).DriftReason(nodeClaim, class); got != tt.want {
				t.Errorf("DriftReason() = %q, want %q", got, tt.want)
			}
		})
	}
}
