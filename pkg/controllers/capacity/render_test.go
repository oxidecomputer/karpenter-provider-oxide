package capacity

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/object"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

func assertValidStatus(t *testing.T, status v1alpha1.OxideCapacityStatus) {
	t.Helper()
	crd := object.Unmarshal[apiextensionsv1.CustomResourceDefinition](apis.OxideCapacityCRD)
	props := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, props, nil); err != nil {
		t.Fatal(err)
	}
	statusProps := props.Properties["status"]
	schema, err := structuralschema.NewStructural(&statusProps)
	if err != nil {
		t.Fatal(err)
	}
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		t.Fatal(err)
	}
	before := runtime.DeepCopyJSONValue(data)
	pruning.Prune(data, schema, false)
	if !reflect.DeepEqual(data, before) {
		t.Fatal("the CRD schema prunes published fields")
	}
	validator, _, err := apiextensionsvalidation.NewSchemaValidator(&statusProps)
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range apiextensionsvalidation.ValidateCustomResource(field.NewPath("status"), data, validator) {
		t.Error(violation)
	}
	violations, _ := cel.NewValidator(schema, true, celconfig.PerCallLimit).Validate(
		t.Context(), field.NewPath("status"), schema, data, nil, celconfig.RuntimeCELCostBudget)
	for _, violation := range violations {
		t.Error(violation)
	}
	for _, obsolete := range []string{"instanceTypes", "placementRefusals", "storage"} {
		if _, exists := data[obsolete]; exists {
			t.Errorf("published obsolete field %s", obsolete)
		}
	}
}

func TestBackoffReportsAffectedPlatformsAndProbeLifecycle(t *testing.T) {
	c, _, manager := newController(t, nil)
	shape := availability.Shape{Ncpus: 16, MemInGiB: 32}
	platform := availability.Platform("amd_turin")
	manager.SeedLaunchable(t.Context(), availability.Shape{Ncpus: 4, MemInGiB: 8}, platform)
	manager.RecordICE(t.Context(), shape, platform, availability.ReasonCapacity)
	status := c.render(c.observe())
	assertValidStatus(t, status)
	backoff := status.LaunchBackoffs[0]
	if backoff.Scope.Type != v1alpha1.CapacityScopeGlobal ||
		!slices.Equal(backoff.Scope.CPUPlatforms, []string{"amd_turin", "amd_turin_v2"}) {
		t.Fatalf("wrong affected platforms: %+v", backoff.Scope)
	}
	if backoff.KnownLaunchableShape == nil || backoff.KnownLaunchableShape.CPUs != 4 {
		t.Fatalf("missing launch evidence: %+v", backoff)
	}
	if backoff.State != v1alpha1.BackoffWaiting || backoff.RetryAfter == nil {
		t.Fatalf("missing backoff: %+v", backoff)
	}
	manager.RecordCapacityReleased(t.Context(), shape, platform)
	eligible := c.observe()
	status = c.render(eligible)
	assertValidStatus(t, status)
	if status.LaunchBackoffs[0].State != v1alpha1.BackoffProbeEligible ||
		status.LaunchBackoffs[0].RetryAfter != nil {
		t.Fatalf("released capacity did not reopen the backoff: %+v", status.LaunchBackoffs[0])
	}
	probe, ok := manager.Acquire(availability.Request{Shape: shape, Platform: platform})
	if !ok {
		t.Fatal("reopened backoff did not admit a probe")
	}
	defer probe.Release()
	status = c.render(c.observe())
	assertValidStatus(t, status)
	if status.LaunchBackoffs[0].State != v1alpha1.BackoffProbing {
		t.Fatal("reserved probe was reported as eligible")
	}
	if c.render(eligible).LaunchBackoffs[0].State != v1alpha1.BackoffProbeEligible {
		t.Fatal("taking a probe mutated a snapshot")
	}
	probe.RecordSuccess(t.Context())
	if got := c.render(c.observe()).LaunchBackoffs; len(got) != 0 {
		t.Fatalf("success left backoffs: %+v", got)
	}
}

func TestPlacementHistoryAndBackoffsDoNotReduceTheGlobalSummary(t *testing.T) {
	c, _, manager := newController(t, nil)
	placement := availability.NewPlacement(
		availability.Constraint{Kind: v1alpha1.AffinityGroupTypeAntiAffinity, ID: "spread"},
		availability.Constraint{Kind: v1alpha1.AffinityGroupTypeAffinity, ID: "together"},
	)
	manager.RecordPlacementRefusal(
		t.Context(),
		placement,
		availability.Shape{Ncpus: 4, MemInGiB: 8},
		"placement denied",
	)
	status := c.render(c.observe())
	assertValidStatus(t, status)
	if len(status.LaunchBackoffs) != 0 || status.History == nil ||
		len(status.History.Placement) != 1 {
		t.Fatalf("a first refusal must only appear in history: %+v", status)
	}
	manager.RecordPlacementRefusal(
		t.Context(),
		placement,
		availability.Shape{Ncpus: 8, MemInGiB: 16},
		"placement denied again",
	)
	status = c.render(c.observe())
	assertValidStatus(t, status)
	if len(status.LaunchBackoffs) != 1 || status.GlobalInstanceTypes.UnavailableCount != 0 {
		t.Fatalf("placement backoff affected the global catalog: %+v", status)
	}
	backoff := status.LaunchBackoffs[0]
	want := []v1alpha1.CapacityPlacementConstraint{
		{Type: v1alpha1.AffinityGroupTypeAffinity, ID: "together"},
		{Type: v1alpha1.AffinityGroupTypeAntiAffinity, ID: "spread"},
	}
	if backoff.Scope.Type != v1alpha1.CapacityScopePlacement ||
		!slices.Equal(backoff.Scope.Placement, want) ||
		len(backoff.Scope.CPUPlatforms) != 0 {
		t.Fatalf("incorrect placement scope: %+v", backoff.Scope)
	}
	if backoff.State != v1alpha1.BackoffWaiting || backoff.RetryAfter == nil {
		t.Fatalf("missing placement gate: %+v", backoff)
	}
	manager.RecordPlacementSuccess(
		t.Context(),
		placement,
		availability.Shape{Ncpus: 8, MemInGiB: 16},
	)
	status = c.render(c.observe())
	if status.History != nil || len(status.LaunchBackoffs) != 0 {
		t.Fatalf("success did not clear placement evidence: %+v", status)
	}
}

func TestQuotaSeparatesUnknownFromFreshAndStaleZeroHeadroom(t *testing.T) {
	c, _, _ := newController(t, nil)
	for _, tc := range []struct {
		name    string
		reading utilization.Snapshot
		state   string
	}{
		{"unread", utilization.Snapshot{}, v1alpha1.QuotaUnknown},
		{"exhausted", utilization.Snapshot{Known: true, TakenAt: time.Now()}, v1alpha1.QuotaFresh},
		{"failed refresh", utilization.Snapshot{
			Known: true, TakenAt: time.Now().Add(-time.Hour), Stale: true,
		}, v1alpha1.QuotaStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := c.observe()
			view.Availability = view.Availability.WithQuota(tc.reading)
			status := c.render(view)
			assertValidStatus(t, status)
			if status.Quota.State != tc.state ||
				(status.Quota.Remaining != nil) != tc.reading.Known {
				t.Fatalf("ambiguous quota observation: %+v", status.Quota)
			}
		})
	}
}
