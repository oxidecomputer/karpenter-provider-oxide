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

// Create creates the objects in order and registers the one cleanup that tears everything
// down: the objects are deleted in reverse, workload first, then the nodeclaims, the Oxide
// instances and their disks must go away. A leak fails the test. On failure, diagnostics
// are written first.
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
		ctx, cancel := context.WithTimeout(context.Background(), e.DeleteTimeout+time.Minute)
		defer cancel()
		if t.Failed() {
			e.diagnose(ctx, t, nc, pool)
			if e.Keep {
				t.Logf("E2E_KEEP=true: leaving %s behind", nc.Name)
				return
			}
		}
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
		e.EventuallyGone(ctx, t, pool, launches)
	})
	for _, obj := range objs {
		if err := e.Kube.Create(ctx, obj); err != nil {
			t.Fatalf("creating %T %s: %v", obj, obj.GetName(), err)
		}
	}
}

// Launch is what a nodeclaim put on the rack, and what has to disappear from Oxide once
// the nodeclaim is gone. Take it before a test removes the nodeclaim, since the cleanup
// only sees the nodeclaims that still exist.
type Launch struct {
	NodeClaim  string
	InstanceID string
	DiskPrefix string
}

// LaunchOf reads the launch off a nodeclaim.
func LaunchOf(claim *karpv1.NodeClaim) Launch {
	return Launch{
		NodeClaim:  claim.Name,
		InstanceID: instanceID(claim.Status.ProviderID),
		DiskPrefix: claim.Annotations[v1alpha1.AnnotationDiskPrefix],
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
