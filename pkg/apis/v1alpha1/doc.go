// +k8s:openapi-gen=true
// +k8s:deepcopy-gen=package,register
// +k8s:defaulter-gen=TypeMeta
// +groupName=karpenter.oxide.computer
package v1alpha1 // doc.go is discovered by codegen

import (
	corev1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
)

func init() {
	gv := schema.GroupVersion{Group: apis.Group, Version: "v1alpha1"}
	corev1.AddToGroupVersion(scheme.Scheme, gv)
	scheme.Scheme.AddKnownTypes(gv,
		&OxideNodeClass{},
		&OxideNodeClassList{},
	)
}
