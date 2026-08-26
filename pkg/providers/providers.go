// Package providers defines provider interfaces.
package providers

import (
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Drift reasons, one per way a node can stop matching the nodeclass it came from.
const (
	// DriftReasonNodeClass is a change to what a launch fixes into an instance and cannot revisit afterwards.
	DriftReasonNodeClass cloudprovider.DriftReason = "NodeClassDrift"
	// DriftReasonImage is a node running an image the nodeclass would no longer launch.
	DriftReasonImage cloudprovider.DriftReason = "ImageDrift"
	// DriftReasonDiskSetSource identifies changed data disk sources.
	DriftReasonDiskSetSource cloudprovider.DriftReason = "DiskSetSourceDrift"
	// DriftReasonSubnet is a node whose interface sits in a subnet the nodeclass no longer selects.
	DriftReasonSubnet cloudprovider.DriftReason = "SubnetDrift"
	// DriftReasonSSHKey is a node whose authorized keys are not the ones the nodeclass now selects.
	DriftReasonSSHKey cloudprovider.DriftReason = "SSHKeyDrift"
	// DriftReasonEphemeralIP identifies changed IP pools.
	DriftReasonEphemeralIP cloudprovider.DriftReason = "EphemeralIPDrift"
	// DriftReasonAffinityGroup identifies missing group membership.
	DriftReasonAffinityGroup cloudprovider.DriftReason = "AffinityGroup"
)

// Drifter reports whether one resource makes a NodeClaim drift from its nodeclass.
type Drifter interface {
	DriftReason(*karpv1.NodeClaim, *v1alpha1.OxideNodeClass) cloudprovider.DriftReason
}

// The cadences every resource provider is timed against.
const (
	// ResolveInterval is how often a nodeclass's selectors are re-evaluated.
	ResolveInterval = 5 * time.Minute

	// CoalesceWindow is how long a provider serves a resolution before making it again.
	CoalesceWindow = ResolveInterval / 5

	// StaleLimit is how old the last good reading may be before a provider stops serving it.
	StaleLimit = 3 * ResolveInterval
)
