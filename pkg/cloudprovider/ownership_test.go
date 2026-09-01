package cloudprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/types"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func ownershipProvider(t *testing.T, instances []oxide.Instance) *OxideCloudProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && path == "instances":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": instances})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "instances/"):
			id := strings.TrimPrefix(path, "instances/")
			for i := range instances {
				if instances[i].Id == id {
					_ = json.NewEncoder(w).Encode(instances[i])
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error_code": "ObjectNotFound", "message": "not found",
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := oxide.NewClient(oxide.WithHost(server.URL), oxide.WithToken("token"))
	if err != nil {
		t.Fatal(err)
	}
	return &OxideCloudProvider{
		ox: client, project: "project", clusterUID: "cluster-a",
		instanceTypeProvider: instancetype.NewProvider(nil, availability.New(nil)),
	}
}

func markedInstance(id string, marker ownership.Marker) oxide.Instance {
	return oxide.Instance{
		Id: id, Name: oxide.Name("karpenter-hostname-hint"), Hostname: "karpenter-wrong-name",
		Description: marker.Description("node"), Ncpus: 4, Memory: oxide.ByteCount(16 * GiB),
	}
}

func TestListUsesOnlyVerifiedInstanceMarkers(t *testing.T) {
	owned := ownership.Marker{
		ClusterUID:    "cluster-a",
		NodeClaimUID:  "claim-a",
		NodeClaimName: "marker-name",
		Kind:          ownership.KindInstance,
	}
	foreign := owned
	foreign.ClusterUID = "cluster-b"
	wrongKind := owned
	wrongKind.Kind = ownership.KindBootDisk
	provider := ownershipProvider(t, []oxide.Instance{
		markedInstance("11111111-0000-4000-8000-000000000001", owned),
		{
			Id:          "11111111-0000-4000-8000-000000000002",
			Name:        "karpenter-unmarked",
			Description: "unmarked",
		},
		{
			Id:          "11111111-0000-4000-8000-000000000003",
			Name:        "karpenter-malformed",
			Description: "karpenter.oxide.computer/owner/v1 cluster=cluster-a",
		},
		markedInstance("11111111-0000-4000-8000-000000000004", foreign),
		markedInstance("11111111-0000-4000-8000-000000000005", wrongKind),
	})

	claims, err := provider.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(claims) != 1 || claims[0].Name != owned.NodeClaimName {
		t.Fatalf("List() = %#v, want only NodeClaim %q", claims, owned.NodeClaimName)
	}
}

func TestGetRejectsUnmarkedAndForeignInstances(t *testing.T) {
	base := ownership.Marker{
		ClusterUID:    "cluster-a",
		NodeClaimUID:  "claim-a",
		NodeClaimName: "claim-a",
		Kind:          ownership.KindInstance,
	}
	foreign := base
	foreign.ClusterUID = "cluster-b"
	for _, instance := range []oxide.Instance{
		{Id: "11111111-0000-4000-8000-000000000001", Name: "karpenter-unmarked", Description: "unmarked"},
		markedInstance("11111111-0000-4000-8000-000000000002", foreign),
	} {
		t.Run(string(instance.Name), func(t *testing.T) {
			provider := ownershipProvider(t, []oxide.Instance{instance})
			_, err := provider.Get(context.Background(), "oxide://"+instance.Id)
			if !karpcloudprovider.IsNodeClaimNotFoundError(err) {
				t.Fatalf("Get() error = %v, want NodeClaimNotFoundError", err)
			}
		})
	}
}

func TestGetUsesTheMarkerNodeClaimName(t *testing.T) {
	marker := ownership.Marker{
		ClusterUID: types.UID(
			"cluster-a",
		),
		NodeClaimUID:  "claim-a",
		NodeClaimName: "marker-name",
		Kind:          ownership.KindInstance,
	}
	instance := markedInstance("11111111-0000-4000-8000-000000000001", marker)
	claim, err := ownershipProvider(t, []oxide.Instance{instance}).Get(
		context.Background(), "oxide://"+instance.Id,
	)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if claim.Name != marker.NodeClaimName {
		t.Errorf("NodeClaim name = %q, want marker name %q", claim.Name, marker.NodeClaimName)
	}
}
