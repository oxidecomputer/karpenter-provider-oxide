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
	//
	// These describe where a node physically landed, which the scheduler does not know and cannot
	// ask for: an instance is placed by the provision saga after the create call returns. So they are
	// deliberately not offering requirements and nothing selects on them at provisioning time. They
	// exist to be read after the fact, whether by an operator, by a metrics pipeline joining nodes to
	// hardware, or by a pod topology spread that wants sleds as its failure domain.
	LabelSledID          = apis.Group + "/sled-id"
	LabelSledRackID      = apis.Group + "/rack-id"
	LabelSledSerial      = apis.Group + "/sled-serial"
	LabelSledState       = apis.Group + "/sled-state"
	LabelSledPolicy      = apis.Group + "/sled-policy"
	LabelSledProvisiable = apis.Group + "/sled-provision-policy"

	// SledLabels is every label the inventory controller owns. It is the set the controller removes
	// from a node it can no longer place, so that a stale sled is never left asserted on a node.
	SledLabels = []string{
		LabelSledID,
		LabelSledRackID,
		LabelSledSerial,
		LabelSledState,
		LabelSledPolicy,
		LabelSledProvisiable,
	}

	// Sled placement annotations. These carry the detail that is either too long, too structured or
	// too numeric to be worth selecting on, so it is recorded rather than made queryable.
	AnnotationSledBaseboardPart     = apis.Group + "/sled-baseboard-part"
	AnnotationSledBaseboardRevision = apis.Group + "/sled-baseboard-revision"
	AnnotationSledHardwareThreads   = apis.Group + "/sled-usable-hardware-threads"
	AnnotationSledPhysicalRAM       = apis.Group + "/sled-usable-physical-ram"
	// AnnotationSledObservedAt is when the snapshot the placement was read from was taken, not when it
	// was written. A reader needs to know how old the underlying observation is, and a node whose
	// placement has not changed is not re-patched, so the write time would say nothing useful.
	AnnotationSledObservedAt = apis.Group + "/sled-observed-at"

	// ConditionTypeSledUnhealthy is set on a Node when the sled underneath it is being taken out of
	// service. True means the sled will not host this node much longer; False means it is fine.
	//
	// This is a Node status condition rather than a label because that is the only thing Karpenter's
	// node repair watches: RepairPolicies is expressed as a condition type and status, so a label
	// could never trigger remediation no matter what it said. It is set whenever inventory is enabled,
	// and only ever acted on when the NodeRepair feature gate is also on.
	ConditionTypeSledUnhealthy corev1.NodeConditionType = "SledUnhealthy"

	// Reasons recorded on ConditionTypeSledUnhealthy. Expunged and NonProvisionable are the two states
	// worth replacing a node over, and they are distinguished so an operator can tell a sled being
	// removed from the rack from one merely closed to new placements.
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

	// AnnotationAffinityGroups is the set of Oxide groups the instance joined at launch, as
	// "<aa|af>:<uuid>" entries, sorted and comma-separated.
	//
	// It is the record drift is decided against, and the only handle on a membership once the group
	// has left the nodeclass's desired set: by then the nodeclass no longer says which kind of group
	// it was, so the type prefix is carried here to address the right endpoint to leave it on.
	AnnotationAffinityGroups = apis.Group + "/affinity-groups"

	// AnnotationSubnet is the ID of the subnet the instance's network interface was created in.
	//
	// One subnet, not the resolved set: a launch picks one of the selected subnets, so the set says
	// what could have been used and only this says what was. Drift compares it back against the set,
	// which is what makes a subnet leaving the set replace the nodes in it while a subnet joining the
	// set replaces nothing.
	AnnotationSubnet = apis.Group + "/subnet"

	// AnnotationSSHKeys is the set of SSH key IDs copied to the instance at launch, sorted and
	// comma-separated.
	//
	// Recorded by ID rather than by name because a key deleted and recreated under the same name
	// carries different public key material, which is exactly the change worth replacing a node over.
	AnnotationSSHKeys = apis.Group + "/ssh-keys"

	// AnnotationIPPools is the set of IP pool IDs the instance's ephemeral external IPs were
	// allocated from at launch, sorted and comma-separated. One IP is attached per resolved pool.
	AnnotationIPPools = apis.Group + "/ip-pools"

	AnnotationOxideNodeClassHash             = apis.Group + "/oxidenodeclass-hash"
	AnnotationClusterNameTaggedCompatability = apis.CompatibilityGroup + "/cluster-name-tagged"
	AnnotationOxideNodeClassHashVersion      = apis.Group + "/oxidenodeclass-hash-version"

	NodeClaimTagKey = coreapis.Group + "/nodeclaim"
	NameTagKey      = "Name"
	NodePoolTagKey  = karpv1.NodePoolLabelKey
	NodeClassTagKey = LabelNodeClass
)
