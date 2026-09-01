package bootstrap_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
)

type fake struct {
	name    string
	present bool
	err     error
	calls   int
}

func (f *fake) Name() string { return f.name }

func (f *fake) Resolve(context.Context, *v1alpha1.OxideNodeClass, bootstrap.Cluster) error {
	return nil
}

func (f *fake) UserData(
	context.Context,
	*v1alpha1.OxideNodeClass,
	bootstrap.Request,
) (string, error) {
	return "", nil
}

func (f *fake) Detect(context.Context) (bool, error) {
	f.calls++
	return f.present, f.err
}

func TestForDetectsTheClusterWhenTheSpecNamesNoProvider(t *testing.T) {
	rke2 := &fake{name: v1alpha1.BootstrapKindRKE2}
	kubeadm := &fake{name: v1alpha1.BootstrapKindKubeadm, present: true}
	registry := bootstrap.New(rke2, kubeadm)

	nodeClass := &v1alpha1.OxideNodeClass{}
	got, err := registry.For(context.Background(), nodeClass)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name() != v1alpha1.BootstrapKindKubeadm {
		t.Fatalf("got %q", got.Name())
	}

	// The cluster cannot change providers underneath us, so detection runs once.
	if _, err := registry.For(context.Background(), nodeClass); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kubeadm.calls != 1 {
		t.Fatalf("detected %d times, want 1", kubeadm.calls)
	}
}

func TestForPrefersTheConfiguredProviderOverDetection(t *testing.T) {
	rke2 := &fake{name: v1alpha1.BootstrapKindRKE2}
	kubeadm := &fake{name: v1alpha1.BootstrapKindKubeadm, present: true}
	registry := bootstrap.New(rke2, kubeadm)

	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Bootstrap: &v1alpha1.Bootstrap{RKE2: &v1alpha1.RKE2Bootstrap{}},
		},
	}
	got, err := registry.For(context.Background(), nodeClass)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name() != v1alpha1.BootstrapKindRKE2 {
		t.Fatalf("got %q", got.Name())
	}
	if kubeadm.calls != 0 {
		t.Fatalf("detection ran for a nodeclass that named its provider")
	}
}

func TestForReusesTheDetectedProviderRecordedInStatus(t *testing.T) {
	rke2 := &fake{name: v1alpha1.BootstrapKindRKE2}
	registry := bootstrap.New(rke2)

	nodeClass := &v1alpha1.OxideNodeClass{
		Status: v1alpha1.OxideNodeClassStatus{
			Bootstrap: &v1alpha1.BootstrapStatus{Kind: v1alpha1.BootstrapKindRKE2},
		},
	}
	if _, err := registry.For(context.Background(), nodeClass); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rke2.calls != 0 {
		t.Fatalf("the launch path re-detected an already resolved provider")
	}
}

func TestForReportsAnUndetectableCluster(t *testing.T) {
	registry := bootstrap.New(
		&fake{name: v1alpha1.BootstrapKindRKE2},
		&fake{name: v1alpha1.BootstrapKindKubeadm},
	)
	_, err := registry.For(context.Background(), &v1alpha1.OxideNodeClass{})
	if err == nil || !strings.Contains(err.Error(), "set spec.bootstrap") {
		t.Fatalf("got %v", err)
	}
}

func TestForRefusesToGuessBetweenTwoMatches(t *testing.T) {
	registry := bootstrap.New(
		&fake{name: v1alpha1.BootstrapKindRKE2, present: true},
		&fake{name: v1alpha1.BootstrapKindKubeadm, present: true},
	)
	_, err := registry.For(context.Background(), &v1alpha1.OxideNodeClass{})
	if err == nil || !strings.Contains(err.Error(), "more than one provider") {
		t.Fatalf("got %v", err)
	}
}

func TestForSurfacesADetectionFailure(t *testing.T) {
	registry := bootstrap.New(
		&fake{name: v1alpha1.BootstrapKindKubeadm, err: errors.New("forbidden")},
	)
	_, err := registry.For(context.Background(), &v1alpha1.OxideNodeClass{})
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("got %v", err)
	}
}
