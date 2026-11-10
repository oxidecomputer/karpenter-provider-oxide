package instancetype

import (
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
)

func (i *Provider) Availability() availability.Snapshot {
	reading := i.availability.Snapshot()
	if i.quotas != nil {
		reading = reading.WithQuota(i.quotas.Snapshot())
	}
	return reading
}

func (i *Provider) AvailabilityCounts(reading availability.Snapshot) (unavailable, total int) {
	for _, shape := range i.shapes {
		available := false
		for _, platform := range v1alpha1.CPUPlatforms {
			if reading.Evaluate(
				availability.Request{Shape: shape, Platform: availability.Platform(platform)},
			).Available {
				available = true
				break
			}
		}
		if !available {
			unavailable++
		}
	}
	return unavailable, len(i.shapes)
}
