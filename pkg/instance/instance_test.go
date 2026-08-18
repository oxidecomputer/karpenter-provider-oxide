package instance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oxidecomputer/oxide.go/oxide"
)

const giB = 1 << 30

// fakeAPI is enough of the Oxide instance API to drive a teardown: a view, a stop, a delete, and the
// disk calls that follow. Every route can be overridden per test, and what it does not implement it
// answers 404 for, which is what a resource that is already gone looks like.
type fakeAPI struct {
	instance *oxide.Instance
	// stopped is what a view returns after the instance has been asked to stop. Nil means the view
	// keeps returning the original instance, which is how an instance that will not stop looks.
	stopped *oxide.Instance
	// deleteStatus is the status the instance delete answers with. Zero means 204.
	deleteStatus int

	stops   int
	deletes int
}

func (f *fakeAPI) server(t *testing.T) *oxide.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/stop"):
			f.stops++
			if f.stopped != nil {
				f.instance = f.stopped
			}
			writeJSON(t, w, http.StatusAccepted, f.instance)

		case r.Method == http.MethodGet && strings.HasSuffix(path, "/disks"):
			writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{}})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "instances/"):
			if f.instance == nil {
				notFound(t, w)
				return
			}
			writeJSON(t, w, http.StatusOK, f.instance)

		case r.Method == http.MethodDelete && strings.HasPrefix(path, "instances/"):
			f.deletes++
			if f.deleteStatus != 0 {
				writeJSON(t, w, f.deleteStatus, map[string]string{"error_code": "Internal"})
				return
			}
			f.instance = nil
			w.WriteHeader(http.StatusNoContent)

		// Disks are deleted by name here, and there are none to find.
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "disks/"):
			notFound(t, w)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			notFound(t, w)
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

func notFound(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeJSON(t, w, http.StatusNotFound, map[string]string{
		"error_code": "ObjectNotFound",
		"message":    "not found",
	})
}

func runningInstance() *oxide.Instance {
	return &oxide.Instance{
		Id:       "e6bff1ff-24fb-49dc-a54e-c6a350cd4d6c",
		Name:     "karpenter-node",
		Hostname: "karpenter-node",
		Ncpus:    8,
		Memory:   oxide.ByteCount(16 * giB),
		RunState: oxide.InstanceStateRunning,
	}
}

// The whole point of the release notification: once the rack says the instance is gone, its CPU and
// memory are back and whoever is tracking capacity gets told exactly how much.
func TestDeleteReportsTheCapacityItReleased(t *testing.T) {
	stopped := runningInstance()
	stopped.RunState = oxide.InstanceStateStopped
	api := &fakeAPI{instance: runningInstance(), stopped: stopped}

	var released []Released
	p := NewProvider(api.server(t), "project", func(_ context.Context, r Released) {
		released = append(released, r)
	})

	if err := p.Delete(context.Background(), "karpenter-node", "karpenter-node"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	if len(released) != 1 {
		t.Fatalf("%d releases reported, want exactly 1", len(released))
	}
	if want := (Released{Ncpus: 8, Memory: 16 * giB}); released[0] != want {
		t.Errorf("released = %+v, want %+v", released[0], want)
	}
}

// A delete that was asked for but not carried out frees nothing. Reporting it would have the caller
// reopen shapes against capacity the rack is still holding, which is the exact mistake the
// notification exists to avoid.
func TestDeleteReportsNothingUntilTheRackConfirms(t *testing.T) {
	stopped := runningInstance()
	stopped.RunState = oxide.InstanceStateStopped
	api := &fakeAPI{
		instance:     runningInstance(),
		stopped:      stopped,
		deleteStatus: http.StatusInternalServerError,
	}

	var released []Released
	p := NewProvider(api.server(t), "project", func(_ context.Context, r Released) {
		released = append(released, r)
	})

	if err := p.Delete(context.Background(), "karpenter-node", "karpenter-node"); err == nil {
		t.Fatal("delete succeeded despite the API refusing it")
	}
	if api.stops == 0 {
		t.Error("the instance was never asked to stop, so the test proves nothing")
	}
	if len(released) != 0 {
		t.Errorf("%d releases reported for an instance that is still there", len(released))
	}
}

// Karpenter retries a delete until it is told the instance is gone, and an instance that was already
// gone when this call started was released by whoever removed it. Reporting it again would reopen
// shapes on the strength of capacity that was freed long ago.
func TestDeleteReportsNothingForAnInstanceAlreadyGone(t *testing.T) {
	api := &fakeAPI{}

	var released []Released
	p := NewProvider(api.server(t), "project", func(_ context.Context, r Released) {
		released = append(released, r)
	})

	err := p.Delete(context.Background(), "karpenter-node", "karpenter-node")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete returned %v, want %v", err, ErrNotFound)
	}
	if len(released) != 0 {
		t.Errorf("%d releases reported for an instance that was already gone", len(released))
	}
}

// The provider has to work without anybody listening, since the notification is an optimization and
// not part of tearing an instance down.
func TestDeleteWorksWithoutAReleaseCallback(t *testing.T) {
	stopped := runningInstance()
	stopped.RunState = oxide.InstanceStateStopped
	api := &fakeAPI{instance: runningInstance(), stopped: stopped}

	p := NewProvider(api.server(t), "project", nil)

	if err := p.Delete(context.Background(), "karpenter-node", "karpenter-node"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if api.deletes != 1 {
		t.Errorf("the instance was deleted %d times, want 1", api.deletes)
	}
}
