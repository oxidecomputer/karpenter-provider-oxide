// Package node registers synthetic kubelets without changing the production instance lifecycle.
package node

import (
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	managedValue    = "true"
	simulationLabel = "kwok.x-k8s.io/node"
	simulationValue = "fake"
	claimKind       = "NodeClaim"
	claimAPIVersion = "karpenter.sh/v1"
	stateRunning    = "running"
	stateMissing    = "missing"
	stateUnknown    = "unknown"
	stateTerminal   = "terminal"
	Managed         = "kwok.oxide.computer/managed"
	Cluster         = "kwok.oxide.computer/cluster"
	ClaimUID        = "kwok.oxide.computer/claim-uid"
	InstanceID      = "kwok.oxide.computer/instance-id"
	BackendState    = "kwok.oxide.computer/backend-state"
	ObservedAt      = "kwok.oxide.computer/observed-at"
)

const ResourcesInitialized = "kwok.oxide.computer/resources-initialized"

func complete(claim *karpv1.NodeClaim) bool {
	return completeResources(claim.Status.Capacity, claim.Status.Allocatable)
}

func completeResources(capacity, allocatable corev1.ResourceList) bool {
	for _, key := range []corev1.ResourceName{
		corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourcePods,
		corev1.ResourceEphemeralStorage,
	} {
		c, cok := capacity[key]
		a, aok := allocatable[key]
		if !cok || !aok || c.Sign() <= 0 || a.Sign() <= 0 || a.Cmp(c) > 0 {
			return false
		}
	}
	return true
}

// Project only copies persisted launch output. It deliberately supplies no Ready condition.
func Project(claim *karpv1.NodeClaim, cluster types.UID, instanceID, version string) *corev1.Node {
	labels := maps.Clone(claim.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	for key := range labels {
		if strings.HasPrefix(key, "kwok.oxide.computer/") {
			delete(labels, key)
		}
	}
	name := "karpenter-" + claim.Name
	labels[corev1.LabelHostname] = name
	labels[Managed], labels[simulationLabel] = managedValue, simulationValue
	labels[BackendState] = stateRunning
	taints := []corev1.Taint{}
	for _, ts := range [][]corev1.Taint{claim.Spec.Taints, claim.Spec.StartupTaints, {karpv1.UnregisteredNoExecuteTaint}} {
		for _, t := range ts {
			found := false
			for _, existing := range taints {
				if existing.Key == t.Key && existing.Effect == t.Effect {
					found = true
					break
				}
			}
			if !found {
				taints = append(taints, t)
			}
		}
	}
	return &corev1.Node{
		Name:   name,
		Labels: labels,
		Annotations: map[string]string{
			Cluster:    string(cluster),
			ClaimUID:   string(claim.UID),
			InstanceID: instanceID,
		},
		OwnerReferences: []metav1.OwnerReference{
			{
				APIVersion: claimAPIVersion,
				Kind:       claimKind,
				Name:       claim.Name,
				UID:        claim.UID,
			},
		},
		Spec: corev1.NodeSpec{ProviderID: claim.Status.ProviderID, Taints: taints},
		Status: corev1.NodeStatus{
			Capacity:    claim.Status.Capacity.DeepCopy(),
			Allocatable: claim.Status.Allocatable.DeepCopy(),
			Addresses:   []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: name}},
			NodeInfo: corev1.NodeSystemInfo{
				MachineID:               instanceID,
				SystemUUID:              instanceID,
				BootID:                  instanceID,
				OperatingSystem:         labels[corev1.LabelOSStable],
				Architecture:            labels[corev1.LabelArchStable],
				OSImage:                 "Oxide KWOK (simulated)",
				KernelVersion:           "kwok",
				ContainerRuntimeVersion: "containerd://kwok",
				KubeletVersion:          version,
			},
		},
	}
}
