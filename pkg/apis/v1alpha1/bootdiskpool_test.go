package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBootDiskPoolDefaults(t *testing.T) {
	boot := BootDisk{}
	if !boot.PoolEnabled() {
		t.Fatal("pool is disabled by default")
	}
	if boot.PoolMinReady() != 2 || boot.PoolPercentage() != 10 || boot.PoolMaxReady() != 20 {
		t.Fatalf(
			"defaults = %d/%d/%d, want 2/10/20",
			boot.PoolMinReady(),
			boot.PoolPercentage(),
			boot.PoolMaxReady(),
		)
	}
}

func TestBootDiskPoolDoesNotCauseDrift(t *testing.T) {
	a := &OxideNodeClass{Spec: OxideNodeClassSpec{
		Storage: Storage{Boot: BootDisk{Size: resource.MustParse("20Gi")}},
	}}
	b := a.DeepCopy()
	minReady := int32(9)
	b.Spec.Storage.Boot.Pool = &BootDiskPool{MinReady: &minReady}
	if a.Hash() != b.Hash() {
		t.Fatalf("pool settings changed the NodeClass hash")
	}
}
