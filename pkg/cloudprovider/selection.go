package cloudprovider

import (
	"fmt"
	"slices"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	corev1 "k8s.io/api/core/v1"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func (c *OxideCloudProvider) selectLaunch(nodeClaim *karpcorev1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass) (launchCandidate, *availability.Probe, error) {
	var candidates []string

	for _, req := range nodeClaim.Spec.Requirements {
		if req.Key != corev1.LabelInstanceTypeStable {
			continue
		}
		candidates = req.Values
	}
	placement := placementFor(nodeClass.Status.AffinityGroups)
	platforms, err := cpuPlatformsFor(nodeClaim)
	if err != nil {
		return launchCandidate{}, nil, err
	}

	reading := c.instanceTypeProvider.Availability()
	var blockedBy availability.Decision
	var available []launchCandidate
	for _, candidate := range candidates {
		specs, err := instancetype.InstanceTypeToSpecs(candidate)
		if err != nil {
			continue
		}
		for _, platform := range platforms {
			decision := reading.Evaluate(availability.Request{
				Shape:     specs,
				Platform:  platform,
				Placement: placement,
			})
			if !decision.Available {
				if blockedBy.Reason != availability.ReasonQuota {
					blockedBy = decision
				}
				continue
			}
			available = append(available, launchCandidate{specs: specs, platform: platform})
		}
	}

	if len(candidates) > 0 && len(available) == 0 {
		return launchCandidate{}, nil, cloudprovider.NewInsufficientCapacityError(
			noCandidatesError(candidates, blockedBy),
		)
	}

	// Stable, so shapes of one price keep the platform order the NodeClaim allowed.
	slices.SortStableFunc(available, func(a, b launchCandidate) int {
		return instancetype.ComparePrice(a.specs, b.specs)
	})

	if len(available) == 0 {
		return launchCandidate{}, nil, fmt.Errorf("no instance types match NodeClaim requirements")
	}

	for _, candidate := range available {
		request := availability.Request{
			Shape:     candidate.specs,
			Platform:  candidate.platform,
			Placement: placement,
		}
		if probe, ok := c.availability.Acquire(request); ok {
			return candidate, probe, nil
		}
	}
	return launchCandidate{}, nil, fmt.Errorf(
		"all %d instance types the NodeClaim can use are being probed by another launch",
		len(available),
	)
}

func noCandidatesError(candidates []string, decision availability.Decision) error {
	if decision.Message == "" {
		return fmt.Errorf("no valid instance types match NodeClaim requirements")
	}
	return fmt.Errorf(
		"all %d instance types the NodeClaim can use are unavailable: %s",
		len(candidates),
		decision.Message,
	)
}
