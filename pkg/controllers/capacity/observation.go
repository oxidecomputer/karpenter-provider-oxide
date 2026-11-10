package capacity

import (
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/storagerefusal"
)

type observation struct {
	Availability availability.Snapshot
	Unavailable  int
	Total        int
	Storage      storagerefusal.Refusal
	HasStorage   bool
}

func (c *Controller) observe() observation {
	reading := c.instanceTypes.Availability()
	unavailable, total := c.instanceTypes.AvailabilityCounts(reading)
	storage, hasStorage := c.storage.Last()
	return observation{Availability: reading, Unavailable: unavailable, Total: total,
		Storage: storage, HasStorage: hasStorage}
}
