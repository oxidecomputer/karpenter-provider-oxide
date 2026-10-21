package snapshot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/oxide.go/oxide"
)

const (
	projectName    = "karpenter"
	projectID      = "11111111-0000-4000-8000-000000000001"
	otherProjectID = "11111111-0000-4000-8000-000000000002"
	snapshotID     = "88888888-0000-4000-8000-000000000001"
)

type fakeAPI struct {
	snapshots []oxide.Snapshot
	// viewQuery is the query string of the last view.
	viewQuery string
}

func (f *fakeAPI) server(t *testing.T) *oxide.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "snapshots/"):
			f.viewQuery = r.URL.RawQuery
			ref := strings.TrimPrefix(path, "snapshots/")
			for _, snapshot := range f.snapshots {
				if snapshot.Id == ref || string(snapshot.Name) == ref {
					_ = json.NewEncoder(w).Encode(snapshot)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error_code": "ObjectNotFound"})
		case r.Method == http.MethodGet && path == "projects/"+projectName:
			_ = json.NewEncoder(w).Encode(oxide.Project{Id: projectID, Name: projectName})
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
		t.Fatal(err)
	}
	return client
}

func newProvider(t *testing.T, api *fakeAPI) *Provider {
	t.Helper()
	client := api.server(t)
	return NewProvider(client, project.NewResolver(client, projectName, time.Hour), time.Hour)
}

func ready(projectID string) oxide.Snapshot {
	created := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	return oxide.Snapshot{
		Id: snapshotID, Name: "base", ProjectId: projectID, State: oxide.SnapshotStateReady,
		Size: 1 << 30, TimeCreated: &created,
	}
}

func TestAnIDSelectorSendsOnlyTheIDAndChecksTheProjectAfterwards(t *testing.T) {
	tests := []struct {
		name     string
		snapshot oxide.Snapshot
		selector v1alpha1.SnapshotSelector
		want     int
		reason   string
	}{
		{
			name:     "a snapshot in the controller's project needs no project written",
			snapshot: ready(projectID),
			selector: v1alpha1.SnapshotSelector{ID: snapshotID},
			want:     1,
		},
		{
			name:     "a snapshot in the project the selector named",
			snapshot: ready(projectID),
			selector: v1alpha1.SnapshotSelector{ID: snapshotID, Project: projectName},
			want:     1,
		},
		{
			name:     "a snapshot outside the controller's project is a mismatch",
			snapshot: ready(otherProjectID),
			selector: v1alpha1.SnapshotSelector{ID: snapshotID},
			reason:   v1alpha1.ConditionReasonProjectMismatch,
		},
		{
			name:     "a snapshot outside the project the selector named is a mismatch",
			snapshot: ready(projectID),
			selector: v1alpha1.SnapshotSelector{ID: snapshotID, Project: otherProjectID},
			reason:   v1alpha1.ConditionReasonProjectMismatch,
		},
		{
			name:     "a snapshot whose name the selector did not mean is unmatched",
			snapshot: ready(projectID),
			selector: v1alpha1.SnapshotSelector{ID: snapshotID, Name: "other"},
			reason:   v1alpha1.ConditionReasonSnapshotsNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{snapshots: []oxide.Snapshot{tt.snapshot}}
			provider := newProvider(t, api)

			got, err := provider.Resolve(
				context.Background(),
				[]v1alpha1.SnapshotSelector{tt.selector},
			)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if api.viewQuery != "" {
				t.Errorf("the view sent %q alongside the ID, want nothing", api.viewQuery)
			}
			if len(got.Resolved) != tt.want {
				t.Errorf("Resolve() matched %d snapshots, want %d", len(got.Resolved), tt.want)
			}
			if got.Reason != tt.reason {
				t.Errorf("Resolve() reason = %q, want %q: %s", got.Reason, tt.reason, got.Message)
			}
		})
	}
}

func TestANameSelectorLooksInTheControllerProjectByDefault(t *testing.T) {
	api := &fakeAPI{snapshots: []oxide.Snapshot{ready(projectID)}}
	provider := newProvider(t, api)

	got, err := provider.Resolve(
		context.Background(),
		[]v1alpha1.SnapshotSelector{{Name: "base"}},
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(got.Resolved) != 1 {
		t.Fatalf("Resolve() matched %d snapshots, want 1", len(got.Resolved))
	}
	if api.viewQuery != "project="+projectName {
		t.Errorf("the view sent %q, want the controller's project", api.viewQuery)
	}
}
