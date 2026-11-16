package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestWaitForCRDs(t *testing.T) {
	client := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	client.Resources = []*metav1.APIResourceList{{
		GroupVersion: "karpenter.sh/v1",
		APIResources: []metav1.APIResource{{Name: "nodeclaims"}},
	}}
	if got := missingResources(client); len(got) != 2 {
		t.Fatalf("missing %v, want nodepools and oxidenodeclasses", got)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := waitForCRDs(
		ctx,
		client,
		time.Millisecond,
	); !errors.Is(
		err,
		context.DeadlineExceeded,
	) {
		t.Fatalf("returned %v while CRDs were missing", err)
	}

	client.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "karpenter.sh/v1",
			APIResources: []metav1.APIResource{{Name: "nodeclaims"}, {Name: "nodepools"}},
		},
		{
			GroupVersion: v1alpha1.GroupVersion.String(),
			APIResources: []metav1.APIResource{{Name: "oxidenodeclasses"}},
		},
	}
	if err := waitForCRDs(t.Context(), client, time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
