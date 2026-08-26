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
	"time"

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
	// DriftReasonDiskSetSource is a node whose data disks were created from an image or a snapshot the
	// nodeclass would no longer create them from, whether because the selector changed or because a
	// newer source matched the one it already had.
	//
	// Separate from DriftReasonImage, which is about the boot disk: a data disk re-sourced is not a
	// node running different software, and reporting the two the same way would tell an operator the
	// wrong thing about why a node is being replaced.
	DriftReasonDiskSetSource cloudprovider.DriftReason = "DiskSetSourceDrift"
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

// The cadences every resource provider is timed against.
//
// There is one number here, and the other two are derived from it. Freshness is decided by how often
// the nodeclass controller re-resolves, not by how long a cache holds a reading: a cache that
// outlives the resync is not a cache, it is a second and longer poll interval that nothing names.
// Keeping the derivation in one place is what stops the two from drifting apart again.
const (
	// ResolveInterval is how often a nodeclass's selectors are re-evaluated, and so the only thing
	// that decides how soon a newly uploaded image, a new subnet, or a rotated SSH key is picked up.
	// The nodeclass reconcile is the sole loop, so this is the poll interval and the resync both.
	ResolveInterval = 5 * time.Minute

	// CoalesceWindow is how long a provider serves a resolution before making it again.
	//
	// Deliberately a fraction of ResolveInterval rather than a multiple of it, because this is not a
	// freshness knob: it exists to collapse the reads that arrive together -- several nodeclasses
	// carrying the same selectors, and the extra reconcile a status patch's own watch event provokes
	// -- into one Oxide API call. Every resync falls outside the window and refreshes, which is what
	// makes ResolveInterval mean what it says.
	CoalesceWindow = ResolveInterval / 5

	// StaleLimit is how old the last good reading may be before a provider stops serving it and
	// returns the refresh failure instead.
	//
	// Pinned to ResolveInterval rather than to CoalesceWindow: what it bounds is how many resyncs in
	// a row may fail before a nodeclass is taken un-Ready, and that has nothing to do with how
	// tightly concurrent reads are collapsed. Three resyncs reads as a real outage rather than as a
	// blip, while a multiple of the coalescing window would give up inside a single resync.
	StaleLimit = 3 * ResolveInterval
)
