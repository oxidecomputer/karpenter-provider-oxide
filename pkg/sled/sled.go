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

// Package sled tracks instance placement and sled capacity.
package sled

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/capacity"
	"github.com/oxidecomputer/oxide.go/oxide"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ErrNoSnapshot reports an unavailable sled snapshot.
var ErrNoSnapshot = errors.New("no sled inventory snapshot has been fetched yet")

// Placement is where a single instance is running, flattened from the sled that reported it.
type Placement struct {
	// SledID is the sled the instance is running on.
	SledID string
	// RackID is the rack the sled is attached to.
	RackID string
	// Serial, Part and Revision identify the physical hardware.
	Serial   string
	Part     string
	Revision int
	// State is the sled's lifecycle state: active, or decommissioned.
	State string
	// Policy is the operator-defined policy for the sled: in_service, or expunged.
	Policy string
	// ProvisionPolicy is whether the sled will accept new resources.
	ProvisionPolicy string
	// UsableHardwareThreads and UsablePhysicalRam size the sled itself.
	UsableHardwareThreads int
	UsablePhysicalRam     uint64
}

const (
	policyExpunged = "expunged"

	provisionPolicyNonProvisionable = "non_provisionable"
)

// Placement health reasons.
type UnhealthyReason string

const (
	// ReasonExpunged is a sled removed from service entirely.
	ReasonExpunged UnhealthyReason = "Expunged"
	// ReasonNonProvisionable is a sled still running but closed to new placements.
	ReasonNonProvisionable UnhealthyReason = "NonProvisionable"
)

// Unhealthy reports whether the sled is being taken out of service, why, and in words.
func (p Placement) Unhealthy() (bool, UnhealthyReason, string) {
	switch {
	case p.Policy == policyExpunged:
		return true, ReasonExpunged, "the sled has been expunged from the rack"
	case p.ProvisionPolicy == provisionPolicyNonProvisionable:
		return true, ReasonNonProvisionable, "the sled is no longer accepting new placements"
	default:
		return false, "", ""
	}
}

type snapshot struct {
	placements map[string]Placement

	sleds []oxide.Sled

	capacity capacity.Snapshot

	takenAt time.Time
}

// Provider serves the instance-to-sled mapping for a single Oxide project from a periodically refreshed snapshot.
type Provider struct {
	ox      *oxide.Client
	project string

	minRefreshInterval time.Duration

	freshFor time.Duration

	mu   sync.RWMutex
	snap *snapshot

	generation uint64

	lastAttempt time.Time

	refreshing sync.Mutex
}

// NewProvider builds a sled inventory provider that expects to be refreshed every refreshPeriod.
func NewProvider(ox *oxide.Client, project string, refreshPeriod time.Duration) *Provider {
	return &Provider{
		ox:                 ox,
		project:            project,
		minRefreshInterval: refreshPeriod / 4,
		freshFor:           2 * refreshPeriod,
	}
}

// PlacementFor returns where an instance is running according to the current snapshot.
func (p *Provider) PlacementFor(instanceID string) (Placement, bool, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.snap == nil {
		return Placement{}, false, ErrNoSnapshot
	}
	placement, ok := p.snap.placements[instanceID]
	return placement, ok, nil
}

// Sleds returns every sled in the current snapshot, including those running none of this project's instances.
func (p *Provider) Sleds() ([]oxide.Sled, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.snap == nil {
		return nil, ErrNoSnapshot
	}
	return p.snap.sleds, nil
}

// Capacity returns what the rack had room for when it was last read.
func (p *Provider) Capacity() capacity.Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.snap == nil {
		return capacity.Snapshot{}
	}
	return p.snap.capacity
}

// SnapshotAge reports how long ago the current snapshot was taken, and whether there is one at all.
func (p *Provider) SnapshotAge(now time.Time) (time.Duration, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.snap == nil {
		return 0, false
	}
	return now.Sub(p.snap.takenAt), true
}

// Refresh rebuilds the snapshot from the system API, and reports whether it actually did.
func (p *Provider) Refresh(ctx context.Context, force bool) (bool, error) {

	p.refreshing.Lock()
	defer p.refreshing.Unlock()

	now := time.Now()
	if !force {
		p.mu.RLock()
		tooSoon := !p.lastAttempt.IsZero() && now.Sub(p.lastAttempt) < p.minRefreshInterval
		p.mu.RUnlock()
		if tooSoon {
			return false, nil
		}
	}

	p.mu.Lock()
	p.lastAttempt = now
	p.mu.Unlock()

	sleds, err := p.ox.SledListAllPages(ctx, oxide.SledListParams{})
	if err != nil {
		return false, fmt.Errorf("unable to list sleds: %w", err)
	}

	placements := map[string]Placement{}
	capacities := make([]capacity.Sled, 0, len(sleds))
	for _, sled := range sleds {
		placement := placementFor(sled)
		room := capacity.Sled{
			ID:            sled.Id,
			Capacity:      capacityOf(placement),
			Provisionable: provisionable(placement),
		}

		instances, err := p.ox.SledInstanceListAllPages(ctx, oxide.SledInstanceListParams{
			SledId: sled.Id,
		})
		if err != nil {

			log.FromContext(ctx).V(1).Info(
				"unable to list the instances on a sled, its instances will not be mapped "+
					"and it will be treated as empty",
				"sled-id", sled.Id,
				"reason", err.Error(),
			)
			capacities = append(capacities, room)
			continue
		}

		for _, instance := range instances {
			room.Used = plus(room.Used, holds(instance))

			if string(instance.ProjectName) != p.project {
				continue
			}
			placements[instance.Id] = placement
		}
		capacities = append(capacities, room)
	}

	takenAt := time.Now()
	p.mu.Lock()
	p.generation++
	p.snap = &snapshot{
		placements: placements,
		sleds:      sleds,
		capacity:   capacity.NewSnapshot(capacities, takenAt, p.freshFor, p.generation),
		takenAt:    takenAt,
	}
	p.mu.Unlock()

	log.FromContext(ctx).V(1).Info(
		"refreshed the sled inventory snapshot",
		"sleds", len(sleds),
		"placed-instances", len(placements),
	)
	return true, nil
}

const vmmReservoirPercentOfPhysicalRAM = 87

func capacityOf(placement Placement) capacity.Resources {
	return capacity.Resources{
		CPUs:   placement.UsableHardwareThreads,
		Memory: placement.UsablePhysicalRam / 100 * vmmReservoirPercentOfPhysicalRAM,
	}
}

func provisionable(placement Placement) bool {
	unhealthy, _, _ := placement.Unhealthy()
	return !unhealthy && placement.State == string(oxide.SledStateActive)
}

func holds(instance oxide.SledInstance) capacity.Resources {
	switch instance.State {
	case oxide.InstanceStateStopped, oxide.InstanceStateFailed, oxide.InstanceStateDestroyed:
		return capacity.Resources{}
	}
	held := capacity.Resources{}
	if instance.Ncpus != nil {
		held.CPUs = *instance.Ncpus
	}
	if instance.Memory != nil && *instance.Memory > 0 {
		held.Memory = uint64(*instance.Memory)
	}
	return held
}

func plus(a, b capacity.Resources) capacity.Resources {
	return capacity.Resources{CPUs: a.CPUs + b.CPUs, Memory: a.Memory + b.Memory}
}

func placementFor(sled oxide.Sled) Placement {
	placement := Placement{
		SledID:            sled.Id,
		RackID:            sled.RackId,
		Serial:            sled.Baseboard.Serial,
		Part:              sled.Baseboard.Part,
		State:             string(sled.State),
		Policy:            string(sled.Policy.Kind()),
		UsablePhysicalRam: uint64(sled.UsablePhysicalRam),
	}
	if sled.Baseboard.Revision != nil {
		placement.Revision = *sled.Baseboard.Revision
	}
	if sled.UsableHardwareThreads != nil {
		placement.UsableHardwareThreads = *sled.UsableHardwareThreads
	}

	switch policy := sled.Policy.Value.(type) {
	case oxide.SledPolicyInService:
		placement.ProvisionPolicy = string(policy.ProvisionPolicy)
	case *oxide.SledPolicyInService:
		placement.ProvisionPolicy = string(policy.ProvisionPolicy)
	}
	return placement
}
