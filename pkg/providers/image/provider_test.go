package image

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

// fakeAPI is the Oxide image API, serving one project's images and counting what was asked of it, so
// that a cache hit can be asserted as the absence of a call rather than as the presence of an answer.
type fakeAPI struct {
	images []oxide.Image
	lists  int
	views  int
}

func (f *fakeAPI) server(t *testing.T) *oxide.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case r.Method == http.MethodGet && path == "images":
			f.lists++
			writeJSON(t, w, http.StatusOK, map[string]any{"items": f.images})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "images/"):
			f.views++
			name := strings.TrimPrefix(path, "images/")
			for _, img := range f.images {
				if string(img.Name) == name || img.Id == name {
					writeJSON(t, w, http.StatusOK, img)
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

// image is one image in the fake project, created at the given offset from a fixed instant so that
// the ordering a resolution produces is a fact about the test rather than about when it ran.
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
	provider := NewProvider(api.server(t), time.Minute)

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

// A resolution that found nothing reports every selector, since with no images at all every one of
// them missed. That is what the ImagesResolved condition is rendered from.
func TestResolveReportsTheSelectorsWhenNothingMatched(t *testing.T) {
	api := &fakeAPI{}
	provider := NewProvider(api.server(t), time.Minute)

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
}

// Reconciles arriving inside the coalescing window cost one API call between them, which is the whole
// reason the provider exists rather than the reconcile calling the API directly.
func TestResolveServesACacheHit(t *testing.T) {
	api := &fakeAPI{
		images: []oxide.Image{image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)},
	}
	provider := NewProvider(api.server(t), time.Hour)
	class := nodeClass(v1alpha1.ImageSelector{Name: "rke2-*"})

	for range 3 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 1 {
		t.Errorf("the API was listed %d times across three resolutions, want 1", api.lists)
	}
}

// A reading past the coalescing window is made again, which is what lets a newly uploaded image
// matching a wildcard be picked up by the next resync rather than by an edit.
func TestResolvePastTheTTLListsAgain(t *testing.T) {
	api := &fakeAPI{
		images: []oxide.Image{image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)},
	}
	// A zero TTL makes every reading immediately due, which is the same code path as a reading that
	// has aged out without a test having to wait for it.
	provider := NewProvider(api.server(t), 0)
	class := nodeClass(v1alpha1.ImageSelector{Name: "rke2-*"})

	for range 2 {
		if _, err := provider.Resolve(context.Background(), class); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 2 {
		t.Errorf("the API was listed %d times, want one per resolution", api.lists)
	}
}

// An edited selector is a different question, so it gets its own reading. This is the whole of cache
// invalidation on a spec edit: nothing has to notice the edit.
func TestAnEditedSelectorResolvesAgain(t *testing.T) {
	api := &fakeAPI{
		images: []oxide.Image{image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)},
	}
	provider := NewProvider(api.server(t), time.Hour)

	for _, name := range []string{"rke2-*", "ubuntu-*"} {
		if _, err := provider.Resolve(
			context.Background(),
			nodeClass(v1alpha1.ImageSelector{Name: name}),
		); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
	}
	if api.lists != 2 {
		t.Errorf("the API was listed %d times for two different selectors, want 2", api.lists)
	}
}

// Two nodeclasses selecting the same images ask one question, so they share one reading rather than
// each listing the project.
func TestTwoNodeClassesShareOneReading(t *testing.T) {
	api := &fakeAPI{
		images: []oxide.Image{image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)},
	}
	provider := NewProvider(api.server(t), time.Hour)

	first := nodeClass(v1alpha1.ImageSelector{Name: "rke2-*"}, v1alpha1.ImageSelector{OS: "ubuntu"})
	// The same selectors, written the other way round: the key is the set of questions, not the order
	// they were typed in.
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
	if api.lists != 1 {
		t.Errorf(
			"the API was listed %d times for two nodeclasses selecting the same images, want 1",
			api.lists,
		)
	}
}

// The cache serves a copy, so a caller publishing a resolution into a nodeclass it then patches
// cannot reach into the reading the next caller will be served.
func TestResolveDoesNotHandOutTheCachedSlice(t *testing.T) {
	api := &fakeAPI{
		images: []oxide.Image{image("rke2-ubuntu-1", "33333333-0000-4000-8000-000000000001", 0)},
	}
	provider := NewProvider(api.server(t), time.Hour)
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
			// A resolution that found nothing is already reported as ImagesResolved being false.
			// Replacing the fleet over it would take every node down to launch replacements that
			// cannot launch.
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
