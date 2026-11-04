package framework

import (
	"context"
	"fmt"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/selection"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const pauseImage = "registry.k8s.io/pause:3.10"

// NodeClass builds a boot-disk-only nodeclass with the cluster's defaults filled in.
func (e *Env) NodeClass(
	test string, mutate ...func(*v1alpha1.OxideNodeClass),
) *v1alpha1.OxideNodeClass {
	nc := &v1alpha1.OxideNodeClass{
		Name: e.Name(test), Labels: e.Labels(),
		Spec: v1alpha1.OxideNodeClassSpec{
			ImageSelector:  []v1alpha1.ImageSelector{e.Image},
			SubnetSelector: e.Subnets,
			SSHKeySelector: []v1alpha1.SSHKeySelector{
				{Name: selection.WildcardSelector(e.SSHKeyName)},
			},
			Storage: v1alpha1.Storage{
				Boot: v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
			},
		},
	}
	if e.EphemeralIP {
		nc.Spec.EphemeralIPs = []v1alpha1.EphemeralIP{{IPVersion: "v4"}}
	}
	for _, m := range mutate {
		m(nc)
	}
	return nc
}

// NodePool builds a pool for the nodeclass. Requirements and startup taints come from
// the cluster's default pool so the node joins the same way the cluster's own nodes do.
// Consolidation and expiry are off: the test decides when the node goes away. The pool
// is tainted with the run so neither other workloads nor another pool's disruption
// can land anything on it.
func (e *Env) NodePool(nc *v1alpha1.OxideNodeClass) *karpv1.NodePool {
	requirements := []karpv1.NodeSelectorRequirementWithMinValues{
		{
			Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn,
			Values: []string{karpv1.ArchitectureAmd64},
		},
		{
			Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn,
			Values: []string{string(corev1.Linux)},
		},
	}
	var startupTaints []corev1.Taint
	if e.defaultPool != nil {
		requirements = e.defaultPool.Spec.Template.Spec.Requirements
		startupTaints = e.defaultPool.Spec.Template.Spec.StartupTaints
	}
	return &karpv1.NodePool{
		Name: nc.Name, Labels: e.Labels(),
		Spec: karpv1.NodePoolSpec{
			Disruption: karpv1.Disruption{
				ConsolidationPolicy: karpv1.ConsolidationPolicyWhenEmpty,
				ConsolidateAfter:    karpv1.MustParseNillableDuration("Never"),
			},
			Template: karpv1.NodeClaimTemplate{
				Spec: karpv1.NodeClaimTemplateSpec{
					ExpireAfter: karpv1.MustParseNillableDuration("Never"),
					NodeClassRef: &karpv1.NodeClassReference{
						Group: apis.Group, Kind: "OxideNodeClass", Name: nc.Name,
					},
					Requirements:  requirements,
					StartupTaints: startupTaints,
					Taints:        []corev1.Taint{e.taint()},
				},
			},
		},
	}
}

// Pod builds a pause pod pinned to the pool. resources is "cpu=1,memory=256Mi".
func (e *Env) Pod(pool *karpv1.NodePool, resources string) *corev1.Pod {
	requests := corev1.ResourceList{}
	for kv := range strings.SplitSeq(resources, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			panic(fmt.Sprintf("resources %q: want key=value pairs", resources))
		}
		requests[corev1.ResourceName(key)] = resource.MustParse(value)
	}
	labels := e.Labels()
	labels[LabelPool] = pool.Name
	return &corev1.Pod{
		Name: pool.Name, Namespace: e.Namespace, Labels: labels,
		Spec: corev1.PodSpec{
			NodeSelector:                  map[string]string{karpv1.NodePoolLabelKey: pool.Name},
			Tolerations:                   []corev1.Toleration{e.toleration()},
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			Containers: []corev1.Container{{
				Name:      "pause",
				Image:     pauseImage,
				Resources: corev1.ResourceRequirements{Requests: requests},
			}},
		},
	}
}

func (e *Env) taint() corev1.Taint {
	return corev1.Taint{Key: LabelRun, Value: e.Run, Effect: corev1.TaintEffectNoSchedule}
}

func (e *Env) toleration() corev1.Toleration {
	return corev1.Toleration{
		Key: LabelRun, Operator: corev1.TolerationOpEqual, Value: e.Run,
		Effect: corev1.TaintEffectNoSchedule,
	}
}

// loadDefaultPool reads the cluster's default nodepool, if any, for NodePool to copy.
func (e *Env) loadDefaultPool(ctx context.Context) error {
	var pool karpv1.NodePool
	err := e.Kube.Get(ctx, types.NamespacedName{Name: "default"}, &pool)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the default NodePool: %w", err)
	}
	e.defaultPool = &pool
	return nil
}
