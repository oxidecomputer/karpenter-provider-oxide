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

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"
)

const giB = 1 << 30

var testOwner = ownership.Marker{
	ClusterUID:    types.UID("cluster-uid"),
	NodeClaimUID:  types.UID("nodeclaim-uid"),
	NodeClaimName: "node",
	Kind:          ownership.KindInstance,
}

func testOptions() Options { return Options{Owner: testOwner} }

func testDiskMarker(kind ownership.Kind) ownership.Marker {
	marker := testOwner
	marker.Kind = kind
	return marker
}

type fakeAPI struct {
	instance *oxide.Instance

	stopped  *oxide.Instance
	disks    map[string]*oxide.Disk
	listed   []oxide.Disk
	attached []oxide.Disk

	deleteStatus int

	stops       int
	deletes     int
	diskDeletes []string
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
			writeJSON(t, w, http.StatusOK, map[string]any{"items": f.attached})

		case r.Method == http.MethodGet && path == "disks":
			writeJSON(t, w, http.StatusOK, map[string]any{"items": f.listed})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "disks/"):
			ref := strings.TrimPrefix(path, "disks/")
			disk, ok := f.disks[ref]
			if !ok {
				notFound(t, w)
				return
			}
			writeJSON(t, w, http.StatusOK, disk)

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

		case r.Method == http.MethodDelete && strings.HasPrefix(path, "disks/"):
			ref := strings.TrimPrefix(path, "disks/")
			f.diskDeletes = append(f.diskDeletes, ref)
			w.WriteHeader(http.StatusNoContent)

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

type launchAPI struct {
	diskCreateError     string
	instanceCreateError string

	existing *oxide.Disk

	startError string

	disks           map[string]*oxide.Disk
	instance        *oxide.Instance
	diskCreates     int
	instanceCreates int
	starts          int
	diskDeletes     []string

	startRequested *bool

	attachedBootDisk string

	antiAffinityRequested []oxide.NameOrId

	affinityJoins   []string
	joinsAfterStart int

	affinityJoinError string

	stuckDisk bool

	createdRunState oxide.InstanceState

	instanceCreateTimeout bool

	cancelOnInstanceCreate context.CancelFunc
	cancelOnStart          context.CancelFunc
	cleanupContextCanceled bool
	stops                  int
	instanceDeletes        int
}

func (f *launchAPI) serveDisks(
	t *testing.T,
	w http.ResponseWriter,
	r *http.Request,
	path string,
) bool {
	t.Helper()
	if path != "disks" && !strings.HasPrefix(path, "disks/") {
		return false
	}
	switch {
	case r.Method == http.MethodPost && path == "disks":
		f.diskCreates++
		if f.diskCreateError != "" {
			apiError(t, w, http.StatusInsufficientStorage, f.diskCreateError)
			return true
		}
		var body oxide.DiskCreate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("unable to decode the disk create: %v", err)
		}
		name := string(body.Name)
		if _, taken := f.disks[name]; taken {
			apiError(t, w, http.StatusBadRequest, "ObjectAlreadyExists")
			return true
		}
		state := oxide.DiskState{Value: &oxide.DiskStateDetached{}}
		if f.stuckDisk {
			state = oxide.DiskState{Value: &oxide.DiskStateCreating{}}
		}
		disk := &oxide.Disk{
			Id:          fmt.Sprintf("d15c0000-0000-4000-8000-%012d", f.diskCreates),
			Name:        body.Name,
			Description: body.Description,
			Size:        body.Size,
			State:       state,
		}
		f.disks[name] = disk
		f.disks[disk.Id] = disk
		writeJSON(t, w, http.StatusCreated, disk)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "disks/"):
		f.cleanupContextCanceled = f.cleanupContextCanceled || r.Context().Err() != nil
		disk, ok := f.disks[strings.TrimPrefix(path, "disks/")]
		if !ok {
			notFound(t, w)
			return true
		}
		writeJSON(t, w, http.StatusOK, disk)

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "disks/"):
		f.cleanupContextCanceled = f.cleanupContextCanceled || r.Context().Err() != nil
		ref := strings.TrimPrefix(path, "disks/")
		f.diskDeletes = append(f.diskDeletes, ref)
		disk, ok := f.disks[ref]
		if !ok {
			notFound(t, w)
			return true
		}
		delete(f.disks, string(disk.Name))
		delete(f.disks, disk.Id)
		w.WriteHeader(http.StatusNoContent)
	default:
		return false
	}
	return true
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
		served := f.serveDisks(t, w, r, path) ||
			f.serveCreates(t, w, r, path) ||
			f.serveLifecycle(t, w, r, path)
		if !served {
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

func (f *launchAPI) serveCreates(
	t *testing.T, w http.ResponseWriter, r *http.Request, path string,
) bool {
	t.Helper()
	switch {
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
		f.antiAffinityRequested = body.AntiAffinityGroups
		if f.instanceCreateError != "" {
			if f.cancelOnInstanceCreate != nil {
				f.cancelOnInstanceCreate()
			}
			apiError(t, w, http.StatusInsufficientStorage, f.instanceCreateError)
			return true
		}
		if f.instanceCreateTimeout {
			hangUp(t, w)
			return true
		}

		f.instance = runningInstance()
		f.instance.Description = body.Description
		f.instance.RunState = oxide.InstanceStateStopped
		if f.createdRunState != "" {
			f.instance.RunState = f.createdRunState
		}
		writeJSON(t, w, http.StatusCreated, f.instance)

	case r.Method == http.MethodPost && strings.HasPrefix(path, "affinity-groups/"):
		group := strings.TrimPrefix(path, "affinity-groups/")
		group = group[:strings.Index(group, "/")]
		f.affinityJoins = append(f.affinityJoins, group)
		if f.starts > 0 {
			f.joinsAfterStart++
		}
		if f.affinityJoinError != "" {
			apiError(t, w, http.StatusBadRequest, f.affinityJoinError)
			return true
		}
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"type":  "instance",
			"value": map[string]string{"id": "instance", "name": "karpenter-node"},
		})

	default:
		return false
	}
	return true
}

func (f *launchAPI) serveLifecycle(
	t *testing.T, w http.ResponseWriter, r *http.Request, path string,
) bool {
	t.Helper()
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
		f.starts++
		if f.startError != "" {
			if f.cancelOnStart != nil {
				f.instance.RunState = oxide.InstanceStateRunning
				f.cancelOnStart()
			}
			apiError(t, w, http.StatusInsufficientStorage, f.startError)
			return true
		}
		f.instance.RunState = oxide.InstanceStateStarting
		writeJSON(t, w, http.StatusAccepted, f.instance)

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/stop"):
		f.cleanupContextCanceled = f.cleanupContextCanceled || r.Context().Err() != nil
		f.stops++
		f.instance.RunState = oxide.InstanceStateStopped
		writeJSON(t, w, http.StatusAccepted, f.instance)

	case r.Method == http.MethodGet && strings.HasSuffix(path, "/disks"):
		writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{}})

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "instances/"):
		f.cleanupContextCanceled = f.cleanupContextCanceled || r.Context().Err() != nil
		f.instanceDeletes++
		f.instance = nil
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "instances/"):
		if f.instance == nil {
			notFound(t, w)
			return true
		}
		writeJSON(t, w, http.StatusOK, f.instance)

	default:
		return false
	}
	return true
}

func hangUp(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal(
			"the test server does not support hijacking, which this fake needs to drop a request",
		)
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("unable to hijack the connection: %v", err)
	}
	_ = conn.Close()
}

func apiError(t *testing.T, w http.ResponseWriter, status int, code string) {
	t.Helper()
	writeJSON(t, w, status, map[string]string{"error_code": code, "message": code})
}

func launchParams() oxide.InstanceCreateParams {
	bootOwner := testOwner
	bootOwner.Kind = ownership.KindBootDisk
	return oxide.InstanceCreateParams{
		Body: &oxide.InstanceCreate{
			Name:        "karpenter-node",
			Hostname:    "karpenter-node",
			Description: testOwner.Description("node"),
			Ncpus:       8,
			Memory:      oxide.ByteCount(16 * giB),
			BootDisk: oxide.InstanceDiskAttachment{
				Value: &oxide.InstanceDiskAttachmentCreate{
					Name:        "karpenter-node",
					Description: bootOwner.Description("Boot disk"),
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

func TestCreateCreatesTheInstanceStoppedAndStartsItSeparately(t *testing.T) {
	api := &launchAPI{}
	p := NewProvider(api.server(t), "project", nil)

	instance, err := p.Create(context.Background(), launchParams(), testOptions())
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

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

func TestCreateReportsACapacityRefusalFromTheStart(t *testing.T) {
	api := &launchAPI{startError: "InsufficientCapacity"}
	p := NewProvider(api.server(t), "project", nil)

	_, err := p.Create(context.Background(), launchParams(), testOptions())
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("error is %v, want insufficient capacity", err)
	}

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

func TestCreateDoesNotWaitForTheInstanceToRun(t *testing.T) {
	api := &launchAPI{}
	p := NewProvider(api.server(t), "project", nil)

	instance, err := p.Create(context.Background(), launchParams(), testOptions())
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

func TestCreateProvisionsTheBootDiskBeforeTheInstance(t *testing.T) {
	api := &launchAPI{}
	p := NewProvider(api.server(t), "project", nil)

	if _, err := p.Create(context.Background(), launchParams(), testOptions()); err != nil {
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

func TestCreateReportsAStorageRefusalSeparately(t *testing.T) {
	api := &launchAPI{diskCreateError: "InsufficientCapacity"}
	p := NewProvider(api.server(t), "project", nil)

	_, err := p.Create(context.Background(), launchParams(), testOptions())
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

func TestCreateReportsACapacityRefusalAndRemovesTheBootDisk(t *testing.T) {
	api := &launchAPI{instanceCreateError: "InsufficientCapacity"}
	p := NewProvider(api.server(t), "project", nil)

	_, err := p.Create(context.Background(), launchParams(), testOptions())
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

func TestCreateLeavesAPoolOwnedBootDiskForItsControllerOnFailure(t *testing.T) {
	const name = "pooled-boot"
	api := &launchAPI{
		instanceCreateError: "InsufficientCapacity",
		existing: &oxide.Disk{
			Id:   "d15c0000-0000-4000-8000-0000000000aa",
			Name: name,
			Description: ownership.PoolMarker{
				ClusterUID:   "cluster-uid",
				BootDiskUID:  "pool-record",
				NodeClassUID: "nodeclass",
				ConfigHash:   "hash",
			}.Description(),
			State: oxide.DiskState{Value: &oxide.DiskStateDetached{}},
		},
	}
	p := NewProvider(api.server(t), "project", nil)
	params := launchParams()
	params.Body.BootDisk = oxide.InstanceDiskAttachment{
		Value: &oxide.InstanceDiskAttachmentAttach{Name: name},
	}

	_, err := p.Create(context.Background(), params, testOptions())
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("Create() error = %v, want insufficient capacity", err)
	}
	if api.diskCreates != 0 {
		t.Errorf("disk creates = %d, want none", api.diskCreates)
	}
	if len(api.diskDeletes) != 0 {
		t.Errorf("disk deletes = %v, want the pool disk left intact", api.diskDeletes)
	}
}

func TestCreateKeepsAClaimedBootDiskOnSuccess(t *testing.T) {
	const name = "pooled-boot"
	api := &launchAPI{existing: &oxide.Disk{
		Id:   "d15c0000-0000-4000-8000-0000000000aa",
		Name: name,
		Description: ownership.PoolMarker{
			ClusterUID:   "cluster-uid",
			BootDiskUID:  "pool-record",
			NodeClassUID: "nodeclass",
			ConfigHash:   "hash",
		}.Description(),
		State: oxide.DiskState{Value: &oxide.DiskStateDetached{}},
	}}
	p := NewProvider(api.server(t), "project", nil)
	params := launchParams()
	params.Body.BootDisk = oxide.InstanceDiskAttachment{
		Value: &oxide.InstanceDiskAttachmentAttach{Name: name},
	}

	if _, err := p.Create(
		context.Background(),
		params,
		testOptions(),
	); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if len(api.diskDeletes) != 0 {
		t.Errorf("disk deletes = %v, want none", api.diskDeletes)
	}
	if api.attachedBootDisk != name {
		t.Errorf("attached boot disk = %q, want %q", api.attachedBootDisk, name)
	}
}

func TestCreateAdoptsABootDiskLeftBehind(t *testing.T) {
	api := &launchAPI{
		existing: &oxide.Disk{
			Id:   "d15c0000-0000-4000-8000-0000000000ff",
			Name: "karpenter-node",
			Description: func() string {
				marker := testOwner
				marker.Kind = ownership.KindBootDisk
				return marker.Description("Boot disk")
			}(),
			Size:  oxide.ByteCount(50 * giB),
			State: oxide.DiskState{Value: &oxide.DiskStateDetached{}},
		},
	}
	p := NewProvider(api.server(t), "project", nil)

	if _, err := p.Create(context.Background(), launchParams(), testOptions()); err != nil {
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
		Id:          "e6bff1ff-24fb-49dc-a54e-c6a350cd4d6c",
		Name:        "karpenter-node",
		Hostname:    "karpenter-node",
		Ncpus:       8,
		Memory:      oxide.ByteCount(16 * giB),
		RunState:    oxide.InstanceStateRunning,
		Description: testOwner.Description("node"),
	}
}

func TestDeleteReportsTheCapacityItReleased(t *testing.T) {
	stopped := runningInstance()
	stopped.RunState = oxide.InstanceStateStopped
	api := &fakeAPI{instance: runningInstance(), stopped: stopped}

	var released []Released
	p := NewProvider(api.server(t), "project", func(_ context.Context, r Released) {
		released = append(released, r)
	})

	if err := p.Delete(
		context.Background(),
		"karpenter-node",
		Teardown{Owner: testOwner},
	); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	if len(released) != 1 {
		t.Fatalf("%d releases reported, want exactly 1", len(released))
	}
	if want := (Released{Ncpus: 8, Memory: 16 * giB}); released[0] != want {
		t.Errorf("released = %+v, want %+v", released[0], want)
	}
}

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

	if err := p.Delete(
		context.Background(),
		"karpenter-node",
		Teardown{Owner: testOwner},
	); err == nil {
		t.Fatal("delete succeeded despite the API refusing it")
	}
	if api.stops == 0 {
		t.Error("the instance was never asked to stop, so the test proves nothing")
	}
	if len(released) != 0 {
		t.Errorf("%d releases reported for an instance that is still there", len(released))
	}
}

func TestDeleteReportsNothingForAnInstanceAlreadyGone(t *testing.T) {
	api := &fakeAPI{}

	var released []Released
	p := NewProvider(api.server(t), "project", func(_ context.Context, r Released) {
		released = append(released, r)
	})

	err := p.Delete(
		context.Background(),
		"karpenter-node",
		Teardown{Owner: testOwner},
	)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete returned %v, want %v", err, ErrNotFound)
	}
	if len(released) != 0 {
		t.Errorf("%d releases reported for an instance that was already gone", len(released))
	}
}

func TestDeleteWorksWithoutAReleaseCallback(t *testing.T) {
	stopped := runningInstance()
	stopped.RunState = oxide.InstanceStateStopped
	api := &fakeAPI{instance: runningInstance(), stopped: stopped}

	p := NewProvider(api.server(t), "project", nil)

	if err := p.Delete(
		context.Background(),
		"karpenter-node",
		Teardown{Owner: testOwner},
	); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if api.deletes != 1 {
		t.Errorf("the instance was deleted %d times, want 1", api.deletes)
	}
}

func TestCreateJoinsAffinityGroupsBeforeTheStart(t *testing.T) {
	api := &launchAPI{}
	p := NewProvider(api.server(t), "project", nil)

	if _, err := p.Create(
		context.Background(),
		launchParams(),
		Options{Owner: testOwner, AffinityGroups: []string{"group-a", "group-b"}},
	); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if len(api.affinityJoins) != 2 {
		t.Fatalf("joined %v, want both groups", api.affinityJoins)
	}
	if api.joinsAfterStart != 0 {
		t.Errorf("%d joins landed after the start, want none", api.joinsAfterStart)
	}
	if api.starts != 1 {
		t.Errorf("starts = %d, want 1", api.starts)
	}
}

func TestCreateTreatsAnExistingAffinityMembershipAsSuccess(t *testing.T) {
	api := &launchAPI{affinityJoinError: "ObjectAlreadyExists"}
	p := NewProvider(api.server(t), "project", nil)

	if _, err := p.Create(
		context.Background(),
		launchParams(),
		Options{Owner: testOwner, AffinityGroups: []string{"group-a"}},
	); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if api.starts != 1 {
		t.Errorf("starts = %d, want the launch to have gone on to the start", api.starts)
	}
}

func TestCreateAbandonsTheInstanceWhenAnAffinityJoinFails(t *testing.T) {
	api := &launchAPI{affinityJoinError: "Internal"}
	p := NewProvider(api.server(t), "project", nil)

	if _, err := p.Create(
		context.Background(),
		launchParams(),
		Options{Owner: testOwner, AffinityGroups: []string{"group-a"}},
	); err == nil {
		t.Fatal("Create() error = nil, want the launch to have failed")
	}
	if api.starts != 0 {
		t.Errorf("starts = %d, want the launch to have stopped before the placement", api.starts)
	}
	if api.instance != nil {
		t.Error("the instance was left behind after a launch that failed")
	}
	if len(api.diskDeletes) == 0 {
		t.Error("the boot disk was left behind after a launch that failed")
	}
}

func TestCreatePassesAntiAffinityGroupsToTheCreate(t *testing.T) {
	api := &launchAPI{}
	p := NewProvider(api.server(t), "project", nil)

	params := launchParams()
	params.Body.AntiAffinityGroups = []oxide.NameOrId{"spread-a"}
	if _, err := p.Create(context.Background(), params, testOptions()); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if len(api.antiAffinityRequested) != 1 || api.antiAffinityRequested[0] != "spread-a" {
		t.Errorf("the create asked for %v, want [spread-a]", api.antiAffinityRequested)
	}
	if len(api.affinityJoins) != 0 {
		t.Errorf("joined %v by a separate call, want none", api.affinityJoins)
	}
}

func TestCreateRefusesForeignInstancesWithoutMutatingThem(t *testing.T) {
	for _, state := range []oxide.InstanceState{
		oxide.InstanceStateRunning,
		oxide.InstanceStateFailed,
	} {
		t.Run(string(state), func(t *testing.T) {
			api := &launchAPI{instanceCreateError: "ObjectAlreadyExists"}
			client := api.server(t)
			api.instance = runningInstance()
			api.instance.RunState = state
			foreign := testOwner
			foreign.ClusterUID = "other-cluster"
			api.instance.Description = foreign.Description("foreign")
			p := NewProvider(client, "project", nil)

			_, err := p.Create(context.Background(), launchParams(), testOptions())
			var conflict *ownership.ErrOwnershipConflict
			if !errors.As(err, &conflict) || conflict.Reason != ownership.ReasonForeignCluster {
				t.Fatalf("Create() error = %v, want a foreign-cluster ownership conflict", err)
			}
			if api.instance == nil {
				t.Fatal("the foreign instance was deleted")
			}
			if api.starts != 0 || len(api.affinityJoins) != 0 {
				t.Errorf(
					"foreign instance was mutated: starts=%d affinity joins=%v",
					api.starts,
					api.affinityJoins,
				)
			}
		})
	}
}

func TestCreateRefusesAForeignDiskNameCollision(t *testing.T) {
	foreign := testDiskMarker(ownership.KindBootDisk)
	foreign.NodeClaimUID = "other-claim"
	api := &launchAPI{existing: &oxide.Disk{
		Id:          "d15c0000-0000-4000-8000-0000000000ff",
		Name:        "karpenter-node",
		Description: foreign.Description("foreign"),
		State:       oxide.DiskState{Value: &oxide.DiskStateDetached{}},
	}}
	p := NewProvider(api.server(t), "project", nil)

	_, err := p.Create(context.Background(), launchParams(), testOptions())
	var conflict *ownership.ErrOwnershipConflict
	if !errors.As(err, &conflict) || conflict.Reason != ownership.ReasonWrongNodeClaim {
		t.Fatalf("Create() error = %v, want a wrong-nodeclaim ownership conflict", err)
	}
	if len(api.diskDeletes) != 0 {
		t.Errorf("foreign disk was deleted: %v", api.diskDeletes)
	}
	if api.instanceCreates != 0 {
		t.Errorf(
			"instance creates = %d, want launch stopped at the disk collision",
			api.instanceCreates,
		)
	}
}

func TestDeleteLeavesOperatorAttachedDisksIntact(t *testing.T) {
	owned := &oxide.Disk{
		Id:          "d15c0000-0000-4000-8000-000000000001",
		Name:        "owned",
		Description: testDiskMarker(ownership.KindBootDisk).Description("owned"),
		State: oxide.DiskState{
			Value: &oxide.DiskStateAttached{Instance: runningInstance().Id},
		},
	}
	operator := &oxide.Disk{
		Id:          "d15c0000-0000-4000-8000-000000000002",
		Name:        "operator",
		Description: "operator attached",
		State: oxide.DiskState{
			Value: &oxide.DiskStateAttached{Instance: runningInstance().Id},
		},
	}
	stopped := runningInstance()
	stopped.RunState = oxide.InstanceStateStopped
	api := &fakeAPI{
		instance: stopped,
		disks:    map[string]*oxide.Disk{owned.Id: owned, operator.Id: operator},
		attached: []oxide.Disk{*owned, *operator},
	}
	p := NewProvider(api.server(t), "project", nil)

	if err := p.Delete(context.Background(), stopped.Id, Teardown{Owner: testOwner}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if len(api.diskDeletes) != 1 || api.diskDeletes[0] != owned.Id {
		t.Errorf("disk deletes = %v, want only owned disk %s", api.diskDeletes, owned.Id)
	}
}

func TestGenericCleanupQuietlySkipsPoolDisks(t *testing.T) {
	pool := ownership.PoolMarker{
		ClusterUID:   "cluster-uid",
		BootDiskUID:  "record-uid",
		NodeClassUID: "class-uid",
		ConfigHash:   "hash",
	}
	counter := metrics.OwnershipRejections.WithLabelValues("cleanup", "disk", "unmarked")
	before := testutil.ToFloat64(counter)

	got := (&Provider{}).verifyDiskCandidates(context.Background(), []oxide.Disk{
		{
			Id:          "d15c0000-0000-4000-8000-000000000004",
			Name:        "kbd-pool",
			Description: pool.Description(),
		},
	}, testOwner)

	if len(got) != 0 {
		t.Errorf("verified disks = %#v, want a pool disk excluded from generic cleanup", got)
	}
	if after := testutil.ToFloat64(counter); after != before {
		t.Errorf(
			"ownership rejection counter changed from %v to %v for a valid pool disk",
			before,
			after,
		)
	}
}

func TestDeleteDoesNotTrustAPrefixWhenTheInstanceIsGone(t *testing.T) {
	foreign := &oxide.Disk{
		Id:          "d15c0000-0000-4000-8000-000000000003",
		Name:        "kd-launch-foreign",
		Description: "operator disk",
		State:       oxide.DiskState{Value: &oxide.DiskStateDetached{}},
	}
	api := &fakeAPI{
		disks:  map[string]*oxide.Disk{foreign.Id: foreign},
		listed: []oxide.Disk{*foreign},
	}
	p := NewProvider(api.server(t), "project", nil)

	err := p.Delete(
		context.Background(), runningInstance().Id,
		Teardown{Owner: testOwner, DiskPrefix: "kd-launch-"},
	)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() error = %v, want ErrNotFound", err)
	}
	if len(api.diskDeletes) != 0 {
		t.Errorf("prefix-matching foreign disk was deleted: %v", api.diskDeletes)
	}
}
