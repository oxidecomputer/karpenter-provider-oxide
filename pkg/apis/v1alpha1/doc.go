// +k8s:openapi-gen=true
// +k8s:deepcopy-gen=package,register
// +k8s:defaulter-gen=TypeMeta
// +groupName=karpenter.oxide.computer
package v1alpha1 // doc.go is discovered by codegen

import (
	corev1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
)

// GroupVersion is the group and version every type in this package belongs to.
var GroupVersion = schema.GroupVersion{Group: apis.Group, Version: "v1alpha1"}

// AddToScheme registers this package's types with a scheme.
//
// The init below does this to the client-go global scheme, which is the one Karpenter's operator
// builds its manager client from, so the controller needs no wiring. This exists for callers building
// a scheme of their own, which in practice means tests.
func AddToScheme(s *runtime.Scheme) error {
	corev1.AddToGroupVersion(s, GroupVersion)
	s.AddKnownTypes(GroupVersion,
		&OxideNodeClass{},
		&OxideNodeClassList{},
		&OxideCapacity{},
		&OxideCapacityList{},
	)
	return nil
}

func init() {
	if err := AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}
}
