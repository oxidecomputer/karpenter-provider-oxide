// Package providers holds the interfaces the resource providers under it share, and nothing else.
//
// Each subpackage is everything that talks to the Oxide API about one resource: how a nodeclass's
// selectors for it are resolved, how long a resolution is worth keeping, and whether a node stops
// matching the nodeclass because of it. What is in common between them is small enough to be one
// file, and deliberately imports nothing beyond the API types so that no provider has to depend on
// its siblings.
package providers

import (
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Drifter reports whether one resource makes a NodeClaim drift from its nodeclass.
//
// Implementations compare the record the launch left behind against the nodeclass's published
// status; none of them call the Oxide API. Drift lives next to resolution because the two are the
// same claim from opposite ends -- a subnet added to the selected set drifts nothing, an affinity
// group removed from it drifts nothing -- and those asymmetries only make sense beside the code that
// produces the set they are about.
type Drifter interface {
	DriftReason(*karpv1.NodeClaim, *v1alpha1.OxideNodeClass) cloudprovider.DriftReason
}
