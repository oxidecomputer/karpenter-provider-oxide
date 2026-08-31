package disk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oxidecomputer/oxide.go/oxide"
)

func TestWaitDetachedUsesTheFreshlyReadState(t *testing.T) {
	disk := oxide.Disk{
		Id:    "d15c0000-0000-4000-8000-000000000001",
		Name:  "boot",
		State: oxide.DiskState{Value: &oxide.DiskStateCreating{}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := disk
		current.State = oxide.DiskState{Value: &oxide.DiskStateDetached{}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(current)
	}))
	t.Cleanup(server.Close)
	ox, err := oxide.NewClient(oxide.WithHost(server.URL), oxide.WithToken("token"))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(ox, "project", WithPollInterval(time.Millisecond))

	current, err := p.WaitDetached(context.Background(), &disk)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.State() != oxide.DiskStateStateDetached {
		t.Fatalf("state = %s, want detached", current.State.State())
	}
}

func TestDeleteOwnedVerifiesBeforeDeleting(t *testing.T) {
	disk := oxide.Disk{
		Id:    "d15c0000-0000-4000-8000-000000000001",
		Name:  "boot",
		State: oxide.DiskState{Value: &oxide.DiskStateDetached{}},
	}
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(disk)
	}))
	t.Cleanup(server.Close)
	ox, err := oxide.NewClient(oxide.WithHost(server.URL), oxide.WithToken("token"))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(ox, "project")

	want := errors.New("not owned")
	err = p.DeleteOwned(context.Background(), disk.Id, func(*oxide.Disk) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want ownership error", err)
	}
	if deletes != 0 {
		t.Fatalf("delete requests = %d, want 0", deletes)
	}
}

func TestDeleteOwnedEventuallyRetriesTransientFailures(t *testing.T) {
	disk := oxide.Disk{
		Id:    "d15c0000-0000-4000-8000-000000000001",
		Name:  "boot",
		State: oxide.DiskState{Value: &oxide.DiskStateDetached{}},
	}
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			deletes++
			if deletes == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error_code": "Internal",
					"message":    "try again",
				})
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(disk)
	}))
	t.Cleanup(server.Close)
	ox, err := oxide.NewClient(oxide.WithHost(server.URL), oxide.WithToken("token"))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(
		ox,
		"project",
		WithPollInterval(time.Millisecond),
		WithDeleteTimeout(time.Second),
	)

	err = p.DeleteOwnedEventually(
		context.Background(),
		disk.Id,
		func(*oxide.Disk) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if deletes != 2 {
		t.Fatalf("delete requests = %d, want 2", deletes)
	}
}
