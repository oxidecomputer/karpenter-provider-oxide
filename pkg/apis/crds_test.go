package apis

import (
	"context"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
)

// The API server rejects a CRD whose CEL rules it estimates as too costly, and
// nothing short of its own validator models that estimate.
func TestCRDsAreAdmissible(t *testing.T) {
	for _, crd := range CRDs {
		t.Run(crd.Name, func(t *testing.T) {
			internal := &apiextensions.CustomResourceDefinition{}
			if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(
				crd.DeepCopy(),
				internal,
				nil,
			); err != nil {
				t.Fatalf("unable to convert the CRD: %v", err)
			}
			// The server fills this in on create, and validation requires it.
			for _, version := range internal.Spec.Versions {
				if version.Storage {
					internal.Status.StoredVersions = []string{version.Name}
				}
			}
			for _, err := range crdvalidation.ValidateCustomResourceDefinition(context.Background(), internal) {
				t.Error(err)
			}
		})
	}
}
