/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	coreapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
)

func init() {
	karpv1.RestrictedLabelDomains = karpv1.RestrictedLabelDomains.Insert(RestrictedLabelDomains...)
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Insert()
	karpv1.WellKnownResources.Insert()
}

var (
	TerminationFinalizer   = apis.Group + "/termination"
	WellKnownArchitectures = sets.NewString(
		karpv1.ArchitectureAmd64,
		karpv1.ArchitectureArm64,
	)
	WellKnownResources = sets.New(
		corev1.ResourceCPU,
		corev1.ResourceMemory,
		corev1.ResourceEphemeralStorage,
		corev1.ResourcePods,
	)
	RestrictedLabelDomains = []string{
		apis.Group,
	}

	LabelInstanceCPU = apis.Group + "/instance-cpu"
	LabelNodeClass   = apis.Group + "/oxidenodeclass"

	// Sled placement labels, written to Nodes by the optional inventory controller.
	LabelSledID          = apis.Group + "/sled-id"
	LabelSledRackID      = apis.Group + "/rack-id"
	LabelSledSerial      = apis.Group + "/sled-serial"
	LabelSledState       = apis.Group + "/sled-state"
	LabelSledPolicy      = apis.Group + "/sled-policy"
	LabelSledProvisiable = apis.Group + "/sled-provision-policy"

	// SledLabels is every label the inventory controller owns.
	SledLabels = []string{
		LabelSledID,
		LabelSledRackID,
		LabelSledSerial,
		LabelSledState,
		LabelSledPolicy,
		LabelSledProvisiable,
	}

	// Sled placement annotations.
	AnnotationSledBaseboardPart     = apis.Group + "/sled-baseboard-part"
	AnnotationSledBaseboardRevision = apis.Group + "/sled-baseboard-revision"
	AnnotationSledHardwareThreads   = apis.Group + "/sled-usable-hardware-threads"
	AnnotationSledPhysicalRAM       = apis.Group + "/sled-usable-physical-ram"
	// AnnotationSledObservedAt is when the snapshot the placement was read from was taken, not when it was written.
	AnnotationSledObservedAt = apis.Group + "/sled-observed-at"

	// ConditionTypeSledUnhealthy is set on a Node when the sled underneath it is being taken out of service.
	ConditionTypeSledUnhealthy corev1.NodeConditionType = "SledUnhealthy"

	// Reasons recorded on ConditionTypeSledUnhealthy.
	ConditionReasonSledExpunged        = "SledExpunged"
	ConditionReasonSledNonProvisonable = "SledNonProvisionable"
	ConditionReasonSledHealthy         = "SledInService"

	// SledAnnotations is every annotation the inventory controller owns, cleared alongside SledLabels.
	SledAnnotations = []string{
		AnnotationSledBaseboardPart,
		AnnotationSledBaseboardRevision,
		AnnotationSledHardwareThreads,
		AnnotationSledPhysicalRAM,
		AnnotationSledObservedAt,
	}

	// AnnotationAffinityGroups is the set of Oxide groups the instance joined at launch.
	AnnotationAffinityGroups = apis.Group + "/affinity-groups"

	// AnnotationSubnet is the ID of the subnet the instance's network interface was created in.
	AnnotationSubnet = apis.Group + "/subnet"

	// AnnotationSSHKeys is the set of SSH key IDs copied to the instance at launch, sorted and comma-separated.
	AnnotationSSHKeys = apis.Group + "/ssh-keys"

	// AnnotationEphemeralIPs is the external addresses the instance was launched with.
	AnnotationEphemeralIPs = apis.Group + "/ephemeral-ips"

	// AnnotationDiskPrefix is the prefix every disk of this launch was named with, boot disk included.
	AnnotationDiskPrefix = apis.Group + "/disk-prefix"

	// AnnotationHourlyPrice records the effective hourly launch price.
	AnnotationHourlyPrice = apis.Group + "/hourly-price"

	// AnnotationDiskSetSources is the image or snapshot each diskSet's disks were created from at launch.
	AnnotationDiskSetSources = apis.Group + "/disk-set-sources"

	AnnotationOxideNodeClassHash             = apis.Group + "/oxidenodeclass-hash"
	AnnotationClusterNameTaggedCompatability = apis.CompatibilityGroup + "/cluster-name-tagged"
	AnnotationOxideNodeClassHashVersion      = apis.Group + "/oxidenodeclass-hash-version"

	NodeClaimTagKey = coreapis.Group + "/nodeclaim"
	NameTagKey      = "Name"
	NodePoolTagKey  = karpv1.NodePoolLabelKey
	NodeClassTagKey = LabelNodeClass
)
