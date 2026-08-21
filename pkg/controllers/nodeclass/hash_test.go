package nodeclass

import (
	"context"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// version is the hash version every stamp written by this controller carries.
const version = v1alpha1.OxideNodeClassHashVersion

// hashNodeClass is a nodeclass with something in the hashed part of its spec, so that a stamp is
// distinguishable from the hash of an empty spec.
// defaultNodeClass is the nodeclass name these cases share, so that what varies between them is only
// the configuration being hashed.
const defaultNodeClass = "default"

func hashNodeClass(annotations map[string]string) *v1alpha1.OxideNodeClass {
	return &v1alpha1.OxideNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: defaultNodeClass, Annotations: annotations},
		Spec:       v1alpha1.OxideNodeClassSpec{UserData: "#!/bin/sh\n"},
	}
}

// hashNodeClaim is a NodeClaim of that nodeclass, carrying whatever launch record the case is about.
func hashNodeClaim(name string, annotations map[string]string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: "karpenter.oxide.computer",
				Kind:  "OxideNodeClass",
				Name:  defaultNodeClass,
			},
		},
	}
}

func hashClientFor(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objects...).
		WithStatusSubresource(&karpv1.NodeClaim{}).
		Build()
}

// reconcileHash runs one reconcile, since every case here is about what one leaves behind.
func reconcileHash(
	t *testing.T,
	kubeClient client.Client,
	nodeClass *v1alpha1.OxideNodeClass,
) {
	t.Helper()
	controller := NewHashController(kubeClient)
	if _, err := controller.Reconcile(context.Background(), nodeClass); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

func annotationsOf(t *testing.T, kubeClient client.Client, name string) map[string]string {
	t.Helper()
	nodeClaim := &karpv1.NodeClaim{}
	if err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: name},
		nodeClaim,
	); err != nil {
		t.Fatalf("unable to re-read the NodeClaim: %v", err)
	}
	return nodeClaim.Annotations
}

// The stamp is what drift is decided against, so it has to appear on a nodeclass that has never
// carried one, and it has to be the hash of the current spec.
func TestHashReconcileStampsTheNodeClass(t *testing.T) {
	nodeClass := hashNodeClass(nil)
	kubeClient := hashClientFor(t, nodeClass)

	reconcileHash(t, kubeClient, nodeClass)

	updated := &v1alpha1.OxideNodeClass{}
	if err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: defaultNodeClass},
		updated,
	); err != nil {
		t.Fatalf("unable to re-read the nodeclass: %v", err)
	}
	if got, want := updated.Annotations[v1alpha1.AnnotationOxideNodeClassHash], nodeClass.Hash(); got != want {
		t.Errorf("hash annotation = %q, want %q", got, want)
	}
	if got := updated.Annotations[v1alpha1.AnnotationOxideNodeClassHashVersion]; got != version {
		t.Errorf("hash version annotation = %q, want %q", got, version)
	}
}

// The migration case, and the one that matters most: a nodeclass reconciled for the first time by a
// version that hashes at all. Its existing nodes recorded no hash, and stamping the nodeclass without
// stamping them would leave a fleet that can never be compared. Adopting the current hash is what
// gives them static drift from here on without replacing any of them now.
func TestHashReconcileAdoptsExistingNodeClaims(t *testing.T) {
	nodeClass := hashNodeClass(nil)
	nodeClaim := hashNodeClaim("node-1", nil)
	kubeClient := hashClientFor(t, nodeClass, nodeClaim)

	reconcileHash(t, kubeClient, nodeClass)

	annotations := annotationsOf(t, kubeClient, "node-1")
	if got, want := annotations[v1alpha1.AnnotationOxideNodeClassHash], nodeClass.Hash(); got != want {
		t.Errorf("NodeClaim hash annotation = %q, want the nodeclass's %q", got, want)
	}
	if got := annotations[v1alpha1.AnnotationOxideNodeClassHashVersion]; got != version {
		t.Errorf("NodeClaim hash version annotation = %q, want %q", got, version)
	}
}

// A NodeClaim already found drifted keeps the hash it has. It was judged while the two were
// comparable, and re-stamping it would erase a finding this version has no way to reproduce.
func TestHashReconcileLeavesDriftedNodeClaimsAlone(t *testing.T) {
	nodeClass := hashNodeClass(nil)
	nodeClaim := hashNodeClaim("node-1", map[string]string{
		v1alpha1.AnnotationOxideNodeClassHash:        "stale",
		v1alpha1.AnnotationOxideNodeClassHashVersion: "v-old",
	})
	nodeClaim.StatusConditions().SetTrue(karpv1.ConditionTypeDrifted)
	kubeClient := hashClientFor(t, nodeClass, nodeClaim)

	reconcileHash(t, kubeClient, nodeClass)

	annotations := annotationsOf(t, kubeClient, "node-1")
	if got := annotations[v1alpha1.AnnotationOxideNodeClassHash]; got != "stale" {
		t.Errorf("NodeClaim hash annotation = %q, want the drifted node's own %q", got, "stale")
	}
	// The version still moves, so the node is not re-examined at every reconcile for a mismatch that
	// is never going to be resolved.
	if got := annotations[v1alpha1.AnnotationOxideNodeClassHashVersion]; got != version {
		t.Errorf("NodeClaim hash version annotation = %q, want %q", got, version)
	}
}

// Once the nodeclass is at the current version, a spec edit only moves its own stamp. Re-stamping the
// nodes here would be re-stamping away the very drift the edit is supposed to produce.
func TestHashReconcileDoesNotRestampAtTheCurrentVersion(t *testing.T) {
	nodeClass := hashNodeClass(map[string]string{
		v1alpha1.AnnotationOxideNodeClassHash:        "previous",
		v1alpha1.AnnotationOxideNodeClassHashVersion: v1alpha1.OxideNodeClassHashVersion,
	})
	nodeClaim := hashNodeClaim("node-1", map[string]string{
		v1alpha1.AnnotationOxideNodeClassHash:        "previous",
		v1alpha1.AnnotationOxideNodeClassHashVersion: v1alpha1.OxideNodeClassHashVersion,
	})
	kubeClient := hashClientFor(t, nodeClass, nodeClaim)

	reconcileHash(t, kubeClient, nodeClass)

	if got := annotationsOf(t, kubeClient, "node-1")[v1alpha1.AnnotationOxideNodeClassHash]; got != "previous" {
		t.Errorf("NodeClaim hash annotation = %q, want the launch's own %q", got, "previous")
	}
}

// A NodeClaim belonging to some other nodeclass, or to another provider's nodeclass that happens to
// share the name, is none of this controller's business.
func TestHashReconcileIgnoresOtherNodeClaims(t *testing.T) {
	nodeClass := hashNodeClass(nil)
	otherName := hashNodeClaim("node-1", nil)
	otherName.Spec.NodeClassRef.Name = "other"
	otherGroup := hashNodeClaim("node-2", nil)
	otherGroup.Spec.NodeClassRef.Group = "karpenter.k8s.aws"
	kubeClient := hashClientFor(t, nodeClass, otherName, otherGroup)

	reconcileHash(t, kubeClient, nodeClass)

	for _, name := range []string{"node-1", "node-2"} {
		if got, ok := annotationsOf(t, kubeClient, name)[v1alpha1.AnnotationOxideNodeClassHash]; ok {
			t.Errorf("NodeClaim %s hash annotation = %q, want it untouched", name, got)
		}
	}
}

// A nodeclass being deleted is taking its nodes with it, so there is nothing a stamp would inform.
func TestHashReconcileSkipsADeletingNodeClass(t *testing.T) {
	nodeClass := hashNodeClass(nil)
	now := metav1.Now()
	nodeClass.DeletionTimestamp = &now
	nodeClass.Finalizers = []string{v1alpha1.TerminationFinalizer}
	kubeClient := hashClientFor(t, nodeClass)

	reconcileHash(t, kubeClient, nodeClass)

	if _, ok := nodeClass.Annotations[v1alpha1.AnnotationOxideNodeClassHash]; ok {
		t.Error("the deleting nodeclass was stamped, want it left alone")
	}
}
