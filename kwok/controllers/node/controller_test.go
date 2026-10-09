package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const testID = "d4f97e61-2c39-4c18-96b3-4718ecc18efa"

type instanceReader struct {
	instance *oxide.Instance
	err      error
}

func (r *instanceReader) InstanceView(
	context.Context,
	oxide.InstanceViewParams,
) (*oxide.Instance, error) {
	return r.instance, r.err
}

func fixture() (*karpv1.NodeClaim, *instanceReader) {
	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "example",
			UID:  "claim-uid",
			Labels: map[string]string{
				karpv1.NodePoolLabelKey: "test",
				corev1.LabelOSStable:    "linux",
				corev1.LabelArchStable:  "amd64",
			},
		},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: "karpenter.oxide.computer",
				Kind:  "OxideNodeClass",
				Name:  "test",
			},
		},
	}
	claim.Status.ProviderID = "oxide://" + testID
	claim.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("4"),
		corev1.ResourceMemory:           resource.MustParse("8Gi"),
		corev1.ResourcePods:             resource.MustParse("110"),
		corev1.ResourceEphemeralStorage: resource.MustParse("40Gi"),
	}
	claim.Status.Allocatable = claim.Status.Capacity.DeepCopy()
	claim.Status.Allocatable[corev1.ResourceEphemeralStorage] = resource.MustParse("36Gi")
	claim.StatusConditions().SetTrue(karpv1.ConditionTypeLaunched)
	return claim, &instanceReader{
		instance: &oxide.Instance{
			Id:        testID,
			Hostname:  "karpenter-example",
			ProjectId: "project",
			RunState:  "running",
			Description: ownership.Marker{
				ClusterUID:    "cluster",
				NodeClaimUID:  claim.UID,
				NodeClaimName: claim.Name,
				Kind:          ownership.KindInstance,
			}.Description(
				"",
			),
		},
	}
}
func reconciler(k client.Client, r InstanceReader) *Controller {
	return &Controller{
		Client:            k,
		Reader:            k,
		Instances:         r,
		ClusterUID:        "cluster",
		ProjectID:         "project",
		KubernetesVersion: "v1.36.1",
		PollInterval:      5 * time.Second,
		StaleAfter:        time.Minute,
		Clock:             clocktesting.NewFakeClock(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)),
	}
}
func step(t *testing.T, c *Controller) error {
	t.Helper()
	_, err := c.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "example"}},
	)
	return err
}
func getNode(t *testing.T, k client.Client) *corev1.Node {
	t.Helper()
	n := &corev1.Node{}
	if err := k.Get(
		context.Background(),
		client.ObjectKey{Name: "karpenter-example"},
		n,
	); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestProjection(t *testing.T) {
	claim, _ := fixture()
	permanent := corev1.Taint{Key: "permanent", Value: "keep", Effect: corev1.TaintEffectNoSchedule}
	claim.Spec.Taints = []corev1.Taint{permanent}
	claim.Spec.StartupTaints = []corev1.Taint{
		permanent,
		{Key: "startup", Effect: corev1.TaintEffectNoExecute},
	}
	claim.Labels[BackendState] = "foreign"
	n := Project(claim, "cluster", testID, "v1.36.1")
	if n.Spec.ProviderID != claim.Status.ProviderID || n.Name != "karpenter-example" ||
		n.OwnerReferences[0].UID != claim.UID ||
		n.Labels[BackendState] != "running" ||
		len(n.Spec.Taints) != 3 {
		t.Fatalf("bad projection: %#v", n)
	}
	if n.Status.Allocatable.StorageEphemeral().Cmp(resource.MustParse("36Gi")) != 0 {
		t.Fatal("lost boot-size-dependent storage")
	}
	n.Status.Capacity[corev1.ResourceCPU] = resource.MustParse("1")
	n.Labels[karpv1.NodePoolLabelKey] = "changed"
	if claim.Status.Capacity.Cpu().String() != "4" ||
		claim.Labels[karpv1.NodePoolLabelKey] != "test" {
		t.Fatal("projection aliases claim")
	}
}

func TestRegistrationGates(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*karpv1.NodeClaim, *instanceReader)
		wantErr bool
	}{
		{
			"starting",
			func(_ *karpv1.NodeClaim, r *instanceReader) { r.instance.RunState = "starting" },
			false,
		},
		{
			"stopped",
			func(_ *karpv1.NodeClaim, r *instanceReader) { r.instance.RunState = "stopped" },
			false,
		},
		{
			"failed",
			func(_ *karpv1.NodeClaim, r *instanceReader) { r.instance.RunState = "failed" },
			false,
		},
		{
			"missing",
			func(_ *karpv1.NodeClaim, r *instanceReader) { r.err = oxide.ErrObjectNotFound },
			false,
		},
		{
			"unauthorized",
			func(_ *karpv1.NodeClaim, r *instanceReader) { r.err = oxide.ErrUnauthenticated },
			true,
		},
		{
			"timeout",
			func(_ *karpv1.NodeClaim, r *instanceReader) { r.err = context.DeadlineExceeded },
			true,
		},
		{
			"project",
			func(_ *karpv1.NodeClaim, r *instanceReader) { r.instance.ProjectId = "other" },
			true,
		},
		{
			"owner",
			func(_ *karpv1.NodeClaim, r *instanceReader) { r.instance.Description = "unowned" },
			true,
		},
		{
			"hostname",
			func(_ *karpv1.NodeClaim, r *instanceReader) { r.instance.Hostname = "other" },
			true,
		},
		{"incomplete", func(c *karpv1.NodeClaim, _ *instanceReader) {
			delete(c.Status.Allocatable, corev1.ResourceEphemeralStorage)
		}, false},
		{
			"invalid-id",
			func(c *karpv1.NodeClaim, _ *instanceReader) { c.Status.ProviderID = "oxide://invalid" },
			true,
		},
		{"not-launched", func(c *karpv1.NodeClaim, _ *instanceReader) {
			c.StatusConditions().SetFalse(karpv1.ConditionTypeLaunched, "Waiting", "Waiting")
		}, false},
		{"registered-lost", func(c *karpv1.NodeClaim, _ *instanceReader) {
			c.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
		}, false},
		{"deleting", func(c *karpv1.NodeClaim, _ *instanceReader) {
			now := metav1.Now()
			c.DeletionTimestamp = &now
			c.Finalizers = []string{"test"}
		}, false},
		{
			"foreign-kind",
			func(c *karpv1.NodeClaim, _ *instanceReader) { c.Spec.NodeClassRef.Kind = "Foreign" },
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claim, r := fixture()
			tt.change(claim, r)
			k := fake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithStatusSubresource(&karpv1.NodeClaim{}, &corev1.Node{}).
				WithObjects(claim).
				Build()
			err := step(t, reconciler(k, r))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v", err)
			}
			nodes := &corev1.NodeList{}
			if err := k.List(context.Background(), nodes); err != nil {
				t.Fatal(err)
			}
			if len(nodes.Items) != 0 {
				t.Fatal("registered an ineligible claim")
			}
		})
	}
}

func TestRestartHealthAndTaints(t *testing.T) {
	ctx := context.Background()
	claim, r := fixture()
	claim.Spec.Taints = []corev1.Taint{{Key: "permanent", Effect: corev1.TaintEffectNoSchedule}}
	claim.Spec.StartupTaints = []corev1.Taint{
		{Key: "cni", Effect: corev1.TaintEffectNoSchedule},
		{Key: "unknown", Effect: corev1.TaintEffectNoSchedule},
	}
	k := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&karpv1.NodeClaim{}, &corev1.Node{}).
		WithObjects(claim).
		Build()
	c := reconciler(k, r)
	if err := step(t, c); err != nil {
		t.Fatal(err)
	}
	n := getNode(t, k)
	if !complete(claim) || n.Status.Capacity.Cpu().String() != "4" ||
		len(n.Status.Conditions) != 0 {
		t.Fatal("initial status incorrect")
	}
	// Simulate a crash between creation and status initialization, then a fresh process.
	delete(n.Annotations, ResourcesInitialized)
	if err := k.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	n.Status = corev1.NodeStatus{
		Conditions: []corev1.NodeCondition{
			{Type: "OxideSledHealthy", Status: corev1.ConditionTrue},
		},
	}
	if err := k.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	c = reconciler(k, r)
	if err := step(t, c); err != nil {
		t.Fatal(err)
	}
	n = getNode(t, k)
	if n.Status.Capacity.Cpu().String() != "4" || len(n.Status.Conditions) != 1 {
		t.Fatal("restart did not preserve conditions and restore resources")
	}
	if err := k.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
		t.Fatal(err)
	}
	claim.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
	if err := k.Status().Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	c.RemoveStartupTaints = []string{"cni:NoSchedule", "permanent:NoSchedule"}
	n.Status.Capacity[corev1.ResourceCPU] = resource.MustParse("3")
	if err := k.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := step(t, c); err != nil {
		t.Fatal(err)
	}
	n = getNode(t, k)
	if n.Status.Capacity.Cpu().String() != "3" {
		t.Fatal("overwrote registered node resources")
	}
	for _, taint := range n.Spec.Taints {
		if taint.Key == "cni" {
			t.Fatal("allowlisted startup taint not removed")
		}
	}
	if len(n.Spec.Taints) != 3 {
		t.Fatal("removed permanent/unknown/unregistered taint")
	}
	r.err = oxide.ErrForbidden
	if err := step(t, c); err == nil {
		t.Fatal("expected API failure")
	}
	if getNode(t, k).Labels[BackendState] != "running" {
		t.Fatal("transient API error changed state")
	}
	c.Clock.(*clocktesting.FakeClock).Step(2 * time.Minute)
	if err := step(t, c); err == nil {
		t.Fatal("expected stale API failure")
	}
	if getNode(t, k).Labels[BackendState] != "unknown" {
		t.Fatal("stale backend not unknown")
	}
	r.err = oxide.ErrObjectNotFound
	if err := step(t, c); err != nil {
		t.Fatal(err)
	}
	if getNode(t, k).Labels[BackendState] != "missing" {
		t.Fatal("confirmed absence not published")
	}
	r.err = nil
	if err := step(t, c); err != nil {
		t.Fatal(err)
	}
	if getNode(t, k).Labels[BackendState] != "running" {
		t.Fatal("backend recovery failed")
	}
}

func TestCollisions(t *testing.T) {
	for _, kind := range []string{"name", "provider-id", "claim-uid", "deleting"} {
		t.Run(kind, func(t *testing.T) {
			claim, r := fixture()
			n := Project(claim, "cluster", testID, "v1.36.1")
			switch kind {
			case "name":
				n.Spec.ProviderID = "foreign"
			case "provider-id":
				n.Name = "other"
			case "claim-uid":
				n.Annotations[ClaimUID] = "foreign"
			case "deleting":
				now := metav1.Now()
				n.DeletionTimestamp = &now
				n.Finalizers = []string{"keep"}
			}
			k := fake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithStatusSubresource(&corev1.Node{}, &karpv1.NodeClaim{}).
				WithObjects(claim, n).
				Build()
			err := step(t, reconciler(k, r))
			if kind != "deleting" && err == nil {
				t.Fatal("expected collision")
			}
			if kind == "deleting" && err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A failed status write must leave a recoverable initial Node, with no memory backup.
type failStatusClient struct{ client.Client }
type failStatusWriter struct{ client.SubResourceWriter }

func (c failStatusClient) Status() client.SubResourceWriter {
	return failStatusWriter{c.Client.Status()}
}

func (w failStatusWriter) Patch(
	context.Context,
	client.Object,
	client.Patch,
	...client.SubResourcePatchOption,
) error {
	return apierrors.NewConflict(
		schema.GroupResource{Resource: "nodes"},
		"karpenter-example",
		errors.New("injected conflict"),
	)
}
func TestStatusConflictRecovery(t *testing.T) {
	claim, r := fixture()
	k := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&corev1.Node{}, &karpv1.NodeClaim{}).
		WithObjects(claim).
		Build()
	c := reconciler(k, r)
	c.Client = failStatusClient{k}
	if err := step(t, c); !apierrors.IsConflict(err) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// Core registration can win the race against the first status write.
	if err := k.Get(context.Background(), client.ObjectKeyFromObject(claim), claim); err != nil {
		t.Fatal(err)
	}
	claim.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
	if err := k.Status().Update(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if err := step(t, reconciler(k, r)); err != nil {
		t.Fatal(err)
	}
	if getNode(t, k).Status.Capacity.Cpu().String() != "4" {
		t.Fatal("did not resume status initialization")
	}
}
