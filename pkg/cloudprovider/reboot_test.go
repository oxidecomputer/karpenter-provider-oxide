package cloudprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/oxide.go/oxide"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const rebootInstanceID = "11111111-0000-4000-8000-000000000001"

func rebootProvider(t *testing.T, inst *oxide.Instance) *OxideCloudProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case inst == nil && strings.HasPrefix(path, "instances/"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error_code": "ObjectNotFound"})
		case r.Method == http.MethodGet && path == "instances/"+rebootInstanceID:
			_ = json.NewEncoder(w).Encode(inst)
		case r.Method == http.MethodPost && path == "instances/"+rebootInstanceID+"/reboot":
			inst.RunState = oxide.InstanceStateRebooting
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(inst)
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
	return &OxideCloudProvider{
		ox: client, project: "project",
		instanceProvider: instance.NewProvider(client, "project", nil),
	}
}

func rebootNodeClaim(providerID string) *karpcorev1.NodeClaim {
	nodeClaim := &karpcorev1.NodeClaim{}
	nodeClaim.Name = "default-abc12"
	nodeClaim.Status.ProviderID = providerID
	return nodeClaim
}

func TestRebootRestartsARunningInstance(t *testing.T) {
	inst := &oxide.Instance{Id: rebootInstanceID, RunState: oxide.InstanceStateRunning}
	c := rebootProvider(t, inst)

	if err := c.Reboot(
		context.Background(),
		rebootNodeClaim("oxide://"+rebootInstanceID),
		"op",
	); err != nil {
		t.Fatalf("reboot failed: %v", err)
	}
	if inst.RunState != oxide.InstanceStateRebooting {
		t.Errorf("instance is %s after the reboot, want rebooting", inst.RunState)
	}
}

func TestRebootReportsAMissingInstanceAsNotFound(t *testing.T) {
	c := rebootProvider(t, nil)

	err := c.Reboot(context.Background(), rebootNodeClaim("oxide://"+rebootInstanceID), "op")
	if !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("reboot returned %v, want a NodeClaimNotFoundError", err)
	}
}

func TestRebootFailsWithoutRetryWhenTheRackRefuses(t *testing.T) {
	inst := &oxide.Instance{Id: rebootInstanceID, RunState: oxide.InstanceStateFailed}
	c := rebootProvider(t, inst)

	err := c.Reboot(context.Background(), rebootNodeClaim("oxide://"+rebootInstanceID), "op")
	if !karpcloudprovider.IsNodeRebootFailedError(err) {
		t.Fatalf("reboot returned %v, want a NodeRebootFailedError", err)
	}
}

func TestRebootFailsWithoutRetryOnABadProviderID(t *testing.T) {
	c := rebootProvider(t, nil)

	err := c.Reboot(context.Background(), rebootNodeClaim("aws:///i-123"), "op")
	if !karpcloudprovider.IsNodeRebootFailedError(err) {
		t.Fatalf("reboot returned %v, want a NodeRebootFailedError", err)
	}
}
