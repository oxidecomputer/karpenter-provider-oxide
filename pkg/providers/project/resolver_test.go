package project

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/oxide.go/oxide"
)

const projectID = "11111111-0000-4000-8000-000000000001"

func server(t *testing.T, views *int) *oxide.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		if r.Method != http.MethodGet || !strings.HasPrefix(path, "projects/") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		*views++
		w.Header().Set("Content-Type", "application/json")
		if strings.TrimPrefix(path, "projects/") != "prod" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error_code": "ObjectNotFound"})
			return
		}
		_ = json.NewEncoder(w).Encode(oxide.Project{Id: projectID, Name: "prod"})
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

func TestForDefaultsToTheControllerProject(t *testing.T) {
	r := NewResolver(nil, "prod", time.Hour)
	if got := r.For(""); got != "prod" {
		t.Errorf("For(\"\") = %q, want the default", got)
	}
	if got := r.For("other"); got != "other" {
		t.Errorf("For(other) = %q, want the selector's own project", got)
	}
}

func TestMatchesComparesAnIDWithoutCallingTheAPI(t *testing.T) {
	views := 0
	r := NewResolver(server(t, &views), "prod", time.Hour)
	ok, err := r.Matches(context.Background(), projectID, projectID)
	if err != nil || !ok {
		t.Fatalf("Matches(id, id) = %t, %v, want true", ok, err)
	}
	ok, err = r.Matches(context.Background(), projectID, "11111111-0000-4000-8000-000000000002")
	if err != nil || ok {
		t.Fatalf("Matches(id, other) = %t, %v, want false", ok, err)
	}
	if views != 0 {
		t.Errorf("the API was called %d times for ID comparisons, want 0", views)
	}
}

func TestMatchesResolvesANameOnce(t *testing.T) {
	views := 0
	r := NewResolver(server(t, &views), "prod", time.Hour)
	for range 3 {
		ok, err := r.Matches(context.Background(), "prod", projectID)
		if err != nil || !ok {
			t.Fatalf("Matches(prod, id) = %t, %v, want true", ok, err)
		}
	}
	ok, err := r.Matches(context.Background(), "prod", "11111111-0000-4000-8000-000000000002")
	if err != nil || ok {
		t.Fatalf("Matches(prod, other) = %t, %v, want false", ok, err)
	}
	if views != 1 {
		t.Errorf("the project was viewed %d times, want 1", views)
	}
}

func TestMatchesReportsAnUnknownProject(t *testing.T) {
	views := 0
	r := NewResolver(server(t, &views), "prod", time.Hour)
	if _, err := r.Matches(context.Background(), "missing", projectID); err == nil {
		t.Fatal("Matches(missing) error = nil, want the lookup failure")
	}
}
