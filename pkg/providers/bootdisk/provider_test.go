package bootdisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	diskprovider "github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/disk"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAcquireIsSingleUse(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{
		Name: "default", UID: types.UID("class-uid"),
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot: v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
			},
		},
		Status: v1alpha1.OxideNodeClassStatus{
			Images: []v1alpha1.OxideImage{{ID: "image-id"}},
		},
	}
	config, ok := ConfigFor(nodeClass)
	if !ok {
		t.Fatal("ConfigFor() did not resolve")
	}
	record := NewRecord(config, &v1alpha1.OxideBootDiskPool{
		Name: "default", UID: types.UID("pool-uid"),
	})
	record.UID = types.UID("record-uid")
	record.ResourceVersion = "1"
	record.CreationTimestamp = metav1.NewTime(time.Now())
	record.Status = v1alpha1.OxideBootDiskStatus{ID: "disk-id", State: v1alpha1.BootDiskStateReady}

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.OxideBootDisk{}).
		WithObjects(record).
		Build()

	disk := oxide.Disk{
		Id:    "disk-id",
		Name:  oxide.Name(record.Spec.DiskName),
		State: oxide.DiskState{Value: &oxide.DiskStateDetached{}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(disk)
	}))
	t.Cleanup(server.Close)
	ox, err := oxide.NewClient(
		oxide.WithHost(server.URL),
		oxide.WithToken("token"),
		projecttest.WithGuard(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	provider := NewProvider(
		kubeClient,
		kubeClient,
		diskprovider.NewProvider(ox, "project"),
		types.UID("cluster-uid"),
	)

	var group sync.WaitGroup
	results := make(chan *Acquired, 8)
	for i := range 8 {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			got, _ := provider.Acquire(
				context.Background(),
				nodeClass,
				"claim-"+string(rune('a'+i)),
				types.UID("uid-"+string(rune('a'+i))),
			)
			results <- got
		}(i)
	}
	group.Wait()
	close(results)

	claimed := 0
	for result := range results {
		if result != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("successful claims = %d, want 1", claimed)
	}

	stored := &v1alpha1.OxideBootDisk{}
	if err := kubeClient.Get(context.Background(), clientKey(record.Name), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.ClaimRef == nil {
		t.Fatal("the winning claim was not stored")
	}
	again, err := provider.Acquire(
		context.Background(),
		nodeClass,
		stored.Spec.ClaimRef.Name,
		stored.Spec.ClaimRef.UID,
	)
	if err != nil || again == nil || again.RecordName != record.Name {
		t.Fatalf("retry = %#v, %v, want the same disk", again, err)
	}
}

func clientKey(name string) client.ObjectKey {
	return client.ObjectKey{Name: name}
}

func TestUnclaimReturnsADetachedDiskToThePool(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   oxide.DiskState
		reusing bool
	}{
		{"detached", oxide.DiskState{Value: &oxide.DiskStateDetached{}}, true},
		{"attached", oxide.DiskState{Value: &oxide.DiskStateAttached{}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := &v1alpha1.OxideBootDisk{
				Name:            "pool-record",
				UID:             types.UID("record-uid"),
				ResourceVersion: "1",
				Spec: v1alpha1.OxideBootDiskSpec{
					DiskName: "pool-disk",
					ClaimRef: &v1alpha1.BootDiskOwnerRef{
						Name: "claim",
						UID:  types.UID("claim-uid"),
					},
					ClaimedAt: &metav1.Time{Time: time.Now()},
				},
				Status: v1alpha1.OxideBootDiskStatus{
					ID:    "disk-id",
					State: v1alpha1.BootDiskStateReady,
				},
			}
			scheme := runtime.NewScheme()
			if err := v1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			kubeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&v1alpha1.OxideBootDisk{}).
				WithObjects(record).
				Build()

			disk := oxide.Disk{Id: "disk-id", Name: "pool-disk", State: tc.state}
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(disk)
				},
			))
			t.Cleanup(server.Close)
			ox, err := oxide.NewClient(
				oxide.WithHost(server.URL),
				oxide.WithToken("token"),
				projecttest.WithGuard(t),
			)
			if err != nil {
				t.Fatal(err)
			}
			provider := NewProvider(
				kubeClient,
				kubeClient,
				diskprovider.NewProvider(ox, "project"),
				types.UID("cluster-uid"),
			)

			err = provider.Unclaim(context.Background(), record.Name, types.UID("claim-uid"))
			if err != nil {
				t.Fatal(err)
			}
			stored := &v1alpha1.OxideBootDisk{}
			if err := kubeClient.Get(
				context.Background(),
				clientKey(record.Name),
				stored,
			); err != nil {
				t.Fatal(err)
			}
			if tc.reusing {
				if stored.Spec.ClaimRef != nil || stored.Spec.ClaimedAt != nil {
					t.Fatal("a detached disk was not returned to the pool")
				}
				if stored.Spec.Retire {
					t.Fatal("a reusable disk was retired")
				}
				return
			}
			if !stored.Spec.Retire {
				t.Fatal("a disk that is no longer detached was not retired")
			}
		})
	}
}

func TestUnclaimLeavesAForeignClaimAlone(t *testing.T) {
	record := &v1alpha1.OxideBootDisk{
		Name: "pool-record", ResourceVersion: "1",
		Spec: v1alpha1.OxideBootDiskSpec{
			DiskName: "pool-disk",
			ClaimRef: &v1alpha1.BootDiskOwnerRef{Name: "other", UID: types.UID("other-uid")},
		},
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(record).Build()
	provider := NewProvider(
		kubeClient,
		kubeClient,
		diskprovider.NewProvider(nil, "project"),
		types.UID("cluster-uid"),
	)

	if err := provider.Unclaim(
		context.Background(),
		record.Name,
		types.UID("claim-uid"),
	); err != nil {
		t.Fatal(err)
	}
	stored := &v1alpha1.OxideBootDisk{}
	if err := kubeClient.Get(context.Background(), clientKey(record.Name), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Retire || stored.Spec.ClaimRef == nil {
		t.Fatal("unclaim disturbed a record claimed by another launch")
	}
}
