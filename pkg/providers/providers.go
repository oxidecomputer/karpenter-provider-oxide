// Package providers holds the interfaces the resource providers under it share, and nothing else.
//
// Each subpackage is everything that talks to the Oxide API about one resource: how a nodeclass's
// selectors for it are resolved, how long a resolution is worth keeping, and whether a node stops
// matching the nodeclass because of it. What is in common between them is small enough to be one
// file, and deliberately imports nothing beyond the API types so that no provider has to depend on
// its siblings -- or on pkg/cloudprovider, which consumes them.
//
// The drift reasons are here rather than beside the cloud provider that reports them so that the
// whole set can be read at once, including the one no provider owns: static drift is about the spec
// as a whole, so it belongs to none of the resources and is listed with them anyway.
package providers

import (
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Drift reasons, one per way a node can stop matching the nodeclass it came from. They are separate
// because they are the text an operator reads on the NodeClaim to find out why a node is being
// replaced, and "the nodeclass changed" is a poor answer when the nodeclass did not.
const (
	// DriftReasonNodeClass is a change to what a launch fixes into an instance and cannot revisit
	// afterwards: user data, environment, bootstrap configuration, kubelet args, boot disk size.
	// Detected by hash, since every one of those is declared on the spec and nothing outside
	// Kubernetes can move it.
	DriftReasonNodeClass cloudprovider.DriftReason = "NodeClassDrift"
	// DriftReasonImage is a node running an image the nodeclass would no longer launch, whether
	// because the selector changed or because a newer image matched the one it already had.
	DriftReasonImage cloudprovider.DriftReason = "ImageDrift"
	// DriftReasonSubnet is a node whose interface sits in a subnet the nodeclass no longer selects.
	// An interface cannot be moved between subnets, so replacing the node is the only way out of one.
	DriftReasonSubnet cloudprovider.DriftReason = "SubnetDrift"
	// DriftReasonSSHKey is a node whose authorized keys are not the ones the nodeclass now selects.
	// Keys are copied in at launch, so neither adding nor removing one reaches a running node.
	DriftReasonSSHKey cloudprovider.DriftReason = "SSHKeyDrift"
	// DriftReasonIPPool is a node whose ephemeral external addresses do not come from the pools the
	// nodeclass now selects.
	DriftReasonIPPool cloudprovider.DriftReason = "IPPoolDrift"
	// DriftReasonAffinityGroup is reported when a node is not in a group its nodeclass now says every
	// instance should be in. Replacing it is the only way it can join, since a group is joined at launch.
	DriftReasonAffinityGroup cloudprovider.DriftReason = "AffinityGroup"
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
