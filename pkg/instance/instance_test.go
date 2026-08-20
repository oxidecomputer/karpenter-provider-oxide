package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// launchAPI is enough of the Oxide API to drive a launch: a boot disk create and view, an instance
// create, and the deletes a failed launch cleans up with. Each create can be made to fail with a
// given error code, which is how the two insufficient capacity refusals this package exists to tell
// apart are reproduced.
type launchAPI struct {
	// diskCreateError and instanceCreateError are Oxide error codes to refuse the respective create
	// with. Empty means the create succeeds.
	diskCreateError     string
	instanceCreateError string
	// existing is a disk already present under the boot disk name, which is what an earlier attempt
	// that died between creating the disk and creating the instance leaves behind.
	existing *oxide.Disk
	// startError is an Oxide error code to refuse the instance start with. Empty means it succeeds.
	// This is where a rack with no room for the shape refuses, since the start is what places the
	// instance.
	startError string

	disks           map[string]*oxide.Disk
	instance        *oxide.Instance
	diskCreates     int
	instanceCreates int
	starts          int
	diskDeletes     []string
	// startRequested is what the instance create's body asked for in its start field, or nil if it
	// left the field out and so accepted the API's default of starting the instance.
	startRequested *bool
	// attachedBootDisk is the name the instance create was asked to attach, and is empty if it was
	// asked to create the boot disk instead.
	attachedBootDisk string
}

func (f *launchAPI) server(t *testing.T) *oxide.Client {
	t.Helper()

	f.disks = map[string]*oxide.Disk{}
	if f.existing != nil {
		f.disks[string(f.existing.Name)] = f.existing
		f.disks[f.existing.Id] = f.existing
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case r.Method == http.MethodPost && path == "disks":
			f.diskCreates++
			if f.diskCreateError != "" {
				apiError(t, w, http.StatusInsufficientStorage, f.diskCreateError)
				return
			}
			var body oxide.DiskCreate
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("unable to decode the disk create: %v", err)
			}
			name := string(body.Name)
			if _, taken := f.disks[name]; taken {
				apiError(t, w, http.StatusBadRequest, "ObjectAlreadyExists")
				return
			}
			disk := &oxide.Disk{
				Id:    fmt.Sprintf("d15c0000-0000-4000-8000-%012d", f.diskCreates),
				Name:  body.Name,
				Size:  body.Size,
				State: oxide.DiskState{Value: &oxide.DiskStateDetached{}},
			}
			f.disks[name] = disk
			f.disks[disk.Id] = disk
			writeJSON(t, w, http.StatusCreated, disk)

		case r.Method == http.MethodGet && strings.HasPrefix(path, "disks/"):
			disk, ok := f.disks[strings.TrimPrefix(path, "disks/")]
			if !ok {
				notFound(t, w)
				return
			}
			writeJSON(t, w, http.StatusOK, disk)

		case r.Method == http.MethodDelete && strings.HasPrefix(path, "disks/"):
			ref := strings.TrimPrefix(path, "disks/")
			f.diskDeletes = append(f.diskDeletes, ref)
			disk, ok := f.disks[ref]
			if !ok {
				notFound(t, w)
				return
			}
			delete(f.disks, string(disk.Name))
			delete(f.disks, disk.Id)
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && path == "instances":
			f.instanceCreates++
			var body oxide.InstanceCreate
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("unable to decode the instance create: %v", err)
			}
			if attach, ok := body.BootDisk.Value.(*oxide.InstanceDiskAttachmentAttach); ok {
				f.attachedBootDisk = string(attach.Name)
			}
			f.startRequested = body.Start
			if f.instanceCreateError != "" {
				apiError(t, w, http.StatusInsufficientStorage, f.instanceCreateError)
				return
			}
			// Created stopped, which is what a create that is asked to start nothing answers with.
			f.instance = runningInstance()
			f.instance.RunState = oxide.InstanceStateStopped
			writeJSON(t, w, http.StatusCreated, f.instance)

		// The placement decision, and so the call a rack with no room refuses.
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
			f.starts++
			if f.startError != "" {
				apiError(t, w, http.StatusInsufficientStorage, f.startError)
				return
			}
			f.instance.RunState = oxide.InstanceStateStarting
			writeJSON(t, w, http.StatusAccepted, f.instance)

		// The teardown a launch that is giving up cleans itself up with. The instance is stopped by
		// the time this is reached, which is a state the delete is accepted in, so there is no stop
		// to serve.
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/disks"):
			writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{}})

		case r.Method == http.MethodDelete && strings.HasPrefix(path, "instances/"):
			f.instance = nil
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodGet && strings.HasPrefix(path, "instances/"):
			if f.instance == nil {
				notFound(t, w)
				return
			}
			writeJSON(t, w, http.StatusOK, f.instance)

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

func apiError(t *testing.T, w http.ResponseWriter, status int, code string) {
	t.Helper()
	writeJSON(t, w, status, map[string]string{"error_code": code, "message": code})
}

// launchParams is the create a caller hands in: a boot disk to be made along with the instance, which
// is the shape the provider splits apart.
func launchParams() oxide.InstanceCreateParams {
	return oxide.InstanceCreateParams{
		Body: &oxide.InstanceCreate{
			Name:        "karpenter-node",
			Hostname:    "karpenter-node",
			Description: "node",
			Ncpus:       8,
			Memory:      oxide.ByteCount(16 * giB),
			BootDisk: oxide.InstanceDiskAttachment{
				Value: &oxide.InstanceDiskAttachmentCreate{
					Name:        "karpenter-node",
					Description: "Boot disk",
					Size:        oxide.ByteCount(50 * giB),
					DiskBackend: oxide.DiskBackend{
						Value: &oxide.DiskBackendDistributed{
							DiskSource: oxide.DiskSource{
								Value: &oxide.DiskSourceImage{ImageId: "image"},
							},
						},
					},
				},
			},
		},
	}
}

// The instance is created stopped and started by a separate call, which is what makes placement a
// question the API answers before returning. A create that starts the instance does not wait for it
// to be placed, so the refusal a launch exists to catch would arrive after the call had already
// reported success, leaving it to be inferred from a run state that means two different things.
func TestCreateCreatesTheInstanceStoppedAndStartsItSeparately(t *testing.T) {
	api := &launchAPI{}
	p := NewProvider(api.server(t), "project", nil)

	instance, err := p.Create(context.Background(), launchParams())
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Explicitly false rather than absent: the API's default is to start the instance, so leaving
	// the field out is the same request this is here to avoid making.
	if api.startRequested == nil || *api.startRequested {
		t.Errorf("the create asked for start=%v, want an explicit false", api.startRequested)
	}
	if api.starts != 1 {
		t.Errorf("the instance was started %d times, want exactly once", api.starts)
	}
	if instance.RunState != oxide.InstanceStateStarting {
		t.Errorf("run state is %q, want starting", instance.RunState)
	}
}

// The start is where a rack with no room for the shape refuses, and it says so synchronously. The
// launch is over at that point, so nothing it created may be left behind: Karpenter reschedules an
// insufficient capacity failure onto a NodeClaim under a different name, which never comes back for
// this instance or its boot disk.
func TestCreateReportsACapacityRefusalFromTheStart(t *testing.T) {
	api := &launchAPI{startError: "InsufficientCapacity"}
	p := NewProvider(api.server(t), "project", nil)

	_, err := p.Create(context.Background(), launchParams())
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("error is %v, want insufficient capacity", err)
	}
	// A refusal to place says nothing about storage, and reporting both would have the caller take
	// a shape out of consideration for a limit it did not hit.
	if errors.Is(err, ErrInsufficientStorage) {
		t.Errorf("a refusal to place the instance was also reported as a storage refusal: %v", err)
	}
	if api.instance != nil {
		t.Error("the instance was left behind by a launch that was refused")
	}
	if len(api.diskDeletes) == 0 {
		t.Error("the boot disk was left behind by a launch that was refused")
	}
}

// Nothing waits for the guest to boot. Placement is what the launch establishes, and the start has
// established it by the time Create returns, so a launch costs three calls and no polling.
func TestCreateDoesNotWaitForTheInstanceToRun(t *testing.T) {
	api := &launchAPI{}
	p := NewProvider(api.server(t), "project", nil)

	instance, err := p.Create(context.Background(), launchParams())
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if instance.RunState == oxide.InstanceStateRunning {
		t.Fatal("the fake API never reports running, so this test proves nothing")
	}
	if api.instance.RunState != oxide.InstanceStateStarting {
		t.Errorf("the instance is in %q, want the launch to have left it starting",
			api.instance.RunState)
	}
}

// The split itself: the disk is made on its own and the instance create only attaches it. Without
// this the instance create is what allocates the disk's regions, and a storage refusal is
// indistinguishable from a sled that had no room.
func TestCreateProvisionsTheBootDiskBeforeTheInstance(t *testing.T) {
	api := &launchAPI{}
	p := NewProvider(api.server(t), "project", nil)

	if _, err := p.Create(context.Background(), launchParams()); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if api.diskCreates != 1 {
		t.Errorf("the boot disk was created %d times, want 1", api.diskCreates)
	}
	if api.attachedBootDisk != "karpenter-node" {
		t.Errorf(
			"the instance create attached boot disk %q, want it to attach karpenter-node by name",
			api.attachedBootDisk,
		)
	}
	if len(api.diskDeletes) != 0 {
		t.Errorf("a successful launch deleted disks %v", api.diskDeletes)
	}
}

// A rack with nowhere to put the disk's regions has to be reported as its own thing. Reporting it as
// insufficient capacity would have the caller mark the instance shape unavailable, which is untrue:
// Crucible places regions on three sleds picked independently of the instance, so no shape was ever
// the constraint.
func TestCreateReportsAStorageRefusalSeparately(t *testing.T) {
	api := &launchAPI{diskCreateError: "InsufficientCapacity"}
	p := NewProvider(api.server(t), "project", nil)

	_, err := p.Create(context.Background(), launchParams())
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("create returned %v, want it to wrap %v", err, ErrInsufficientStorage)
	}
	if errors.Is(err, ErrInsufficientCapacity) {
		t.Error(
			"a storage refusal also reported insufficient capacity, so callers cannot tell them apart",
		)
	}
	if api.instanceCreates != 0 {
		t.Errorf(
			"the instance was created %d times despite having no boot disk",
			api.instanceCreates,
		)
	}
}

// The other half of the same guarantee, and the reason the boot disk is cleaned up here: the shape
// could not be placed, so Karpenter reschedules onto a NodeClaim under a different name that will
// never come looking for this disk.
func TestCreateReportsACapacityRefusalAndRemovesTheBootDisk(t *testing.T) {
	api := &launchAPI{instanceCreateError: "InsufficientCapacity"}
	p := NewProvider(api.server(t), "project", nil)

	_, err := p.Create(context.Background(), launchParams())
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("create returned %v, want it to wrap %v", err, ErrInsufficientCapacity)
	}
	if errors.Is(err, ErrInsufficientStorage) {
		t.Error(
			"a capacity refusal also reported insufficient storage, so callers cannot tell them apart",
		)
	}
	if api.diskCreates != 1 {
		t.Fatalf("the boot disk was created %d times, want 1", api.diskCreates)
	}
	if len(api.diskDeletes) == 0 {
		t.Error("the boot disk was left behind by a launch nothing will retry under this name")
	}
}

// Splitting the create adds a window where the disk exists and the instance does not, which is
// exactly what a retried NodeClaim runs into. Adopting the disk is what keeps that from failing the
// launch, and from leaking a disk per attempt.
func TestCreateAdoptsABootDiskLeftBehind(t *testing.T) {
	api := &launchAPI{
		existing: &oxide.Disk{
			Id:    "d15c0000-0000-4000-8000-0000000000ff",
			Name:  "karpenter-node",
			Size:  oxide.ByteCount(50 * giB),
			State: oxide.DiskState{Value: &oxide.DiskStateDetached{}},
		},
	}
	p := NewProvider(api.server(t), "project", nil)

	if _, err := p.Create(context.Background(), launchParams()); err != nil {
		t.Fatalf("create failed with a boot disk left over from an earlier attempt: %v", err)
	}
	if len(api.diskDeletes) != 0 {
		t.Errorf("the adoptable boot disk was deleted and remade: %v", api.diskDeletes)
	}
	if api.attachedBootDisk != "karpenter-node" {
		t.Errorf("the instance create attached %q, want the adopted disk", api.attachedBootDisk)
	}
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
