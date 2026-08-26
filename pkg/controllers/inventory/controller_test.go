package inventory

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/sled"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
)

var observedAt = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

func placement() sled.Placement {
	return sled.Placement{
		SledID:                "b6a2f1d0-0000-4000-8000-000000000001",
		RackID:                "b6a2f1d0-0000-4000-8000-000000000002",
		Serial:                "BRM42220004",
		Part:                  "913-0000019",
		Revision:              2,
		State:                 "active",
		Policy:                "in_service",
		ProvisionPolicy:       "provisionable",
		UsableHardwareThreads: 128,
		UsablePhysicalRam:     1099511627776,
	}
}

func TestDecorationsRendersEveryField(t *testing.T) {
	labels, annotations := decorations(placement(), observedAt)

	for key, want := range map[string]string{
		v1alpha1.LabelSledID:          "b6a2f1d0-0000-4000-8000-000000000001",
		v1alpha1.LabelSledRackID:      "b6a2f1d0-0000-4000-8000-000000000002",
		v1alpha1.LabelSledSerial:      "BRM42220004",
		v1alpha1.LabelSledState:       "active",
		v1alpha1.LabelSledPolicy:      "in_service",
		v1alpha1.LabelSledProvisiable: "provisionable",
	} {
		if got := labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}

	for key, want := range map[string]string{
		v1alpha1.AnnotationSledBaseboardPart:     "913-0000019",
		v1alpha1.AnnotationSledBaseboardRevision: "2",
		v1alpha1.AnnotationSledHardwareThreads:   "128",
		v1alpha1.AnnotationSledPhysicalRAM:       "1099511627776",
		v1alpha1.AnnotationSledObservedAt:        "2026-08-18T12:00:00Z",
	} {
		if got := annotations[key]; got != want {
			t.Errorf("annotation %s = %q, want %q", key, got, want)
		}
	}
}

func TestDecorationsOmitsEmptyValues(t *testing.T) {
	p := placement()
	p.ProvisionPolicy = ""
	p.Policy = "expunged"
	p.Part = ""
	p.Revision = 0

	labels, annotations := decorations(p, observedAt)

	if _, ok := labels[v1alpha1.LabelSledProvisiable]; ok {
		t.Error("an expunged sled was labeled with a provision policy")
	}
	if got := labels[v1alpha1.LabelSledPolicy]; got != "expunged" {
		t.Errorf("sled-policy label = %q, want %q", got, "expunged")
	}
	if _, ok := annotations[v1alpha1.AnnotationSledBaseboardPart]; ok {
		t.Error("an empty baseboard part was annotated")
	}
	if _, ok := annotations[v1alpha1.AnnotationSledBaseboardRevision]; ok {
		t.Error("a zero baseboard revision was annotated")
	}
}

func TestDecorationsDropsIllegalLabelValues(t *testing.T) {
	p := placement()
	p.Serial = strings.Repeat("x", 64)

	labels, annotations := decorations(p, observedAt)

	if _, ok := labels[v1alpha1.LabelSledSerial]; ok {
		t.Error("an over-long serial was written as a label value")
	}
	if got := annotations[v1alpha1.LabelSledSerial]; got != p.Serial {
		t.Errorf("serial annotation = %q, want the full serial", got)
	}
	if _, ok := labels[v1alpha1.LabelSledID]; !ok {
		t.Error("one illegal value dropped the rest of the labels")
	}
}

func TestNeedsPatch(t *testing.T) {
	_, annotations := decorations(placement(), observedAt)
	labels, _ := decorations(placement(), observedAt)

	if needsPatch(labels, labels) {
		t.Error("identical labels were reported as needing a patch")
	}
	if !needsPatch(nil, labels) {
		t.Error("an unlabeled node was reported as not needing a patch")
	}

	moved := maps.Clone(labels)
	moved[v1alpha1.LabelSledID] = "b6a2f1d0-0000-4000-8000-000000000009"
	if !needsPatch(labels, moved) {
		t.Error("a changed sled ID was reported as not needing a patch")
	}

	_, later := decorations(placement(), observedAt.Add(time.Hour))
	if needsPatch(annotations, later) {
		t.Error("a newer timestamp alone was reported as needing a patch")
	}
}

func TestConditionReasonsCoverEverySledReason(t *testing.T) {
	for _, reason := range []sled.UnhealthyReason{
		sled.ReasonExpunged,
		sled.ReasonNonProvisionable,
	} {
		if conditionReasons[reason] == "" {
			t.Errorf("sled reason %q has no condition reason mapped", reason)
		}
	}
}

func TestUpsertConditionLeavesOtherConditionsAlone(t *testing.T) {
	existing := []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		{Type: v1alpha1.ConditionTypeSledUnhealthy, Status: corev1.ConditionFalse},
		{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
	}

	got := upsertCondition(existing, corev1.NodeCondition{
		Type:   v1alpha1.ConditionTypeSledUnhealthy,
		Status: corev1.ConditionTrue,
	})

	if len(got) != 3 {
		t.Fatalf("conditions has %d entries, want 3", len(got))
	}
	if status := nodeutils.GetCondition(
		&corev1.Node{Status: corev1.NodeStatus{Conditions: got}},
		v1alpha1.ConditionTypeSledUnhealthy,
	).Status; status != corev1.ConditionTrue {
		t.Errorf("SledUnhealthy = %q, want True", status)
	}
	if got[0].Type != corev1.NodeReady || got[2].Type != corev1.NodeMemoryPressure {
		t.Error("an unrelated condition was displaced")
	}
}

func TestUpsertConditionAppendsWhenAbsent(t *testing.T) {
	got := upsertCondition(
		[]corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		corev1.NodeCondition{
			Type:   v1alpha1.ConditionTypeSledUnhealthy,
			Status: corev1.ConditionTrue,
		},
	)

	if len(got) != 2 {
		t.Fatalf("conditions has %d entries, want 2", len(got))
	}
	if got[1].Type != v1alpha1.ConditionTypeSledUnhealthy {
		t.Errorf("appended condition is %q, want SledUnhealthy", got[1].Type)
	}
}

func newNodeController(t *testing.T, node *corev1.Node) (*Controller, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("unable to build a scheme: %v", err)
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&corev1.Node{}).
		Build()

	return NewController(kubeClient, nil, time.Minute), kubeClient
}

func conditionOn(t *testing.T, kubeClient client.Client, name string) corev1.NodeCondition {
	t.Helper()

	node := &corev1.Node{}
	if err := kubeClient.Get(t.Context(), client.ObjectKey{Name: name}, node); err != nil {
		t.Fatalf("unable to read the node back: %v", err)
	}
	return nodeutils.GetCondition(node, v1alpha1.ConditionTypeSledUnhealthy)
}

func TestApplyConditionMarksUnhealthySled(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	c, kubeClient := newNodeController(t, node)

	p := placement()
	p.Policy = "expunged"

	if err := c.applyCondition(t.Context(), node, p); err != nil {
		t.Fatalf("applyCondition returned %v", err)
	}

	got := conditionOn(t, kubeClient, "node-1")
	if got.Status != corev1.ConditionTrue {
		t.Errorf("SledUnhealthy = %q, want True", got.Status)
	}
	if got.Reason != v1alpha1.ConditionReasonSledExpunged {
		t.Errorf("Reason = %q, want %q", got.Reason, v1alpha1.ConditionReasonSledExpunged)
	}
	if got.Message == "" {
		t.Error("an unhealthy condition was written with no message")
	}
}

func TestApplyConditionClearsWhenSledReturnsToService(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:               v1alpha1.ConditionTypeSledUnhealthy,
			Status:             corev1.ConditionTrue,
			Reason:             v1alpha1.ConditionReasonSledNonProvisonable,
			LastTransitionTime: metav1.NewTime(observedAt),
		}}},
	}
	c, kubeClient := newNodeController(t, node)

	if err := c.applyCondition(t.Context(), node, placement()); err != nil {
		t.Fatalf("applyCondition returned %v", err)
	}

	got := conditionOn(t, kubeClient, "node-1")
	if got.Status != corev1.ConditionFalse {
		t.Errorf("SledUnhealthy = %q, want False", got.Status)
	}
	if got.Reason != v1alpha1.ConditionReasonSledHealthy {
		t.Errorf("Reason = %q, want %q", got.Reason, v1alpha1.ConditionReasonSledHealthy)
	}
}

func TestApplyConditionDoesNotResetTransitionTime(t *testing.T) {
	transitioned := metav1.NewTime(observedAt)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:               v1alpha1.ConditionTypeSledUnhealthy,
			Status:             corev1.ConditionTrue,
			Reason:             v1alpha1.ConditionReasonSledExpunged,
			Message:            "a message from an earlier observation",
			LastTransitionTime: transitioned,
		}}},
	}
	c, kubeClient := newNodeController(t, node)

	p := placement()
	p.Policy = "expunged"

	if err := c.applyCondition(t.Context(), node, p); err != nil {
		t.Fatalf("applyCondition returned %v", err)
	}

	got := conditionOn(t, kubeClient, "node-1")
	if !got.LastTransitionTime.Equal(&transitioned) {
		t.Errorf(
			"LastTransitionTime = %v, want it left at %v",
			got.LastTransitionTime, transitioned,
		)
	}
}

func TestMergedLeavesForeignKeysAlone(t *testing.T) {
	current := map[string]string{
		"kubernetes.io/hostname": "node-1",
		v1alpha1.LabelSledID:     "old",
	}
	labels, _ := decorations(placement(), observedAt)

	got := merged(current, labels)

	if got["kubernetes.io/hostname"] != "node-1" {
		t.Error("a label this controller does not own was modified")
	}
	if got[v1alpha1.LabelSledID] != placement().SledID {
		t.Errorf(
			"sled-id = %q, want it overwritten with the current placement",
			got[v1alpha1.LabelSledID],
		)
	}
}
