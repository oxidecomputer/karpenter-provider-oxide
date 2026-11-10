package framework

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Create creates objects in order and registers cleanup in reverse order.
// Cleanup checks for leaked NodeClaims, instances, and disks.
// Failed tests collect diagnostics before cleanup.
func (e *Env) Create(t *testing.T, objs ...client.Object) {
	t.Helper()
	ctx := t.Context()
	var nc *v1alpha1.OxideNodeClass
	var pool *karpv1.NodePool
	for _, obj := range objs {
		switch o := obj.(type) {
		case *v1alpha1.OxideNodeClass:
			nc = o
		case *karpv1.NodePool:
			pool = o
		}
	}
	t.Cleanup(func() {
		diagnose := func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			e.diagnose(ctx, t, nc, pool)
		}
		failed := t.Failed()
		if failed {
			diagnose()
			if e.Keep {
				t.Logf("E2E_KEEP=true: leaving %s behind", nc.Name)
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), e.DeleteTimeout)
		defer cancel()
		var launches []Launch
		if pool != nil {
			launches = e.launches(ctx, t, pool)
		}
		for _, obj := range slices.Backward(objs) {
			if err := e.Kube.Delete(ctx, obj, client.GracePeriodSeconds(0)); err != nil &&
				!apierrors.IsNotFound(err) {
				t.Errorf("deleting %T %s: %v", obj, obj.GetName(), err)
			}
		}
		if err := e.waitForCleanup(ctx, pool, launches); err != nil {
			t.Error(err)
		}
		if !failed && t.Failed() {
			diagnose()
		}
	})
	for _, obj := range objs {
		if err := e.Kube.Create(ctx, obj); err != nil {
			t.Fatalf("creating %T %s: %v", obj, obj.GetName(), err)
		}
	}
}

// Launch identifies a NodeClaim's instance and disks for cleanup checks.
// Capture it before deleting the NodeClaim.
type Launch struct {
	NodeClaim  string
	InstanceID string
	DiskPrefix string
	DiskIDs    []string
}

// LaunchOf reads the launch off a nodeclaim.
func LaunchOf(claim *karpv1.NodeClaim) Launch {
	return Launch{
		NodeClaim:  claim.Name,
		InstanceID: instanceID(claim.Status.ProviderID),
		DiskPrefix: claim.Annotations[v1alpha1.AnnotationDiskPrefix],
		DiskIDs:    v1alpha1.ParseIDs(claim.Annotations[v1alpha1.AnnotationDiskIDs]).UnsortedList(),
	}
}

func (e *Env) launches(ctx context.Context, t *testing.T, pool *karpv1.NodePool) []Launch {
	t.Helper()
	claims, err := e.NodeClaims(ctx, pool)
	if err != nil {
		t.Errorf("listing nodeclaims of %s: %v", pool.Name, err)
		return nil
	}
	out := make([]Launch, 0, len(claims))
	for i := range claims {
		out = append(out, LaunchOf(&claims[i]))
	}
	return out
}

func instanceID(providerID string) string {
	return strings.TrimPrefix(providerID, "oxide://")
}
