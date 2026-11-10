package cloudprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/storagerefusal"
	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpoptions "sigs.k8s.io/karpenter/pkg/operator/options"
)

type launchBootstrapper struct{ request bootstrap.Request }

func (*launchBootstrapper) Name() string                         { return "test" }
func (*launchBootstrapper) Detect(context.Context) (bool, error) { return true, nil }

func (*launchBootstrapper) Resolve(
	context.Context,
	*v1alpha1.OxideNodeClass,
	bootstrap.Cluster,
) error {
	return nil
}

func (b *launchBootstrapper) UserData(
	_ context.Context,
	_ *v1alpha1.OxideNodeClass,
	request bootstrap.Request,
) (string, error) {
	b.request = request
	return "bootstrap-data", nil
}

func TestCreateUsesOnePlanForBootstrapResourcesAndLaunchRecord(t *testing.T) {
	const instanceID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const diskID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	var disk *oxide.Disk
	var created oxide.InstanceCreate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var response any
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/disks/"):
			if disk == nil {
				w.WriteHeader(http.StatusNotFound)
				response = map[string]string{"error_code": "ObjectNotFound"}
			} else {
				response = disk
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/disks":
			var body oxide.DiskCreate
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			disk = &oxide.Disk{
				Id:          diskID,
				Name:        body.Name,
				Description: body.Description,
				Size:        body.Size,
				State:       oxide.DiskState{Value: &oxide.DiskStateDetached{}},
			}
			w.WriteHeader(http.StatusCreated)
			response = disk
		case r.Method == http.MethodPost && r.URL.Path == "/v1/instances":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			response = &oxide.Instance{
				Id:          instanceID,
				Name:        created.Name,
				Description: created.Description,
				RunState:    oxide.InstanceStateStopped,
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/instances/"+instanceID+"/start":
			w.WriteHeader(http.StatusAccepted)
			response = &oxide.Instance{
				Id:          instanceID,
				Name:        created.Name,
				Description: created.Description,
				Ncpus:       created.Ncpus,
				Memory:      created.Memory,
				CpuPlatform: created.CpuPlatform,
				BootDiskId:  diskID,
				RunState:    oxide.InstanceStateStarting,
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
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
	nodeClass := nodeClassWithBootDisk("default", 40*GiB, 40*GiB)
	nodeClass.Status.Images = []v1alpha1.OxideImage{{ID: "image", SizeBytes: 10 * GiB}}
	nodeClass.Status.Subnets = []v1alpha1.OxideSubnet{
		{ID: "subnet", Name: "subnet", VPCName: "vpc"},
	}
	for _, condition := range []string{
		v1alpha1.ConditionTypeImagesResolved,
		v1alpha1.ConditionTypeSubnetsResolved,
		v1alpha1.ConditionTypeStorageResolved,
	} {
		nodeClass.StatusConditions().SetTrue(condition)
	}
	claim := &karpv1.NodeClaim{Name: "claim", UID: "claim-uid", Spec: karpv1.NodeClaimSpec{
		NodeClassRef: &karpv1.NodeClassReference{Name: nodeClass.Name},
		Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
			{
				Key:      corev1.LabelInstanceTypeStable,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"8-16", "4-8"},
			},
		},
		Taints: make([]corev1.Taint, 1, 4),
	}}
	claim.Spec.Taints[0] = corev1.Taint{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}
	before := claim.DeepCopy()
	manager := availability.New(nil)
	bootstrapper := &launchBootstrapper{}
	c := &OxideCloudProvider{
		kubeClient: fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(nodeClass).
			Build(),
		ox:                   ox,
		project:              "project",
		clusterUID:           "cluster-uid",
		instanceProvider:     instance.NewProvider(ox, "project", nil),
		instanceTypeProvider: instancetype.NewProvider(nil, manager),
		availability:         manager,
		storageRefusals:      storagerefusal.NewTracker(),
		bootstrappers:        bootstrap.New(bootstrapper),
	}
	ctx := karpoptions.ToContext(t.Context(), &karpoptions.Options{})
	got, err := c.Create(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if created.Ncpus != 4 || created.Memory != 8*GiB || created.Start == nil || *created.Start {
		t.Fatalf("wrong launch request: %+v", created)
	}
	if bootstrapper.request.Storage.Boot.Name != string(disk.Name) ||
		got.Annotations[v1alpha1.AnnotationBootDisk] != string(disk.Name) ||
		got.Annotations[v1alpha1.AnnotationDiskPrefix] != bootstrapper.request.Storage.Prefix {
		t.Fatal("bootstrap, resources, and record used different disk plans")
	}
	if got.Status.ProviderID != "oxide://"+instanceID || got.Status.ImageID != "image" ||
		got.Annotations[v1alpha1.AnnotationSubnet] != "subnet" ||
		got.Annotations[v1alpha1.AnnotationOxideNodeClassHash] != nodeClass.Hash() {
		t.Fatalf("launch record did not describe the plan: %+v", got)
	}
	if !reflect.DeepEqual(claim, before) {
		t.Fatal("Create mutated its input NodeClaim")
	}
	if len(bootstrapper.request.NodeTaints) != 2 {
		t.Fatalf("bootstrap taints = %v", bootstrapper.request.NodeTaints)
	}
	if extras := claim.Spec.Taints[:cap(claim.Spec.Taints)][1:]; extras[0].Key != "" {
		t.Fatal("Create mutated the input taints backing array")
	}
}
