package nodeclass

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/image"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/snapshot"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/api/resource"
)

func storageNodeClass(bootSize string, sets ...v1alpha1.DiskSet) *v1alpha1.OxideNodeClass {
	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{Storage: v1alpha1.Storage{
			Boot:     v1alpha1.BootDisk{Size: resource.MustParse(bootSize)},
			DiskSets: sets,
		}},
		Status: v1alpha1.OxideNodeClassStatus{
			Images: []v1alpha1.OxideImage{{ID: "boot-image", SizeBytes: 10 << 30}},
		},
	}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesResolved)
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeSubnetsResolved)
	return nodeClass
}

func storageSet(size, diskType string) v1alpha1.DiskSet {
	return v1alpha1.DiskSet{
		Name:     "data",
		Type:     diskType,
		Count:    1,
		Size:     resource.MustParse(size),
		MountDir: "/mnt/data",
	}
}

func TestResolveStorageValidatesStaticSizes(t *testing.T) {
	for _, tt := range []struct {
		name, size, diskType string
		valid                bool
	}{
		{name: "zero", size: "0", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "below minimum", size: "512Mi", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "minimum", size: "1Gi", diskType: v1alpha1.DiskSetTypeDistributed, valid: true},
		{name: "fractional GiB", size: "1536Mi", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "whole GiB", size: "2Gi", diskType: v1alpha1.DiskSetTypeDistributed, valid: true},
		{name: "distributed maximum", size: "1023Gi", diskType: v1alpha1.DiskSetTypeDistributed, valid: true},
		{name: "above distributed maximum", size: "1024Gi", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "negative", size: "-1Gi", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "overflow", size: "1000000000000000000000000000000Gi", diskType: v1alpha1.DiskSetTypeDistributed},
		{name: "large local", size: "1024Gi", diskType: v1alpha1.DiskSetTypeLocal, valid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nodeClass := storageNodeClass("20Gi", storageSet(tt.size, tt.diskType))
			if err := (&Controller{}).resolveStorage(context.Background(), nodeClass); err != nil {
				t.Fatal(err)
			}
			condition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeStorageResolved)
			if got := condition.IsTrue(); got != tt.valid {
				t.Fatalf("StorageResolved true = %t, want %t: %#v", got, tt.valid, condition)
			}
			if !tt.valid && condition.Reason != v1alpha1.ConditionReasonInvalidDiskSize {
				t.Errorf(
					"reason = %q, want %q",
					condition.Reason,
					v1alpha1.ConditionReasonInvalidDiskSize,
				)
			}
		})
	}
}

func TestResolveStorageValidatesStaticBootSizes(t *testing.T) {
	for _, tt := range []struct {
		size  string
		valid bool
	}{
		{size: "0"},
		{size: "512Mi"},
		{size: "1Gi", valid: true},
		{size: "1536Mi"},
		{size: "2Gi", valid: true},
		{size: "1023Gi", valid: true},
		{size: "1024Gi"},
		{size: "-1Gi"},
		{size: "1000000000000000000000000000000Gi"},
	} {
		t.Run(tt.size, func(t *testing.T) {
			nodeClass := storageNodeClass(tt.size)
			nodeClass.Status.Images[0].SizeBytes = 1 << 30
			if err := (&Controller{}).resolveStorage(context.Background(), nodeClass); err != nil {
				t.Fatal(err)
			}
			condition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeStorageResolved)
			if got := condition.IsTrue(); got != tt.valid {
				t.Fatalf("StorageResolved true = %t, want %t: %#v", got, tt.valid, condition)
			}
		})
	}
}

func TestResolveStorageValidatesBootImageSize(t *testing.T) {
	for _, tt := range []struct {
		name      string
		requested string
		source    int64
		valid     bool
	}{
		{name: "smaller", requested: "1Gi", source: 2 << 30},
		{name: "equal", requested: "2Gi", source: 2 << 30, valid: true},
		{name: "larger", requested: "3Gi", source: 2 << 30, valid: true},
		{name: "unknown", requested: "2Gi", source: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nodeClass := storageNodeClass(tt.requested)
			nodeClass.Status.Images[0].SizeBytes = tt.source
			if err := (&Controller{}).resolveStorage(context.Background(), nodeClass); err != nil {
				t.Fatal(err)
			}
			condition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeStorageResolved)
			if got := condition.IsTrue(); got != tt.valid {
				t.Fatalf("StorageResolved true = %t, want %t: %#v", got, tt.valid, condition)
			}
		})
	}
}

func TestResolveStorageValidatesSourcedDiskSizes(t *testing.T) {
	for _, kind := range []string{"image", "snapshot"} {
		for _, readOnly := range []bool{false, true} {
			for _, requested := range []string{"1Gi", "2Gi", "3Gi"} {
				name := kind + "/" + map[bool]string{false: "read-write", true: "read-only"}[readOnly] + "/" + requested
				t.Run(name, func(t *testing.T) {
					set := storageSet(requested, v1alpha1.DiskSetTypeDistributed)
					set.ReadOnly = &readOnly
					if kind == "image" {
						set.ImageSelector = []v1alpha1.ImageSelector{{Name: "source"}}
					} else {
						set.SnapshotSelector = []v1alpha1.SnapshotSelector{{Name: "source"}}
					}
					nodeClass := storageNodeClass("20Gi", set)
					controller := sourceStorageController(t, 2<<30, true)
					if err := controller.resolveStorage(
						context.Background(),
						nodeClass,
					); err != nil {
						t.Fatal(err)
					}
					want := requested == "2Gi" || (!readOnly && requested == "3Gi")
					condition := nodeClass.StatusConditions().
						Get(v1alpha1.ConditionTypeStorageResolved)
					if got := condition.IsTrue(); got != want {
						t.Fatalf("StorageResolved true = %t, want %t: %#v", got, want, condition)
					}
				})
			}
		}
	}
}

func TestResolveStorageRejectsUnknownAndMissingSources(t *testing.T) {
	for _, tt := range []struct {
		name       string
		sourceSize int64
		found      bool
		reason     string
	}{
		{name: "unknown size", found: true, reason: v1alpha1.ConditionReasonDiskSourceSizeMismatch},
		{name: "missing", reason: v1alpha1.ConditionReasonDiskSourceNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			set := storageSet("2Gi", v1alpha1.DiskSetTypeDistributed)
			set.ImageSelector = []v1alpha1.ImageSelector{{Name: "source"}}
			nodeClass := storageNodeClass("20Gi", set)
			if err := sourceStorageController(t, tt.sourceSize, tt.found).
				resolveStorage(context.Background(), nodeClass); err != nil {
				t.Fatal(err)
			}
			condition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeStorageResolved)
			if !condition.IsFalse() || condition.Reason != tt.reason {
				t.Fatalf("condition = %#v, want False/%s", condition, tt.reason)
			}
		})
	}
}

func TestInvalidStorageClearsStaleLayoutAndMakesReadyFalse(t *testing.T) {
	nodeClass := storageNodeClass("20Gi", storageSet("512Mi", v1alpha1.DiskSetTypeDistributed))
	nodeClass.Status.Storage = &v1alpha1.StorageStatus{
		BootSizeBytes: 99,
		DiskSets:      []v1alpha1.DiskSetStatus{{Name: "stale"}},
	}

	if err := (&Controller{}).resolveStorage(context.Background(), nodeClass); err != nil {
		t.Fatal(err)
	}
	if nodeClass.Status.Storage != nil {
		t.Fatalf("stale resolved layout remains published: %#v", nodeClass.Status.Storage)
	}
	if ready := nodeClass.StatusConditions().Get(status.ConditionReady); !ready.IsFalse() {
		t.Fatalf("Ready = %s, want False for invalid storage", ready.Status)
	}
}

func sourceStorageController(t *testing.T, sourceSize int64, found bool) *Controller {
	t.Helper()
	created := time.Now()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		image := oxide.Image{
			Id: "image-id", Name: "source", Size: oxide.ByteCount(sourceSize),
			TimeCreated: &created,
		}
		switch {
		case r.Method == http.MethodGet && path == "images":
			items := []oxide.Image{image}
			if !found {
				items = nil
			}
			writeStorageJSON(t, w, http.StatusOK, map[string]any{"items": items})
			return
		case !found:
			writeStorageJSON(t, w, http.StatusNotFound, map[string]string{
				"error_code": "ObjectNotFound", "message": "not found",
			})
			return
		case r.Method == http.MethodGet && strings.HasPrefix(path, "images/"):
			writeStorageJSON(t, w, http.StatusOK, image)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "snapshots/"):
			writeStorageJSON(t, w, http.StatusOK, oxide.Snapshot{
				Id: "snapshot-id", Name: "source", Size: oxide.ByteCount(sourceSize),
				State: oxide.SnapshotStateReady, TimeCreated: &created,
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := oxide.NewClient(
		oxide.WithHost(server.URL),
		oxide.WithToken("token"),
		projecttest.WithGuard(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	projects := project.NewResolver(client, "project", 0)
	return &Controller{
		images:    image.NewProvider(client, projects, 0),
		snapshots: snapshot.NewProvider(client, projects, 0),
	}
}

func writeStorageJSON(t *testing.T, w http.ResponseWriter, code int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}
