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

// Package sled answers one question the project-scoped Oxide API cannot: which physical sled is a
// given instance running on.
//
// That mapping only exists on the fleet-scoped system API, and only sled-by-sled: there is no
// endpoint that takes an instance ID and reports its sled. Building it means listing every sled and
// then listing the instances on each one, so a rack costs one call per sled plus one. That is far too
// much to repeat per node, so the whole mapping is fetched at once into a snapshot that callers read
// from, and refreshes are rate limited no matter how many callers ask for one.
//
// Everything here is best effort by construction. The system API requires fleet-level permissions
// that an operator may reasonably decline to grant to Karpenter, so a caller that cannot read it is
// expected to carry on without the mapping rather than fail.
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

// ErrNoSnapshot reports that no snapshot has been fetched yet, so nothing can be looked up. It is
// returned instead of an empty snapshot so that callers can tell "the mapping is not available" from
// "this instance is not on any sled", which are very different things: the first is temporary and
// says nothing about the instance, the second means the instance is gone.
var ErrNoSnapshot = errors.New("no sled inventory snapshot has been fetched yet")

// Placement is where a single instance is running, flattened from the sled that reported it. Every
// field comes from the sled rather than the instance, since the instance is only how the sled was
// found.
type Placement struct {
	// SledID is the sled the instance is running on.
	SledID string
	// RackID is the rack the sled is attached to.
	RackID string
	// Serial, Part and Revision identify the physical hardware, and are what an operator correlates
	// against a rack diagram or a support case.
	Serial   string
	Part     string
	Revision int
	// State is the sled's lifecycle state: active, or decommissioned.
	State string
	// Policy is the operator-defined policy for the sled: in_service, or expunged. A node on an
	// expunged sled is on hardware that is being removed from the rack.
	Policy string
	// ProvisionPolicy is whether the sled will accept new resources. It is only meaningful for an
	// in_service sled, and is empty otherwise.
	ProvisionPolicy string
	// UsableHardwareThreads and UsablePhysicalRam size the sled itself, which is the denominator for
	// how much of a sled a node actually occupies.
	UsableHardwareThreads int
	UsablePhysicalRam     uint64
}

// Sled policy and provision policy values that mean a node's days on its current sled are numbered.
// They are compared as strings because both arrive as open enums from the API, and a value this code
// has never heard of has to read as "not one of the states we act on" rather than fail to parse.
const (
	// policyExpunged is a sled the operator has removed from service. The hardware is on its way out of
	// the rack and everything on it has to go somewhere else.
	policyExpunged = "expunged"
	// provisionPolicyNonProvisionable is a sled that is still running but closed to new placements,
	// which is how a sled is drained ahead of maintenance.
	provisionPolicyNonProvisionable = "non_provisionable"
)

// Reasons a placement is unhealthy, returned by Unhealthy so that callers can distinguish the two
// cases without re-deriving them from the raw policy strings.
type UnhealthyReason string

const (
	// ReasonExpunged is a sled removed from service entirely.
	ReasonExpunged UnhealthyReason = "Expunged"
	// ReasonNonProvisionable is a sled still running but closed to new placements.
	ReasonNonProvisionable UnhealthyReason = "NonProvisionable"
)

// Unhealthy reports whether the sled is being taken out of service, why, and in words.
//
// Both states are deliberately conservative: they are set by an operator, and neither is transient in
// the way a failed health check is, so a node sitting on one is worth replacing rather than waiting
// out. Anything else, including a state or policy this code does not recognise, is healthy. That
// direction matters: an unrecognised value must never be grounds for deleting a node.
//
// A node on a non-provisionable sled is also the case that makes replacement work at all. Oxide
// places instances rather than letting a caller pick a sled, so there is no way to ask for a
// replacement somewhere else. But a sled closed to new placements will not accept one, so the
// replacement necessarily lands elsewhere.
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

// snapshot is one complete read of the rack, held immutably so that readers never observe a
// half-applied refresh.
type snapshot struct {
	// placements maps an Oxide instance ID to where it was running when the snapshot was taken.
	placements map[string]Placement
	// sleds is every sled the rack reported, including ones running none of our instances.
	sleds []oxide.Sled
	// capacity is the same read expressed as what each sled has room for, which is what decides
	// whether a shape is worth offering at all. It is built here because the walk that produces the
	// placements is already listing every instance on every sled, and that listing is the only thing
	// that says how much of a sled is spoken for.
	capacity capacity.Snapshot
	// takenAt is when the refresh that produced this snapshot completed.
	takenAt time.Time
}

// Provider serves the instance-to-sled mapping for a single Oxide project from a periodically
// refreshed snapshot. It is safe for concurrent use.
type Provider struct {
	ox      *oxide.Client
	project string
	// minRefreshInterval is the floor between refreshes. Callers ask for a refresh whenever they miss
	// the snapshot, and a node that is not in it at all misses on every reconcile, so without this
	// floor a single unmappable instance would drive a full rack walk per attempt.
	minRefreshInterval time.Duration
	// freshFor is how long a reading of the rack may be believed about what does not fit. See
	// capacity.Snapshot.Fresh: it is what stops a provider that has lost access to the system API from
	// suppressing offerings forever on the strength of an ancient reading.
	freshFor time.Duration

	mu   sync.RWMutex
	snap *snapshot
	// generation counts successful refreshes, and is what readers of the capacity snapshot compare to
	// notice that the rack has been read again.
	generation uint64
	// lastAttempt is when a refresh was last started, whether or not it succeeded. Failures count:
	// the usual failure is a token without fleet permissions, which fails fast and would otherwise
	// retry as quickly as callers ask.
	lastAttempt time.Time
	// refreshing serializes refreshes so that concurrent callers collapse into one rack walk rather
	// than each issuing their own.
	refreshing sync.Mutex
}

// NewProvider builds a sled inventory provider that expects to be refreshed every refreshPeriod.
//
// The period is the one number an operator sets, and the two intervals derived from it are both
// consequences of it rather than independent knobs. The on-demand floor is tighter than the period so
// that a node launched between scheduled refreshes gets labeled promptly, while a cluster full of
// unplaceable nodes still cannot drive more than one rack walk per floor. The freshness window is
// looser than the period so that a single missed or slow refresh does not make the rack look unknown.
func NewProvider(ox *oxide.Client, project string, refreshPeriod time.Duration) *Provider {
	return &Provider{
		ox:                 ox,
		project:            project,
		minRefreshInterval: refreshPeriod / 4,
		freshFor:           2 * refreshPeriod,
	}
}

// PlacementFor returns where an instance is running according to the current snapshot.
//
// The boolean reports whether the instance was found. A false with a nil error means the snapshot is
// current but does not mention this instance, which happens both for an instance that no longer
// exists and for one created since the last refresh; the caller decides whether that is worth
// refreshing for. ErrNoSnapshot means nothing has been read yet and the caller should not conclude
// anything about the instance.
func (p *Provider) PlacementFor(instanceID string) (Placement, bool, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.snap == nil {
		return Placement{}, false, ErrNoSnapshot
	}
	placement, ok := p.snap.placements[instanceID]
	return placement, ok, nil
}

// Sleds returns every sled in the current snapshot, including those running none of this project's
// instances. It is the fleet-wide view behind rack-level reporting, as opposed to the per-node view
// PlacementFor serves.
func (p *Provider) Sleds() ([]oxide.Sled, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.snap == nil {
		return nil, ErrNoSnapshot
	}
	return p.snap.sleds, nil
}

// Capacity returns what the rack had room for when it was last read.
//
// Unlike the lookups above this reports no error, because its caller is asking a question that has a
// safe answer when the rack has never been read: the zero snapshot reports itself as unknown, and a
// caller that cannot see the rack is expected to carry on without it rather than stop. That is the
// same posture the rest of this package takes, and it is what makes inventory something an operator
// can leave switched off.
func (p *Provider) Capacity() capacity.Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.snap == nil {
		return capacity.Snapshot{}
	}
	return p.snap.capacity
}

// SnapshotAge reports how long ago the current snapshot was taken, and whether there is one at all.
// Callers use it to label a node with how stale the placement they are writing is.
func (p *Provider) SnapshotAge(now time.Time) (time.Duration, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.snap == nil {
		return 0, false
	}
	return now.Sub(p.snap.takenAt), true
}

// Refresh rebuilds the snapshot from the system API, and reports whether it actually did.
//
// A false with a nil error means the call was skipped because a refresh happened within
// minRefreshInterval, which is the expected outcome when many callers are asking at once. force
// bypasses that floor and is for the periodic refresh that has to make progress regardless of how
// often lookups have been missing.
//
// The snapshot is only replaced once the whole rack has been read: a partial walk would silently
// look like instances having left their sleds.
func (p *Provider) Refresh(ctx context.Context, force bool) (bool, error) {
	// Held for the duration of the walk so that a second caller waits for this refresh and then sees
	// it as recent, rather than starting a duplicate one behind it.
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
			// One unreadable sled would otherwise discard a whole rack's worth of good placements. The
			// instances on it simply go unmapped, which reads the same as a node the snapshot has not
			// caught up with yet.
			//
			// Its capacity is recorded as entirely free, which is not a claim that it is: it is the
			// reading that cannot suppress an offering. Understating what a sled has room for is the
			// one error this snapshot must not make, since a shape nobody offers is a shape nobody ever
			// discovers the truth about.
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
			// A rack hosts every silo's instances, and only this project's can correspond to a node in
			// this cluster. Skipping the rest keeps the snapshot proportional to the cluster rather
			// than to the rack, and keeps unrelated tenants' instance IDs out of memory.
			//
			// The resources above are counted before this filter, deliberately: another silo's instance
			// occupies the sled just as much as ours, and a placement decision that ignored it would be
			// answering a question about a rack nobody is running.
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

// capacityOf is what a sled has to place instances with.
//
// These are the numbers the sled reports about its own hardware, which is more than the control plane
// will actually hand out to instances: memory is held back for the hypervisor and the control plane,
// and none of that is visible on this API. Treating the hardware totals as placeable therefore
// overstates the rack, which is the direction that costs a refused launch rather than a shape nobody
// ever offers.
func capacityOf(placement Placement) capacity.Resources {
	return capacity.Resources{
		CPUs:   placement.UsableHardwareThreads,
		Memory: placement.UsablePhysicalRam,
	}
}

// provisionable reports whether the sled will accept a new instance. A sled being drained or removed
// from the rack has free resources and will still refuse everything placed on it, so its headroom is
// not headroom at all.
func provisionable(placement Placement) bool {
	unhealthy, _, _ := placement.Unhealthy()
	return !unhealthy && placement.State == string(oxide.SledStateActive)
}

// holds is what an instance on a sled is occupying.
//
// An instance that is stopped, failed or destroyed is counted as holding nothing. It has no virtual
// machine running on the sled, so its resources are the sled's to hand out, and counting them as
// spoken for would understate the rack. The size fields are pointers on this API and a missing one is
// read as zero for the same reason: guessing high there would invent usage that does not exist.
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

// plus adds two amounts of resources.
func plus(a, b capacity.Resources) capacity.Resources {
	return capacity.Resources{CPUs: a.CPUs + b.CPUs, Memory: a.Memory + b.Memory}
}

// placementFor flattens a sled into the placement recorded for every instance on it.
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
	// Provision policy lives inside the in_service variant of the policy union, so it is only present
	// on a sled that is in service at all.
	switch policy := sled.Policy.Value.(type) {
	case oxide.SledPolicyInService:
		placement.ProvisionPolicy = string(policy.ProvisionPolicy)
	case *oxide.SledPolicyInService:
		placement.ProvisionPolicy = string(policy.ProvisionPolicy)
	}
	return placement
}
