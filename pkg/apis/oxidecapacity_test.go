package apis_test

import (
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/object"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

func TestCapacityStatusRejectsContradictoryStatesAndScopes(t *testing.T) {
	crd := object.Unmarshal[apiextensionsv1.CustomResourceDefinition](apis.OxideCapacityCRD)
	root := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, root, nil); err != nil {
		t.Fatal(err)
	}
	props := root.Properties["status"]
	schema, err := structuralschema.NewStructural(&props)
	if err != nil {
		t.Fatal(err)
	}
	validator, _, err := apiextensionsvalidation.NewSchemaValidator(&props)
	if err != nil {
		t.Fatal(err)
	}
	celValidator := cel.NewValidator(schema, true, celconfig.PerCallLimit)
	for _, tc := range []struct {
		name   string
		change func(*v1alpha1.OxideCapacityStatus)
		valid  bool
	}{
		{"global backoff", func(*v1alpha1.OxideCapacityStatus) {}, true},
		{"platform scope", func(s *v1alpha1.OxideCapacityStatus) {
			s.LaunchBackoffs[0].Scope.CPUPlatforms = []string{"amd_milan"}
		}, true},
		{"placement scope", func(s *v1alpha1.OxideCapacityStatus) {
			s.LaunchBackoffs[0].Scope = v1alpha1.CapacityScope{Type: v1alpha1.CapacityScopePlacement,
				Placement: []v1alpha1.CapacityPlacementConstraint{{Type: "AntiAffinity", ID: "group"}}}
		}, true},
		{"global scope with groups", func(s *v1alpha1.OxideCapacityStatus) {
			s.LaunchBackoffs[0].Scope.Placement = []v1alpha1.CapacityPlacementConstraint{{Type: "Affinity", ID: "group"}}
		}, false},
		{"placement scope without groups", func(s *v1alpha1.OxideCapacityStatus) {
			s.LaunchBackoffs[0].Scope.Type = v1alpha1.CapacityScopePlacement
		}, false},
		{"waiting without retry time", func(s *v1alpha1.OxideCapacityStatus) {
			s.LaunchBackoffs[0].RetryAfter = nil
		}, false},
		{"probe with retry time", func(s *v1alpha1.OxideCapacityStatus) {
			s.LaunchBackoffs[0].State = v1alpha1.BackoffProbing
		}, false},
		{"probe eligible", func(s *v1alpha1.OxideCapacityStatus) {
			s.LaunchBackoffs[0].State = v1alpha1.BackoffProbeEligible
			s.LaunchBackoffs[0].RetryAfter = nil
		}, true},
		{"zero sized boundary", func(s *v1alpha1.OxideCapacityStatus) {
			s.LaunchBackoffs[0].MinimumShape.CPUs = 0
		}, false},
		{"unknown quota with headroom", func(s *v1alpha1.OxideCapacityStatus) {
			s.Quota.Remaining = &v1alpha1.QuotaHeadroom{}
		}, false},
		{"known quota without observation", func(s *v1alpha1.OxideCapacityStatus) {
			s.Quota.State = v1alpha1.QuotaFresh
		}, false},
		{"unavailable exceeds total", func(s *v1alpha1.OxideCapacityStatus) {
			s.GlobalInstanceTypes.UnavailableCount = 2
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := metav1.NewTime(time.Now())
			status := v1alpha1.OxideCapacityStatus{
				GlobalInstanceTypes: v1alpha1.GlobalInstanceTypeSummary{
					Count:            1,
					UnavailableCount: 1,
				},
				Quota: v1alpha1.QuotaStatus{State: v1alpha1.QuotaUnknown},
				LaunchBackoffs: []v1alpha1.LaunchBackoff{{
					Scope:            v1alpha1.CapacityScope{Type: v1alpha1.CapacityScopeGlobal},
					MinimumShape:     v1alpha1.CapacityShape{CPUs: 8, MemoryGiB: 16},
					LastRefusedShape: v1alpha1.CapacityShape{CPUs: 8, MemoryGiB: 16},
					State:            v1alpha1.BackoffWaiting, RetryAfter: &now, LastRefusal: &now,
					Reason: "InsufficientCapacity", ConsecutiveRefusals: 1,
				}},
			}
			tc.change(&status)
			data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
			if err != nil {
				t.Fatal(err)
			}
			violations := apiextensionsvalidation.ValidateCustomResource(
				field.NewPath("status"),
				data,
				validator,
			)
			celViolations, _ := celValidator.Validate(
				t.Context(),
				field.NewPath("status"),
				schema,
				data,
				nil,
				celconfig.RuntimeCELCostBudget,
			)
			violations = append(violations, celViolations...)
			if got := len(violations) == 0; got != tc.valid {
				t.Fatalf("valid = %v, want %v: %v", got, tc.valid, violations)
			}
		})
	}
}
