package reaper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	"github.com/oxidecomputer/oxide.go/oxide"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	clusterUID    = types.UID("cluster-1")
	liveUID       = types.UID("nodeclaim-live")
	deadUID       = types.UID("nodeclaim-dead")
	nodeClaimName = "node-1"
	instanceID    = "11111111-0000-4000-8000-000000000001"
	bootDiskID    = "22222222-0000-4000-8000-000000000002"
	quarantine    = 5 * time.Minute
	reaperPeriod  = time.Minute
)

type rackAPI struct {
	instances           []oxide.Instance
	disks               []oxide.Disk
	diskViewDescription string

	deletedInstances []string
	deletedDisks     []string
}

func (f *rackAPI) client(t *testing.T) *oxide.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case r.Method == http.MethodGet && path == "instances":
			writeJSON(t, w, http.StatusOK, map[string]any{"items": f.instances})

		case r.Method == http.MethodGet && path == "disks":
			writeJSON(t, w, http.StatusOK, map[string]any{"items": f.disks})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "disks/"):
			id := strings.TrimPrefix(path, "disks/")
			for i := range f.disks {
				if f.disks[i].Id == id {
					disk := f.disks[i]
					if f.diskViewDescription != "" {
						disk.Description = f.diskViewDescription
					}
					writeJSON(t, w, http.StatusOK, disk)
					return
				}
			}
			notFound(t, w)

		case r.Method == http.MethodGet && strings.HasSuffix(path, "/disks"):
			writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{}})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "instances/"):
			id := strings.TrimPrefix(path, "instances/")
			for i := range f.instances {
				if f.instances[i].Id == id {
					writeJSON(t, w, http.StatusOK, f.instances[i])
					return
				}
			}
			notFound(t, w)

		case r.Method == http.MethodDelete && strings.HasPrefix(path, "instances/"):
			id := strings.TrimPrefix(path, "instances/")
			f.deletedInstances = append(f.deletedInstances, id)
			remaining := f.instances[:0]
			for _, existing := range f.instances {
				if existing.Id != id {
					remaining = append(remaining, existing)
				}
			}
			f.instances = remaining
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodDelete && strings.HasPrefix(path, "disks/"):
			id := strings.TrimPrefix(path, "disks/")
			f.deletedDisks = append(f.deletedDisks, id)
			remaining := f.disks[:0]
			for _, existing := range f.disks {
				if existing.Id != id {
					remaining = append(remaining, existing)
				}
			}
			f.disks = remaining
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			notFound(t, w)
		}
	}))
	t.Cleanup(srv.Close)

	oxideClient, err := oxide.NewClient(oxide.WithHost(srv.URL), oxide.WithToken("token"))
	if err != nil {
		t.Fatalf("unable to build a client against the fake API: %v", err)
	}
	return oxideClient
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

func describe(cluster, nodeClaimUID types.UID, kind ownership.Kind) string {
	return ownership.Marker{
		ClusterUID:    cluster,
		NodeClaimUID:  nodeClaimUID,
		NodeClaimName: nodeClaimName,
		Kind:          kind,
	}.Description("")
}

func ownedInstance(description string) oxide.Instance {
	return oxide.Instance{
		Id:          instanceID,
		Name:        "karpenter-node-1",
		Description: description,

		RunState: oxide.InstanceStateStopped,
	}
}

func ownedDisk(description string) oxide.Disk {
	return oxide.Disk{
		Id:          bootDiskID,
		Name:        "karpenter-node-1",
		Description: description,
		State:       oxide.DiskState{Value: &oxide.DiskStateDetached{}},
	}
}

func nodeClaim(name string, uid types.UID) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid},
	}
}

func controllerFor(
	t *testing.T,
	api *rackAPI,
	cached []client.Object,
	authoritative []client.Object,
) *Controller {
	t.Helper()

	kubeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cached...).Build()
	apiReader := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(authoritative...).
		Build()

	oxideClient := api.client(t)
	return NewController(
		kubeClient,
		apiReader,
		oxideClient,
		"project",
		clusterUID,
		instance.NewProvider(oxideClient, "project", nil),
		reaperPeriod,
		quarantine,
	)
}

func elapse(c *Controller) {
	c.now = func() time.Time { return time.Now().Add(quarantine) }
}

func reap(t *testing.T, c *Controller) {
	t.Helper()
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

func TestReconcileCollectsAnOrphanOnceQuarantineHasPassed(t *testing.T) {
	api := &rackAPI{
		instances: []oxide.Instance{
			ownedInstance(describe(clusterUID, deadUID, ownership.KindInstance)),
		},
		disks: []oxide.Disk{
			ownedDisk(describe(clusterUID, deadUID, ownership.KindBootDisk)),
		},
	}
	c := controllerFor(t, api, nil, nil)

	reap(t, c)
	if len(api.deletedInstances) != 0 || len(api.deletedDisks) != 0 {
		t.Fatalf(
			"the first pass deleted instances %v and disks %v, want nothing deleted",
			api.deletedInstances, api.deletedDisks,
		)
	}

	elapse(c)
	reap(t, c)
	if len(api.deletedInstances) != 1 || api.deletedInstances[0] != instanceID {
		t.Errorf("deleted instances = %v, want the orphan removed", api.deletedInstances)
	}

	if len(api.deletedDisks) == 0 {
		t.Error("deleted disks = none, want the orphan's disk collected too")
	}
}

func TestReconcileLeavesResourcesOfALiveNodeClaimAlone(t *testing.T) {
	api := &rackAPI{
		instances: []oxide.Instance{
			ownedInstance(describe(clusterUID, liveUID, ownership.KindInstance)),
		},
	}
	claim := nodeClaim(nodeClaimName, liveUID)
	c := controllerFor(t, api, []client.Object{claim}, []client.Object{claim})

	reap(t, c)
	elapse(c)
	reap(t, c)

	if len(api.deletedInstances) != 0 {
		t.Errorf(
			"deleted instances = %v, want a live NodeClaim's instance left alone",
			api.deletedInstances,
		)
	}
}

func TestReconcileConfirmsAgainstTheApiserverBeforeDeleting(t *testing.T) {
	api := &rackAPI{
		instances: []oxide.Instance{
			ownedInstance(describe(clusterUID, liveUID, ownership.KindInstance)),
		},
	}
	claim := nodeClaim(nodeClaimName, liveUID)
	c := controllerFor(t, api, nil, []client.Object{claim})

	reap(t, c)
	elapse(c)
	reap(t, c)

	if len(api.deletedInstances) != 0 {
		t.Errorf(
			"deleted instances = %v, want nothing deleted while the apiserver still has the NodeClaim",
			api.deletedInstances,
		)
	}
}

func TestReconcileCollectsWhenTheNameWasReusedByADifferentNodeClaim(t *testing.T) {
	api := &rackAPI{
		instances: []oxide.Instance{
			ownedInstance(describe(clusterUID, deadUID, ownership.KindInstance)),
		},
	}

	claim := nodeClaim(nodeClaimName, liveUID)
	c := controllerFor(t, api, []client.Object{claim}, []client.Object{claim})

	reap(t, c)
	elapse(c)
	reap(t, c)

	if len(api.deletedInstances) != 1 {
		t.Errorf(
			"deleted instances = %v, want the resource of the NodeClaim that is gone removed",
			api.deletedInstances,
		)
	}
}

func TestReconcileIgnoresResourcesItCannotProveItOwns(t *testing.T) {
	for name, description := range map[string]string{
		"unmarked":      "a disk somebody created by hand",
		"other cluster": describe("cluster-2", deadUID, ownership.KindInstance),
		"malformed":     "karpenter.oxide.computer/owner/v1 cluster=cluster-1 nodeclaim=",
		"older marker":  "karpenter.oxide.computer/owner/v0 cluster=cluster-1 nodeclaim=x name=y kind=instance",
	} {
		t.Run(name, func(t *testing.T) {
			api := &rackAPI{
				instances: []oxide.Instance{ownedInstance(description)},
				disks:     []oxide.Disk{ownedDisk(description)},
			}
			c := controllerFor(t, api, nil, nil)

			reap(t, c)
			elapse(c)
			reap(t, c)

			if len(api.deletedInstances) != 0 || len(api.deletedDisks) != 0 {
				t.Errorf(
					"deleted instances %v and disks %v, want nothing this cluster cannot prove it owns touched",
					api.deletedInstances,
					api.deletedDisks,
				)
			}
		})
	}
}

func TestReconcileCollectsADetachedOrphanDisk(t *testing.T) {
	api := &rackAPI{
		disks: []oxide.Disk{
			ownedDisk(describe(clusterUID, deadUID, ownership.KindBootDisk)),
		},
	}
	c := controllerFor(t, api, nil, nil)

	reap(t, c)
	elapse(c)
	reap(t, c)

	if len(api.deletedDisks) != 1 || api.deletedDisks[0] != bootDiskID {
		t.Errorf("deleted disks = %v, want the leaked disk removed", api.deletedDisks)
	}
}

func TestReconcileLeavesAnOrphanTheAPIWillNotDelete(t *testing.T) {
	creating := ownedInstance(describe(clusterUID, deadUID, ownership.KindInstance))
	creating.RunState = oxide.InstanceStateCreating

	api := &rackAPI{instances: []oxide.Instance{creating}}
	c := controllerFor(t, api, nil, nil)

	reap(t, c)
	elapse(c)
	reap(t, c)

	if len(api.deletedInstances) != 0 {
		t.Errorf(
			"deleted instances = %v, want an instance in creating left for a later pass",
			api.deletedInstances,
		)
	}
}

func TestReconcileRevalidatesADiskImmediatelyBeforeDeletion(t *testing.T) {
	api := &rackAPI{
		disks: []oxide.Disk{
			ownedDisk(describe(clusterUID, deadUID, ownership.KindBootDisk)),
		},
		diskViewDescription: describe("cluster-2", deadUID, ownership.KindBootDisk),
	}
	c := controllerFor(t, api, nil, nil)

	reap(t, c)
	elapse(c)
	reap(t, c)

	if len(api.deletedDisks) != 0 {
		t.Errorf("deleted disks = %v, want a disk whose marker changed left intact", api.deletedDisks)
	}
}
