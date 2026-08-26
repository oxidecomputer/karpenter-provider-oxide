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
				"storage":      map[string]any{"boot": map[string]any{"size": "50Gi"}},
				"ephemeralIPs": tt.ephemeralIPs,
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
