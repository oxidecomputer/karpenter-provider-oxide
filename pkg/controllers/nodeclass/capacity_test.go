package nodeclass

import (
	"context"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const capacityProviderID = "oxide://6d1c2b0e-4a9f-4a52-9d3f-6f3c1a1e2b7d"

func capacityNodeClass(images ...string) *v1alpha1.OxideNodeClass {
	nodeClass := hashNodeClass(nil)
	for _, id := range images {
		nodeClass.Status.Images = append(nodeClass.Status.Images, v1alpha1.OxideImage{ID: id})
	}
	return nodeClass
}

func capacityNodeClaim(imageID string) *karpv1.NodeClaim {
	nodeClaim := hashNodeClaim("claim", nil)
	nodeClaim.Status.ProviderID = capacityProviderID
	nodeClaim.Status.ImageID = imageID
	return nodeClaim
}

func capacityNode(memory string) *corev1.Node {
	return &corev1.Node{
		Name: "node",
		Labels: map[string]string{
			karpv1.NodeRegisteredLabelKey:  "true",
			corev1.LabelInstanceTypeStable: "1-4",
			nodeClassLabel:                 defaultNodeClass,
		},
		Spec: corev1.NodeSpec{ProviderID: capacityProviderID},
		Status: corev1.NodeStatus{Capacity: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(memory),
		}},
	}
}

func capacityClientFor(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.OxideNodeClass{}).
		WithIndex(&karpv1.NodeClaim{}, "status.providerID", func(o client.Object) []string {
			return []string{o.(*karpv1.NodeClaim).Status.ProviderID}
		}).
		Build()
}

func observedCapacityOf(
	t *testing.T,
	kubeClient client.Client,
	node *corev1.Node,
) []v1alpha1.ObservedCapacity {
	t.Helper()
	if _, err := NewCapacityController(
		kubeClient,
	).Reconcile(context.Background(), node); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	nodeClass := &v1alpha1.OxideNodeClass{}
	if err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: defaultNodeClass},
		nodeClass,
	); err != nil {
		t.Fatalf("unable to re-read the nodeclass: %v", err)
	}
	return nodeClass.Status.ObservedCapacity
}

func TestCapacityRecordsWhatTheNodeRegisteredWith(t *testing.T) {
	kubeClient := capacityClientFor(t, capacityNodeClass("new"), capacityNodeClaim("new"))

	observed := observedCapacityOf(t, kubeClient, capacityNode("3737792Ki"))
	if len(observed) != 1 {
		t.Fatalf("observed = %v, want one entry", observed)
	}
	entry := observed[0]
	if entry.InstanceType != "1-4" || entry.ImageID != "new" ||
		entry.Memory.Cmp(resource.MustParse("3737792Ki")) != 0 {
		t.Fatalf("observed = %+v, want 1-4 on new at 3737792Ki", entry)
	}
}

func TestCapacityKeepsTheSmallestObservation(t *testing.T) {
	nodeClass := capacityNodeClass("new")
	nodeClass.Status.ObservedCapacity = []v1alpha1.ObservedCapacity{{
		InstanceType: "1-4", ImageID: "new", Memory: resource.MustParse("3700000Ki"),
	}}
	kubeClient := capacityClientFor(t, nodeClass, capacityNodeClaim("new"))

	observed := observedCapacityOf(t, kubeClient, capacityNode("3737792Ki"))
	if len(observed) != 1 || observed[0].Memory.Cmp(resource.MustParse("3700000Ki")) != 0 {
		t.Fatalf("a larger node replaced the smaller observation: %v", observed)
	}

	observed = observedCapacityOf(t, kubeClient, capacityNode("3600000Ki"))
	if len(observed) != 1 || observed[0].Memory.Cmp(resource.MustParse("3600000Ki")) != 0 {
		t.Fatalf("a smaller node did not replace the observation: %v", observed)
	}
}

func TestCapacityDropsImagesTheNodeClassNoLongerResolves(t *testing.T) {
	nodeClass := capacityNodeClass("new")
	nodeClass.Status.ObservedCapacity = []v1alpha1.ObservedCapacity{{
		InstanceType: "1-4", ImageID: "retired", Memory: resource.MustParse("3900000Ki"),
	}}
	kubeClient := capacityClientFor(t, nodeClass, capacityNodeClaim("new"))

	observed := observedCapacityOf(t, kubeClient, capacityNode("3737792Ki"))
	if len(observed) != 1 || observed[0].ImageID != "new" {
		t.Fatalf("observed = %v, want only the current image", observed)
	}
}

func TestCapacityIgnoresNodesOfAnUnresolvedImage(t *testing.T) {
	kubeClient := capacityClientFor(t, capacityNodeClass("new"), capacityNodeClaim("retired"))

	if observed := observedCapacityOf(
		t,
		kubeClient,
		capacityNode("3737792Ki"),
	); len(
		observed,
	) != 0 {
		t.Fatalf(
			"observed = %v, want nothing for an image the nodeclass no longer resolves",
			observed,
		)
	}
}

func TestCapacityWaitsForRegistration(t *testing.T) {
	kubeClient := capacityClientFor(t, capacityNodeClass("new"), capacityNodeClaim("new"))
	node := capacityNode("3737792Ki")
	delete(node.Labels, karpv1.NodeRegisteredLabelKey)

	if observed := observedCapacityOf(t, kubeClient, node); len(observed) != 0 {
		t.Fatalf("observed = %v, want nothing before registration", observed)
	}
}
