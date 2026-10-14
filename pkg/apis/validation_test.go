package apis

import (
	"context"
	"testing"

	"github.com/awslabs/operatorpkg/object"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

func nodeClassSchema(t *testing.T) (*apiextensions.JSONSchemaProps, *structuralschema.Structural) {
	t.Helper()

	crd := object.Unmarshal[apiextensionsv1.CustomResourceDefinition](OxideNodeClassCRD)
	var version *apiextensionsv1.CustomResourceDefinitionVersion
	for i, candidate := range crd.Spec.Versions {
		if candidate.Storage {
			version = &crd.Spec.Versions[i]
		}
	}
	if version == nil {
		t.Fatal("the OxideNodeClass CRD declares no storage version")
	}

	internal := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		version.Schema.OpenAPIV3Schema,
		internal,
		nil,
	); err != nil {
		t.Fatalf("unable to convert the CRD schema: %v", err)
	}
	props, ok := internal.Properties["spec"]
	if !ok {
		t.Fatal("the OxideNodeClass schema has no spec")
	}
	structural, err := structuralschema.NewStructural(&props)
	if err != nil {
		t.Fatalf("the CRD schema is not structural: %v", err)
	}
	return &props, structural
}

func validate(
	t *testing.T,
	props *apiextensions.JSONSchemaProps,
	schema *structuralschema.Structural,
	spec map[string]any,
) []string {
	t.Helper()

	validator, _, err := apiextensionsvalidation.NewSchemaValidator(props)
	if err != nil {
		t.Fatalf("unable to build a schema validator: %v", err)
	}
	causes := apiextensionsvalidation.ValidateCustomResource(field.NewPath("spec"), spec, validator)

	celValidator := cel.NewValidator(schema, true, celconfig.PerCallLimit)
	if celValidator == nil {
		t.Fatal("the spec schema carries no CEL rules at all")
	}
	violations, _ := celValidator.Validate(
		context.Background(),
		field.NewPath("spec"),
		schema,
		spec,
		nil,
		celconfig.RuntimeCELCostBudget,
	)

	messages := make([]string, 0, len(causes)+len(violations))
	for _, cause := range causes {
		messages = append(messages, cause.Error())
	}
	for _, violation := range violations {
		messages = append(messages, violation.Error())
	}
	return messages
}

func TestEphemeralIPsValidation(t *testing.T) {
	tests := []struct {
		name         string
		ephemeralIPs []any
		wantValid    bool
	}{
		{
			name:         "an entry with only a version takes the silo default for it",
			ephemeralIPs: []any{map[string]any{"ipVersion": "v4"}},
			wantValid:    true,
		},
		{
			name: "an entry with only a pool allocates from it",
			ephemeralIPs: []any{
				map[string]any{"pool": map[string]any{"name": "prod-egress"}},
			},
			wantValid: true,
		},
		{
			name: "an entry may set both, and the controller checks they agree",
			ephemeralIPs: []any{
				map[string]any{
					"ipVersion": "v4",
					"pool":      map[string]any{"name": "prod-egress"},
				},
			},
			wantValid: true,
		},
		{
			name: "a dual-stack pair is two entries",
			ephemeralIPs: []any{
				map[string]any{"ipVersion": "v4"},
				map[string]any{"ipVersion": "v6"},
			},
			wantValid: true,
		},
		{

			name:         "an entry naming neither a version nor a pool is refused",
			ephemeralIPs: []any{map[string]any{}},
		},
		{
			name:         "an entry naming a family Oxide does not have is refused",
			ephemeralIPs: []any{map[string]any{"ipVersion": "v5"}},
		},
		{

			name: "a third entry is refused, since an instance can hold at most two",
			ephemeralIPs: []any{
				map[string]any{"ipVersion": "v4"},
				map[string]any{"ipVersion": "v6"},
				map[string]any{"pool": map[string]any{"name": "prod-egress"}},
			},
		},
		{

			name: "a pool reference naming both an id and a name is refused",
			ephemeralIPs: []any{
				map[string]any{
					"pool": map[string]any{
						"id":   "66666666-0000-4000-8000-000000000001",
						"name": "prod-egress",
					},
				},
			},
		},
		{
			name:         "and a pool reference naming neither is refused",
			ephemeralIPs: []any{map[string]any{"pool": map[string]any{}}},
		},
	}

	props, schema := nodeClassSchema(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			spec := map[string]any{
				"storage":        map[string]any{"boot": map[string]any{"size": "50Gi"}},
				"subnetSelector": []any{map[string]any{"vpcName": "prod"}},
				"ephemeralIPs":   tt.ephemeralIPs,
			}
			got := validate(t, props, schema, spec)
			if tt.wantValid && len(got) > 0 {
				t.Errorf("the spec was refused with %v, want it accepted", got)
			}
			if !tt.wantValid && len(got) == 0 {
				t.Error("the spec was accepted, want it refused")
			}
		})
	}
}

func TestSubnetSelectorValidation(t *testing.T) {
	tests := []struct {
		name      string
		selectors []any
		omit      bool
		wantValid bool
	}{
		{
			name:      "subnet id",
			selectors: []any{map[string]any{"id": "44444444-0000-4000-8000-000000000001"}},
			wantValid: true,
		},
		{
			name:      "vpc id",
			selectors: []any{map[string]any{"vpcID": "22222222-0000-4000-8000-000000000001"}},
			wantValid: true,
		},
		{
			name:      "scoped subnet name",
			selectors: []any{map[string]any{"name": "web", "vpcName": "prod"}},
			wantValid: true,
		},
		{name: "omitted", omit: true},
		{name: "empty list", selectors: []any{}},
		{name: "empty selector", selectors: []any{map[string]any{}}},
		{name: "unscoped subnet name", selectors: []any{map[string]any{"name": "web"}}},
		{
			name:      "both vpc locators",
			selectors: []any{map[string]any{"vpcID": "id", "vpcName": "prod"}},
		},
	}

	props, schema := nodeClassSchema(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := map[string]any{
				"storage": map[string]any{"boot": map[string]any{"size": "50Gi"}},
			}
			if !tt.omit {
				spec["subnetSelector"] = tt.selectors
			}
			got := validate(t, props, schema, spec)
			if tt.wantValid && len(got) > 0 {
				t.Errorf("the spec was refused with %v, want it accepted", got)
			}
			if !tt.wantValid && len(got) == 0 {
				t.Error("the spec was accepted, want it refused")
			}
		})
	}
}

func TestKubeletValidation(t *testing.T) {
	tests := []struct {
		name      string
		kubelet   map[string]any
		wantValid bool
	}{
		{
			name:      "an empty configuration is accepted",
			kubelet:   map[string]any{},
			wantValid: true,
		},
		{
			name: "reservations in every resource kubelet takes are accepted",
			kubelet: map[string]any{
				"systemReserved": map[string]any{
					"cpu": "500m", "memory": "1Gi", "ephemeral-storage": "2Gi", "pid": "1000",
				},
				"kubeReserved": map[string]any{"cpu": "0.25", "memory": "512Mi"},
				"evictionHard": map[string]any{
					"memory.available":   "5%",
					"nodefs.available":   "1Gi",
					"imagefs.inodesFree": "2.5%",
				},
			},
			wantValid: true,
		},
		{
			name: "the flags kubelet only acts on are accepted",
			kubelet: map[string]any{
				"clusterDNS":                  []any{"10.96.0.10"},
				"maxPods":                     60,
				"podsPerCore":                 8,
				"evictionSoft":                map[string]any{"memory.available": "500Mi"},
				"evictionSoftGracePeriod":     map[string]any{"memory.available": "1m30s"},
				"evictionMaxPodGracePeriod":   30,
				"imageGCHighThresholdPercent": 85,
				"imageGCLowThresholdPercent":  80,
				"cpuCFSQuota":                 false,
			},
			wantValid: true,
		},
		{
			name:    "a resource kubelet cannot reserve is refused",
			kubelet: map[string]any{"systemReserved": map[string]any{"gpu": "1"}},
		},
		{
			name:    "a reservation that is not a quantity is refused",
			kubelet: map[string]any{"kubeReserved": map[string]any{"memory": "lots"}},
		},
		{
			name:    "a negative reservation is refused",
			kubelet: map[string]any{"systemReserved": map[string]any{"cpu": "-1"}},
		},
		{
			name:    "an eviction signal kubelet does not have is refused",
			kubelet: map[string]any{"evictionHard": map[string]any{"swap.available": "1Gi"}},
		},
		{
			name: "an eviction threshold that is neither a quantity nor a percentage is refused",
			kubelet: map[string]any{
				"evictionHard": map[string]any{"memory.available": "10 percent"},
			},
		},
		{
			name:    "a soft threshold without a grace period is refused, since kubelet would not start",
			kubelet: map[string]any{"evictionSoft": map[string]any{"memory.available": "500Mi"}},
		},
		{
			name: "a grace period without a soft threshold is refused",
			kubelet: map[string]any{
				"evictionSoftGracePeriod": map[string]any{"memory.available": "1m"},
			},
		},
		{
			name: "an image GC low threshold at or above the high one is refused",
			kubelet: map[string]any{
				"imageGCHighThresholdPercent": 80, "imageGCLowThresholdPercent": 80,
			},
		},
		{
			name:    "a negative pod cap is refused",
			kubelet: map[string]any{"maxPods": -1},
		},
	}

	props, schema := nodeClassSchema(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := map[string]any{
				"storage":        map[string]any{"boot": map[string]any{"size": "50Gi"}},
				"subnetSelector": []any{map[string]any{"vpcName": "prod"}},
				"kubelet":        tt.kubelet,
			}
			got := validate(t, props, schema, spec)
			if tt.wantValid && len(got) > 0 {
				t.Errorf("the spec was refused with %v, want it accepted", got)
			}
			if !tt.wantValid && len(got) == 0 {
				t.Error("the spec was accepted, want it refused")
			}
		})
	}
}
