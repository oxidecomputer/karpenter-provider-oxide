package image

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
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func TestDescribeSelectors(t *testing.T) {
	tests := []struct {
		name      string
		selectors []v1alpha1.ImageSelector
		want      string
	}{
		{
			name: "no selectors",
			want: "no image selectors are configured",
		},
		{
			name:      "one selector reports only the terms that were set",
			selectors: []v1alpha1.ImageSelector{{OS: "ubuntu", Version: "1.2.*", Project: "prod"}},
			want:      "no images matched {os=ubuntu,version=1.2.*,project=prod}",
		},
		{
			name: "selectors are OR'd, and read that way",
			selectors: []v1alpha1.ImageSelector{
				{Name: "rke2-*"},
				{ID: "444f4178-6bee-45f7-a654-c6ecafd4bb9e"},
			},
			want: "no images matched {name=rke2-*} or {id=444f4178-6bee-45f7-a654-c6ecafd4bb9e}",
		},
		{
			name:      "an empty selector still renders",
			selectors: []v1alpha1.ImageSelector{{}},
			want:      "no images matched {}",
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
	projectName    = "karpenter"
	projectID      = "11111111-0000-4000-8000-000000000001"
	otherProjectID = "11111111-0000-4000-8000-000000000002"
)

type fakeAPI struct {
	images []oxide.Image
	// lists counts list calls by the project they named, "" being the silo.
	lists map[string]int
	views int
	// viewQuery is the query string of the last view, to check what was sent alongside an ID.
	viewQuery string
	// listStatus, when set, is the status every list responds with instead of the images.
	listStatus int
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
		case r.Method == http.MethodGet && path == "images":
			if f.lists == nil {
				f.lists = map[string]int{}
			}
			f.lists[r.URL.Query().Get("project")]++
			if f.listStatus != 0 {
				writeJSON(t, w, f.listStatus, map[string]string{
					"error_code": "InternalError", "message": "the rack is on fire",
				})
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]any{"items": f.images})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "images/"):
			f.views++
			f.viewQuery = r.URL.RawQuery
			name := strings.TrimPrefix(path, "images/")
			for _, img := range f.images {
				if string(img.Name) == name || img.Id == name {
					writeJSON(t, w, http.StatusOK, img)
					return
				}
			}
			writeJSON(t, w, http.StatusNotFound, map[string]string{"error_code": "ObjectNotFound"})

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

func image(name, id string, createdMinutes int) oxide.Image {
	created := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC).
		Add(time.Duration(createdMinutes) * time.Minute)
	return oxide.Image{
		Id:          id,
		Name:        oxide.Name(name),
		Os:          "ubuntu",
		Version:     "24.04",
		TimeCreated: &created,
	}
}

func nodeClass(selectors ...v1alpha1.ImageSelector) *v1alpha1.OxideNodeClass {
	return &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{ImageSelector: selectors},
	}
}

func TestResolveReturnsTheMatchedImagesNewestFirst(t *testing.T) {
	api := &fakeAPI{images: []oxide.Image{
		image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0),
		image("rke2-ubuntu-2", "33333333-0000-4000-8000-000000000002", 10),
		image("other", "33333333-0000-4000-8000-000000000003", 20),
	}}
	provider := newProvider(t, api, time.Minute)

	got, err := provider.Resolve(
		context.Background(),
		nodeClass(v1alpha1.ImageSelector{Name: "rke2-*"}),
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 2 {
		t.Fatalf("Resolve() matched %d images, want 2", len(got.Resolved))
	}
	if got.Resolved[0].Name != "rke2-ubuntu-2" {
		t.Errorf("Resolve() first image = %q, want the newest", got.Resolved[0].Name)
	}
	if len(got.Unmatched) != 0 {
		t.Errorf("Resolve() unmatched = %v, want none", got.Unmatched)
	}
	if !got.Complete() {
		t.Error("Resolve() did not complete, want a completed resolution")
	}
}

func TestResolveReportsTheSelectorsWhenNothingMatched(t *testing.T) {
	api := &fakeAPI{}
	provider := newProvider(t, api, time.Minute)

	selector := v1alpha1.ImageSelector{Name: "rke2-*"}
	got, err := provider.Resolve(context.Background(), nodeClass(selector))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 0 {
		t.Errorf("Resolve() matched %d images, want none", len(got.Resolved))
	}
	if len(got.Unmatched) != 1 || got.Unmatched[0] != selector {
		t.Errorf("Resolve() unmatched = %v, want the one selector that missed", got.Unmatched)
	}
	if got.Complete() {
		t.Error("Resolve() completed, want an incomplete resolution")
	}
	if got.Reason != v1alpha1.ConditionReasonImagesNotFound {
		t.Errorf(
			"Resolve() reason = %q, want %q",
			got.Reason,
			v1alpha1.ConditionReasonImagesNotFound,
		)
	}
	if got.Message != DescribeSelectors([]v1alpha1.ImageSelector{selector}) {
		t.Errorf("Resolve() message = %q, want the selector description", got.Message)
	}
}

func TestAnExactSelectorStillFiltersOnItsOtherTerms(t *testing.T) {
	api := &fakeAPI{images: []oxide.Image{
		image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0),
	}}
	provider := newProvider(t, api, time.Minute)

	got, err := provider.Resolve(
		context.Background(),
		nodeClass(v1alpha1.ImageSelector{Name: "rke2-ubuntu-1", OS: "windows"}),
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 0 {
		t.Errorf("Resolve() matched %d images, want none: os does not match", len(got.Resolved))
	}
}

func TestTwoSelectorsNamingOneImageResolveItOnce(t *testing.T) {
	api := &fakeAPI{images: []oxide.Image{
		image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0),
	}}
	provider := newProvider(t, api, time.Minute)

	got, err := provider.Resolve(context.Background(), nodeClass(
		v1alpha1.ImageSelector{Name: "rke2-ubuntu-1"},
		v1alpha1.ImageSelector{ID: "33333333-0000-4000-8000-000000000001"},
	))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 1 {
		t.Errorf("Resolve() matched %d images, want 1 after dedupe", len(got.Resolved))
	}
}

func TestASelectorWithoutAProjectSearchesTheSiloAndTheControllerProject(t *testing.T) {
	api := &fakeAPI{images: []oxide.Image{
		image("rke2-silo", "33333333-0000-4000-8000-000000000001", 0),
	}}
	provider := newProvider(t, api, time.Minute)

	got, err := provider.Resolve(
		context.Background(),
		nodeClass(v1alpha1.ImageSelector{Name: "rke2-*"}),
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 1 {
		t.Fatalf("Resolve() matched %d images, want the silo image", len(got.Resolved))
	}
	if api.lists[""] != 1 || api.lists[projectName] != 1 {
		t.Errorf(
			"lists by project = %v, want one of the silo and one of %q", api.lists, projectName,
		)
	}
}

func TestASelectorNamingAProjectSearchesOnlyIt(t *testing.T) {
	api := &fakeAPI{}
	provider := newProvider(t, api, time.Minute)

	if _, err := provider.Resolve(
		context.Background(),
		nodeClass(v1alpha1.ImageSelector{Name: "rke2-*", Project: "prod"}),
	); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(api.lists) != 1 || api.lists["prod"] != 1 {
		t.Errorf("lists by project = %v, want only prod", api.lists)
	}
}

func TestAnIDSelectorSendsOnlyTheIDAndChecksTheProjectAfterwards(t *testing.T) {
	inProject := image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)
	inProject.ProjectId = projectID
	silo := image("rke2-silo", "33333333-0000-4000-8000-000000000002", 0)
	elsewhere := image("rke2-ubuntu-2", "33333333-0000-4000-8000-000000000003", 0)
	elsewhere.ProjectId = otherProjectID

	tests := []struct {
		name     string
		selector v1alpha1.ImageSelector
		want     int
		reason   string
	}{
		{
			name:     "an image in the controller's project needs no project written",
			selector: v1alpha1.ImageSelector{ID: selection.IDSelector(inProject.Id)},
			want:     1,
		},
		{
			name:     "a silo image is usable from any project",
			selector: v1alpha1.ImageSelector{ID: selection.IDSelector(silo.Id), Project: "prod"},
			want:     1,
		},
		{
			name: "an image in the project the selector named",
			selector: v1alpha1.ImageSelector{
				ID:      selection.IDSelector(inProject.Id),
				Project: projectID,
			},
			want: 1,
		},
		{
			name:     "an image outside the controller's project is a mismatch",
			selector: v1alpha1.ImageSelector{ID: selection.IDSelector(elsewhere.Id)},
			reason:   v1alpha1.ConditionReasonProjectMismatch,
		},
		{
			name: "an image outside the project the selector named is a mismatch",
			selector: v1alpha1.ImageSelector{
				ID:      selection.IDSelector(inProject.Id),
				Project: otherProjectID,
			},
			reason: v1alpha1.ConditionReasonProjectMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{images: []oxide.Image{inProject, silo, elsewhere}}
			provider := newProvider(t, api, time.Minute)

			got, err := provider.Resolve(context.Background(), nodeClass(tt.selector))
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if api.viewQuery != "" {
				t.Errorf("the view sent %q alongside the ID, want nothing", api.viewQuery)
			}
			if len(got.Resolved) != tt.want {
				t.Errorf("Resolve() matched %d images, want %d", len(got.Resolved), tt.want)
			}
			if got.Reason != tt.reason {
				t.Errorf("Resolve() reason = %q, want %q: %s", got.Reason, tt.reason, got.Message)
			}
			if tt.reason != "" && !strings.Contains(got.Message, string(tt.selector.ID)) {
				t.Errorf("Resolve() message = %q, want the image named", got.Message)
			}
		})
	}
}

func TestAMismatchedSelectorFailsTheResolutionEvenWhenAnotherMatched(t *testing.T) {
	elsewhere := image("rke2-ubuntu-2", "33333333-0000-4000-8000-000000000003", 0)
	elsewhere.ProjectId = otherProjectID
	api := &fakeAPI{images: []oxide.Image{
		image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0), elsewhere,
	}}
	provider := newProvider(t, api, time.Minute)

	got, err := provider.Resolve(context.Background(), nodeClass(
		v1alpha1.ImageSelector{Name: "rke2-ubuntu-1"},
		v1alpha1.ImageSelector{ID: selection.IDSelector(elsewhere.Id)},
	))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Complete() || got.Reason != v1alpha1.ConditionReasonProjectMismatch {
		t.Errorf("Resolve() reason = %q, want a project mismatch", got.Reason)
	}
}

func TestResolveReportsAFailedList(t *testing.T) {
	api := &fakeAPI{listStatus: http.StatusInternalServerError}
	provider := newProvider(t, api, time.Minute)

	_, err := provider.Resolve(
		context.Background(),
		nodeClass(v1alpha1.ImageSelector{Name: "rke2-*", Project: "prod"}),
	)
	if err == nil {
		t.Fatal("Resolve() error = nil, want the list failure")
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("Resolve() error = %v, want the project named", err)
	}
	if !strings.Contains(err.Error(), "the rack is on fire") {
		t.Errorf("Resolve() error = %v, want the cause preserved", err)
	}
}

func TestResolveServesACacheHit(t *testing.T) {
	api := &fakeAPI{
		images: []oxide.Image{image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)},
	}
	provider := newProvider(t, api, time.Hour)
	class := nodeClass(v1alpha1.ImageSelector{Name: "rke2-*"})

	for range 3 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists[projectName] != 1 {
		t.Errorf(
			"the project was listed %d times across three resolutions, want 1",
			api.lists[projectName],
		)
	}
}

func TestResolvePastTheTTLListsAgain(t *testing.T) {
	api := &fakeAPI{
		images: []oxide.Image{image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)},
	}

	provider := newProvider(t, api, 0)
	class := nodeClass(v1alpha1.ImageSelector{Name: "rke2-*"})

	for range 2 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists[projectName] != 2 {
		t.Errorf("the project was listed %d times, want one per resolution", api.lists[projectName])
	}
}

func TestAnEditedSelectorResolvesAgain(t *testing.T) {
	api := &fakeAPI{
		images: []oxide.Image{image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)},
	}
	provider := newProvider(t, api, time.Hour)

	for _, name := range []string{"rke2-*", "ubuntu-*"} {
		if _, err := provider.Resolve(
			context.Background(),
			nodeClass(v1alpha1.ImageSelector{Name: selection.WildcardSelector(name)}),
		); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists[projectName] != 2 {
		t.Errorf(
			"the project was listed %d times for two different selectors, want 2",
			api.lists[projectName],
		)
	}
}

func TestTwoNodeClassesShareOneReading(t *testing.T) {
	api := &fakeAPI{
		images: []oxide.Image{image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)},
	}
	provider := newProvider(t, api, time.Hour)

	first := nodeClass(v1alpha1.ImageSelector{Name: "rke2-*"}, v1alpha1.ImageSelector{OS: "ubuntu"})

	second := nodeClass(
		v1alpha1.ImageSelector{OS: "ubuntu"},
		v1alpha1.ImageSelector{Name: "rke2-*"},
	)
	second.Name = "another"
	for _, class := range []*v1alpha1.OxideNodeClass{first, second} {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists[projectName] != 1 {
		t.Errorf(
			"the project was listed %d times for two nodeclasses selecting the same images, want 1",
			api.lists[projectName],
		)
	}
}

func TestResolveDoesNotHandOutTheCachedSlice(t *testing.T) {
	api := &fakeAPI{
		images: []oxide.Image{image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)},
	}
	provider := newProvider(t, api, time.Hour)
	class := nodeClass(v1alpha1.ImageSelector{Name: "rke2-*"})

	first, err := provider.Resolve(context.Background(), class)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	first.Resolved[0].Name = "overwritten"

	second, err := provider.Resolve(context.Background(), class)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if second.Resolved[0].Name != "rke2-ubuntu-1" {
		t.Errorf("Resolve() = %q, want the cached reading to be untouched", second.Resolved[0].Name)
	}
}

func TestDriftReason(t *testing.T) {
	tests := []struct {
		name     string
		launched string
		desired  []v1alpha1.OxideImage
		want     karpcloudprovider.DriftReason
	}{
		{
			name:     "a node running the image the nodeclass would launch has not drifted",
			launched: "33333333-0000-4000-8000-000000000001",
			desired:  []v1alpha1.OxideImage{{ID: "33333333-0000-4000-8000-000000000001"}},
		},
		{
			name:     "a newer image winning the selector rolls the node onto it",
			launched: "33333333-0000-4000-8000-000000000001",
			desired:  []v1alpha1.OxideImage{{ID: "33333333-0000-4000-8000-000000000002"}},
			want:     providers.DriftReasonImage,
		},
		{
			name:     "a runner-up the node is not running is not drift",
			launched: "33333333-0000-4000-8000-000000000001",
			desired: []v1alpha1.OxideImage{
				{ID: "33333333-0000-4000-8000-000000000001"},
				{ID: "33333333-0000-4000-8000-000000000002"},
			},
		},
		{

			name:     "an empty resolved list is not drift",
			launched: "33333333-0000-4000-8000-000000000001",
		},
		{
			name:    "nor is a NodeClaim that never recorded an image",
			desired: []v1alpha1.OxideImage{{ID: "33333333-0000-4000-8000-000000000001"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim := &karpv1.NodeClaim{Status: karpv1.NodeClaimStatus{ImageID: tt.launched}}
			class := &v1alpha1.OxideNodeClass{
				Status: v1alpha1.OxideNodeClassStatus{Images: tt.desired},
			}
			if got := (&Provider{}).DriftReason(nodeClaim, class); got != tt.want {
				t.Errorf("DriftReason() = %q, want %q", got, tt.want)
			}
		})
	}
}
