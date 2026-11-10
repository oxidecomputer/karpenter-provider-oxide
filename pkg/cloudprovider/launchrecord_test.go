package cloudprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpoptions "sigs.k8s.io/karpenter/pkg/operator/options"
)

func TestAdoptionRecordsObservedResourcesInsteadOfTheNewPlan(t *testing.T) {
	const instanceID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const bootID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	const dataID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	claim := &karpv1.NodeClaim{Name: "claim", UID: "claim-uid", Labels: map[string]string{
		corev1.LabelInstanceTypeStable: "16-32", v1alpha1.LabelInstanceCPUPlatform: "amd_turin",
	}, Annotations: map[string]string{v1alpha1.AnnotationDiskPrefix: "untrusted-new-prefix", "custom": "retained"}}
	before := claim.DeepCopy()
	c := &OxideCloudProvider{clusterUID: "cluster-uid", project: "project",
		instanceTypeProvider: instancetype.NewProvider(nil, availability.New(nil))}
	owner := c.owner(claim)
	bootOwner, dataOwner := owner, owner
	bootOwner.Kind, dataOwner.Kind = ownership.KindBootDisk, ownership.KindDataDisk
	detached := oxide.DiskState{Value: &oxide.DiskStateDetached{}}
	responses := map[string]any{
		"/v1/instances/" + instanceID + "/disks": []oxide.Disk{
			{
				Id:          bootID,
				Name:        "actual-boot",
				Description: bootOwner.Description(""),
				ImageId:     "original-image",
				Size:        40 * GiB,
				State:       detached,
			},
			{
				Id:          dataID,
				Name:        "actual-data",
				Description: dataOwner.Description(""),
				State:       detached,
			},
			{Id: "foreign", Name: "foreign", State: detached},
		},
		"/v1/network-interfaces": []oxide.InstanceNetworkInterface{
			{SubnetId: "original-subnet", Primary: new(true)},
		},
		"/v1/instances/" + instanceID + "/ssh-public-keys": []oxide.SshKey{
			{Id: "original-key"},
		},
		"/v1/instances/" + instanceID + "/external-ips": []oxide.ExternalIp{},
		"/v1/instances/" + instanceID + "/affinity-groups": []oxide.AffinityGroup{
			{Id: "original-group"},
		},
		"/v1/instances/" + instanceID + "/anti-affinity-groups": []oxide.AntiAffinityGroup{},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		items, ok := responses[r.URL.Path]
		if !ok || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"items": items}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	ox, err := oxide.NewClient(
		oxide.WithHost(server.URL),
		oxide.WithToken("test"),
		projecttest.WithGuard(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	c.ox = ox
	nodeClass := nodeClassWithBootDisk("current", 80*GiB, 80*GiB)
	nodeClass.Status.Images = []v1alpha1.OxideImage{{ID: "new-image"}}
	nodeClass.Status.Subnets = []v1alpha1.OxideSubnet{{ID: "new-subnet"}}
	nodeClass.Status.SSHKeys = []v1alpha1.OxideSSHKey{{ID: "new-key"}}
	ctx := karpoptions.ToContext(t.Context(), &karpoptions.Options{})
	got, err := c.recordLaunch(
		ctx,
		claim,
		nodeClass,
		&launchPlan{},
		launchBootDisk{name: "new-boot"},
		&instance.LaunchResult{
			Adopted: true,
			Instance: &oxide.Instance{
				Id:          instanceID,
				BootDiskId:  bootID,
				Ncpus:       4,
				Memory:      8 * GiB,
				CpuPlatform: "amd_milan",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		v1alpha1.AnnotationBootDisk: "actual-boot", v1alpha1.AnnotationSubnet: "original-subnet",
		v1alpha1.AnnotationSSHKeys:        "original-key",
		v1alpha1.AnnotationAffinityGroups: "af:original-group",
		"custom":                          "retained",
	} {
		if got.Annotations[key] != want {
			t.Errorf("%s = %q, want %q", key, got.Annotations[key], want)
		}
	}
	for _, key := range []string{
		v1alpha1.AnnotationDiskPrefix,
		v1alpha1.AnnotationOxideNodeClassHash,
		v1alpha1.AnnotationDiskSetSources,
	} {
		if value, exists := got.Annotations[key]; exists {
			t.Errorf("invented launch intent %s=%q", key, value)
		}
	}
	ids := v1alpha1.ParseIDs(got.Annotations[v1alpha1.AnnotationDiskIDs])
	if ids.Len() != 2 || !ids.HasAll(bootID, dataID) {
		t.Errorf("owned disk IDs = %v", ids)
	}
	if got.Status.ImageID != "original-image" ||
		got.Labels[corev1.LabelInstanceTypeStable] != "4-8" ||
		got.Labels[v1alpha1.LabelInstanceCPUPlatform] != "amd_milan" {
		t.Errorf("wrong adopted identity: %+v", got)
	}
	wantStorage, _ := instancetype.EphemeralStorageFor(40*GiB, instancetype.Reserved{})
	capacity := got.Status.Capacity[corev1.ResourceEphemeralStorage]
	if capacity.Value() != wantStorage.Capacity {
		t.Errorf("storage = %d, want %d", capacity.Value(), wantStorage.Capacity)
	}
	if !reflect.DeepEqual(claim, before) {
		t.Fatal("recording mutated the input NodeClaim")
	}
	teardown := teardownFor(got, "karpenter-claim", owner)
	if teardown.DiskPrefix != "" || len(teardown.DiskIDs) != 2 {
		t.Errorf("unsafe adopted teardown: %+v", teardown)
	}
}
