package cloudprovider

import (
	"context"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	karpoptions "sigs.k8s.io/karpenter/pkg/operator/options"
)

type fakeInstanceTypeOverlayStore struct {
	effective *karpcloudprovider.InstanceType
	pool      string
	called    bool
}

func (f *fakeInstanceTypeOverlayStore) Apply(
	pool string,
	_ *karpcloudprovider.InstanceType,
) (*karpcloudprovider.InstanceType, error) {
	f.called = true
	f.pool = pool
	return f.effective, nil
}

func TestEffectiveInstanceTypeUsesNodeOverlayStore(t *testing.T) {
	instanceTypes := instancetype.NewProvider(nil, availability.New(nil))
	base := instanceTypes.InstanceTypes(instancetype.Options{})[0]
	effective := base.DeepCopy()
	effective.Offerings[0].ApplyPriceOverlay("0.42")
	store := &fakeInstanceTypeOverlayStore{effective: effective}
	provider := &OxideCloudProvider{
		instanceTypeProvider: instanceTypes,
		instanceTypeStore:    store,
	}
	ctx := karpoptions.ToContext(context.Background(), &karpoptions.Options{
		FeatureGates: karpoptions.FeatureGates{NodeOverlay: true},
	})
	nodeClaim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		karpv1.NodePoolLabelKey: "priced-pool",
	}}}

	got, err := provider.effectiveInstanceType(ctx, nodeClaim, base.Name, instancetype.Options{})
	if err != nil {
		t.Fatalf("effectiveInstanceType() error = %v", err)
	}
	if !store.called || store.pool != "priced-pool" {
		t.Fatalf(
			"overlay store call = (%v, %q), want (true, %q)",
			store.called,
			store.pool,
			"priced-pool",
		)
	}
	if price := got.Offerings[0].Price; price != 0.42 {
		t.Fatalf("effective price = %v, want 0.42", price)
	}
}

func TestAnnotateHourlyPriceUsesResolvedOffering(t *testing.T) {
	base := instancetype.NewProvider(nil, availability.New(nil)).InstanceTypes(instancetype.Options{})[0]
	instanceType := base.DeepCopy()
	instanceType.Offerings[0].Price = 0.42
	nodeClaim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		corev1.LabelTopologyZone:    base.Offerings[0].Zone(),
		karpv1.CapacityTypeLabelKey: base.Offerings[0].CapacityType(),
	}}}

	annotateHourlyPrice(nodeClaim, instanceType)

	if got := nodeClaim.Annotations[v1alpha1.AnnotationHourlyPrice]; got != "0.42" {
		t.Fatalf("hourly price annotation = %q, want %q", got, "0.42")
	}
}

func TestAnnotateHourlyPriceOmitsUnknownOffering(t *testing.T) {
	base := instancetype.NewProvider(nil, availability.New(nil)).InstanceTypes(instancetype.Options{})[0]
	nodeClaim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		corev1.LabelTopologyZone:    "another-rack",
		karpv1.CapacityTypeLabelKey: base.Offerings[0].CapacityType(),
	}}}

	annotateHourlyPrice(nodeClaim, base)

	if _, ok := nodeClaim.Annotations[v1alpha1.AnnotationHourlyPrice]; ok {
		t.Fatal("hourly price annotation was published for an unknown offering")
	}
}
