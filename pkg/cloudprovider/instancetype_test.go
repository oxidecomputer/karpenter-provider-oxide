package cloudprovider

import (
	"context"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func nodeClassWithBootDisk(name string, spec, status int64) *v1alpha1.OxideNodeClass {
	nodeClass := &v1alpha1.OxideNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
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
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
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

	want, ok := instancetype.EphemeralStorageFor(bootSizeBytes)
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
	want, _ := instancetype.EphemeralStorageFor(bootSizeBytes)
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
