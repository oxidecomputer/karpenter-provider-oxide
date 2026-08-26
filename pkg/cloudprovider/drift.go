package cloudprovider

import (
	"context"
	"fmt"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/samber/lo"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// IsDrifted returns whether a NodeClaim has drifted from the provisioning requirements it is tied to.
func (c *OxideCloudProvider) IsDrifted(
	ctx context.Context,
	nodeClaim *karpcorev1.NodeClaim,
) (cloudprovider.DriftReason, error) {
	nodeClass := &v1alpha1.OxideNodeClass{}
	if err := c.kubeClient.Get(
		ctx,
		client.ObjectKey{Name: nodeClaim.Spec.NodeClassRef.Name},
		nodeClass,
	); err != nil {

		if kerrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("unable to retrieve the OxideNodeClass, %w", err)
	}

	if reason := nodeClassDrift(nodeClaim, nodeClass); reason != "" {
		return reason, nil
	}

	for _, drifter := range c.drifters {
		if reason := drifter.DriftReason(nodeClaim, nodeClass); reason != "" {
			return reason, nil
		}
	}
	return "", nil
}

func nodeClassDrift(
	nodeClaim *karpcorev1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) cloudprovider.DriftReason {
	nodeClassHash, foundNodeClassHash := nodeClass.Annotations[v1alpha1.AnnotationOxideNodeClassHash]
	nodeClassHashVersion := nodeClass.Annotations[v1alpha1.AnnotationOxideNodeClassHashVersion]
	nodeClaimHash, foundNodeClaimHash := nodeClaim.Annotations[v1alpha1.AnnotationOxideNodeClassHash]
	nodeClaimHashVersion := nodeClaim.Annotations[v1alpha1.AnnotationOxideNodeClassHashVersion]

	if !foundNodeClassHash || !foundNodeClaimHash {
		return ""
	}
	if nodeClassHashVersion != nodeClaimHashVersion {
		return ""
	}
	return lo.Ternary(nodeClassHash != nodeClaimHash, providers.DriftReasonNodeClass, "")
}
