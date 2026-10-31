package cloudprovider

import (
	"context"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func nodeClassWithBootDisk(name string, spec, status int64) *v1alpha1.OxideNodeClass {
	nodeClass := &v1alpha1.OxideNodeClass{
		Name: name,
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot: v1alpha1.BootDisk{Size: *resource.NewQuantity(spec, resource.BinarySI)},
			},
		},
	}
	if status > 0 {
		nodeClass.Status.Storage = &v1alpha1.StorageStatus{BootSizeBytes: status}
	}
	return nodeClass
}

func nodePoolFor(nodeClassName string) *karpv1.NodePool {
	return &karpv1.NodePool{
		Name: "default",
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{
				Spec: karpv1.NodeClaimTemplateSpec{
					NodeClassRef: &karpv1.NodeClassReference{Name: nodeClassName},
				},
			},
		},
	}
}

func TestGetInstanceTypesAdvertisesEphemeralStorage(t *testing.T) {
	const bootSizeBytes = 80 << 30
	nodeClass := nodeClassWithBootDisk("default", 40<<30, bootSizeBytes)
	c := &OxideCloudProvider{
		kubeClient: fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(nodeClass).
			Build(),
		instanceTypeProvider: instancetype.NewProvider(nil, availability.New(nil)),
	}

	instanceTypes, err := c.GetInstanceTypes(context.Background(), nodePoolFor("default"))
	if err != nil {
		t.Fatalf("GetInstanceTypes() error = %v", err)
	}
	if len(instanceTypes) == 0 {
		t.Fatal("no instance types offered")
	}

	want, ok := instancetype.EphemeralStorageFor(bootSizeBytes, instancetype.Reserved{})
	if !ok {
		t.Fatal("an 80GiB boot disk should carry ephemeral storage")
	}
	for _, it := range instanceTypes {
		capacity, advertised := it.Capacity[corev1.ResourceEphemeralStorage]
		if !advertised {
			t.Fatalf("instance type %s advertises no ephemeral storage", it.Name)
		}
		// The resolved status size wins over the spec's.
		if capacity.Value() != want.Capacity {
			t.Fatalf(
				"instance type %s ephemeral storage = %d, want %d",
				it.Name,
				capacity.Value(),
				want.Capacity,
			)
		}
	}
}

func TestGetInstanceTypesFallsBackToTheSpecBootDisk(t *testing.T) {
	const bootSizeBytes = 40 << 30
	nodeClass := nodeClassWithBootDisk("unresolved", bootSizeBytes, 0)
	c := &OxideCloudProvider{
		kubeClient: fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(nodeClass).
			Build(),
		instanceTypeProvider: instancetype.NewProvider(nil, availability.New(nil)),
	}

	instanceTypes, err := c.GetInstanceTypes(context.Background(), nodePoolFor("unresolved"))
	if err != nil {
		t.Fatalf("GetInstanceTypes() error = %v", err)
	}
	want, _ := instancetype.EphemeralStorageFor(bootSizeBytes, instancetype.Reserved{})
	capacity := instanceTypes[0].Capacity[corev1.ResourceEphemeralStorage]
	if capacity.Value() != want.Capacity {
		t.Fatalf("ephemeral storage = %d, want %d", capacity.Value(), want.Capacity)
	}
}

func TestGetInstanceTypesWithoutANodeClass(t *testing.T) {
	c := &OxideCloudProvider{
		kubeClient:           fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		instanceTypeProvider: instancetype.NewProvider(nil, availability.New(nil)),
	}

	if _, err := c.GetInstanceTypes(
		context.Background(), nodePoolFor("missing"),
	); err == nil {
		t.Fatal("GetInstanceTypes() with a missing nodeclass should error")
	}

	instanceTypes, err := c.GetInstanceTypes(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetInstanceTypes(nil) error = %v", err)
	}
	if _, advertised := instanceTypes[0].Capacity[corev1.ResourceEphemeralStorage]; advertised {
		t.Fatal("without a nodeclass there is no boot disk to advertise")
	}
}

// system-reserved only reaches kubelet unless the overhead model carries it
// too; otherwise the scheduler plans against allocatable the node never has.
func TestGetInstanceTypesModelsTheNodeClassKubeletReservations(t *testing.T) {
	plain := nodeClassWithBootDisk("plain", 20<<30, 80<<30)
	reserved := nodeClassWithBootDisk("reserved", 20<<30, 80<<30)
	reserved.Spec.Kubelet = &v1alpha1.KubeletConfiguration{
		SystemReserved: map[string]v1alpha1.KubeletValue{"cpu": "500m", "memory": "1Gi"},
		KubeReserved:   map[string]v1alpha1.KubeletValue{"memory": "3Gi"},
		EvictionHard:   map[string]v1alpha1.KubeletValue{"memory.available": "10%"},
	}
	c := &OxideCloudProvider{
		kubeClient: fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(plain, reserved).
			Build(),
		instanceTypeProvider: instancetype.NewProvider(nil, availability.New(nil)),
	}

	get := func(nodeClassName, instanceType string) *cloudprovider.InstanceType {
		t.Helper()
		instanceTypes, err := c.GetInstanceTypes(context.Background(), nodePoolFor(nodeClassName))
		if err != nil {
			t.Fatalf("GetInstanceTypes(%s) error = %v", nodeClassName, err)
		}
		for _, it := range instanceTypes {
			if it.Name == instanceType {
				return it
			}
		}
		t.Fatalf("%s not offered to %s", instanceType, nodeClassName)
		return nil
	}
	before := get("plain", "8-16")
	after := get("reserved", "8-16")
	// The same boot disk must not hand the plain nodeclass the reserved one's overhead.
	again := get("plain", "8-16")

	if !before.Overhead.SystemReserved.Cpu().IsZero() ||
		!again.Overhead.SystemReserved.Cpu().IsZero() {
		t.Fatalf(
			"a nodeclass without kubelet reservations got system-reserved %v",
			again.Overhead.SystemReserved,
		)
	}
	if got := after.Overhead.SystemReserved.Cpu().MilliValue(); got != 500 {
		t.Errorf("system-reserved cpu = %dm, want 500m", got)
	}
	if got := after.Overhead.SystemReserved.Memory().Value(); got != 1<<30 {
		t.Errorf("system-reserved memory = %d, want 1Gi", got)
	}
	if got := after.Overhead.KubeReserved.Memory().Value(); got != 3<<30 {
		t.Errorf("kube-reserved memory = %d, want 3Gi", got)
	}
	if got, want := after.Overhead.EvictionThreshold.Memory().Value(),
		int64(float64(after.Capacity.Memory().Value())*float64(float32(0.1))); got != want {
		t.Errorf("eviction memory = %d, want 10%% of capacity = %d", got, want)
	}
	if got := after.Overhead.KubeReserved.Cpu(); !got.Equal(*before.Overhead.KubeReserved.Cpu()) {
		t.Errorf("kube-reserved cpu changed to %s without being set", got)
	}

	beforeAllocatable := before.Allocatable()[corev1.ResourceMemory]
	afterAllocatable := after.Allocatable()[corev1.ResourceMemory]
	if afterAllocatable.Cmp(beforeAllocatable) >= 0 {
		t.Errorf(
			"allocatable memory did not shrink: %s then %s",
			&beforeAllocatable,
			&afterAllocatable,
		)
	}
}

func TestGetInstanceTypesRejectsUnparseableKubeletReservations(t *testing.T) {
	nodeClass := nodeClassWithBootDisk("default", 20<<30, 80<<30)
	nodeClass.Spec.Kubelet = &v1alpha1.KubeletConfiguration{
		SystemReserved: map[string]v1alpha1.KubeletValue{"memory": "lots"},
	}
	c := &OxideCloudProvider{
		kubeClient: fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(nodeClass).
			Build(),
		instanceTypeProvider: instancetype.NewProvider(nil, availability.New(nil)),
	}
	if _, err := c.GetInstanceTypes(context.Background(), nodePoolFor("default")); err == nil {
		t.Fatal("GetInstanceTypes() accepted a reservation that is not a quantity")
	}
}
