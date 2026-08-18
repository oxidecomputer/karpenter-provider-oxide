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

// Package utilization tracks how much of the silo's quota is already spent, so that Karpenter stops
// offering instance shapes it is not allowed to launch.
//
// This is a different limit from the rack running out of room, and it fails differently. Rack
// capacity is discovered by trying: the provision saga places the instance or it does not, and the
// only way to find out is to attempt it. A silo quota is a number that can be read up front, so
// launching an instance that exceeds it is a wholly avoidable round trip that ends in a NodeClaim
// being created, failing, and being deleted.
//
// The awkward part is that the API reports both the same way. Exceeding a silo quota and finding no
// sled with room both come back as InsufficientCapacity, with no code to tell them apart. Reading
// the quota is therefore also what makes the error message accurate: given the shape that was
// refused and the quota headroom at the time, the two cases can be distinguished after the fact even
// though the API did not distinguish them.
//
// Everything here fails open. A quota that cannot be read constrains nothing, because the
// alternative is a provider that stops launching nodes when an observability read fails.
package utilization

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oxidecomputer/oxide.go/oxide"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ErrNoSnapshot reports that the quota has not been read yet, so nothing can be judged against it.
// It is distinct from a quota with no headroom: the first means "unknown, do not constrain", the
// second means "known, and full".
var ErrNoSnapshot = errors.New("no utilization snapshot has been fetched yet")

// Resource is one of the three dimensions a silo quota is expressed in.
type Resource string

const (
	ResourceCPUs    Resource = "cpus"
	ResourceMemory  Resource = "memory"
	ResourceStorage Resource = "storage"
)

// Quota is the silo's capacity and what has been provisioned against it, as of one read.
//
// Capacity and Provisioned are both kept rather than just the difference, because the difference on
// its own cannot explain anything: an operator reading "this node did not fit" needs to see what the
// limit was and how much of it is already spent.
type Quota struct {
	CapacityCPUs    int64
	CapacityMemory  uint64
	CapacityStorage uint64

	ProvisionedCPUs    int64
	ProvisionedMemory  uint64
	ProvisionedStorage uint64
}

// Remaining returns the headroom left in one dimension.
//
// It is signed and can go negative, which is a real state rather than a bug: a quota lowered below
// what is already provisioned leaves the silo over its limit until something is torn down. Callers
// comparing a request against it get the right answer either way, and clamping to zero would hide a
// condition worth seeing in a log line.
func (q Quota) Remaining(resource Resource) int64 {
	switch resource {
	case ResourceCPUs:
		return q.CapacityCPUs - q.ProvisionedCPUs
	case ResourceMemory:
		return int64(q.CapacityMemory) - int64(q.ProvisionedMemory)
	case ResourceStorage:
		return int64(q.CapacityStorage) - int64(q.ProvisionedStorage)
	default:
		return 0
	}
}

// Request is what launching one instance would add to the silo's provisioned totals.
//
// Storage is separate from the instance shape because it does not come from the shape. The boot disk
// size is set on the OxideNodeClass, so two nodes of identical CPU and memory can consume different
// amounts of storage quota, and storage therefore cannot be expressed as a property of an instance
// type.
type Request struct {
	CPUs    int64
	Memory  uint64
	Storage uint64
}

// Admits reports whether the quota has room for a request, and names the first dimension that does
// not if it has not.
//
// Dimensions are checked in a fixed order so that the answer is stable rather than depending on map
// iteration: an operator who raises the CPU quota should see the message move on to memory, not
// see it alternate.
func (q Quota) Admits(request Request) (bool, Resource) {
	for _, check := range []struct {
		resource Resource
		wanted   int64
	}{
		{ResourceCPUs, request.CPUs},
		{ResourceMemory, int64(request.Memory)},
		{ResourceStorage, int64(request.Storage)},
	} {
		if check.wanted > q.Remaining(check.resource) {
			return false, check.resource
		}
	}
	return true, ""
}

// Explain renders why a request does not fit, in the terms an operator can act on: what was asked
// for, what the limit is, and how much of it is already spent.
func (q Quota) Explain(request Request, resource Resource) string {
	var wanted, capacity, provisioned int64
	switch resource {
	case ResourceCPUs:
		wanted, capacity, provisioned = request.CPUs, q.CapacityCPUs, q.ProvisionedCPUs
	case ResourceMemory:
		wanted, capacity, provisioned = int64(
			request.Memory,
		), int64(
			q.CapacityMemory,
		), int64(
			q.ProvisionedMemory,
		)
	case ResourceStorage:
		wanted, capacity, provisioned = int64(
			request.Storage,
		), int64(
			q.CapacityStorage,
		), int64(
			q.ProvisionedStorage,
		)
	}
	return fmt.Sprintf(
		"the silo's %s quota has no room for this node: it needs %s, and %s of the %s quota is "+
			"already provisioned, leaving %s",
		resource,
		format(resource, wanted),
		format(resource, provisioned),
		format(resource, capacity),
		format(resource, q.Remaining(resource)),
	)
}

// Provider serves the silo's quota from a periodically refreshed snapshot. It is safe for concurrent
// use.
type Provider struct {
	ox *oxide.Client
	// minRefreshInterval is the floor between refreshes. Callers ask for one whenever a launch is
	// refused, and a burst of NodeClaims all failing at once would otherwise each trigger their own.
	minRefreshInterval time.Duration

	mu    sync.RWMutex
	quota *Quota
	// takenAt is when the current snapshot was read.
	takenAt time.Time
	// lastAttempt is when a refresh was last started, successful or not. Failures count: an
	// unauthorized read fails immediately and would otherwise retry as fast as callers ask.
	lastAttempt time.Time
	// refreshing serializes refreshes so concurrent callers collapse into one API call.
	refreshing sync.Mutex

	// generation changes whenever the quota does. The instance type provider rebuilds its offerings
	// when it moves, so this is what makes a quota change take effect on what the scheduler can pick.
	generation atomic.Uint64
}

func NewProvider(ox *oxide.Client, minRefreshInterval time.Duration) *Provider {
	return &Provider{
		ox:                 ox,
		minRefreshInterval: minRefreshInterval,
	}
}

// Quota returns the current snapshot, or ErrNoSnapshot if the quota has never been read.
//
// Callers are expected to treat ErrNoSnapshot as "do not constrain". Refusing to launch because the
// quota is unknown would turn a failed observability read into an outage.
func (p *Provider) Quota() (Quota, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.quota == nil {
		return Quota{}, ErrNoSnapshot
	}
	return *p.quota, nil
}

// Generation identifies the current quota. It changes whenever the quota does and carries no other
// meaning.
func (p *Provider) Generation() uint64 {
	return p.generation.Load()
}

// Refresh re-reads the silo's quota, and reports whether it actually did.
//
// A false with a nil error means the call was skipped because a refresh happened within
// minRefreshInterval. force bypasses that floor, for the periodic refresh that has to make progress
// regardless of how often launches have been failing.
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

	view, err := p.ox.UtilizationView(ctx)
	if err != nil {
		return false, fmt.Errorf("unable to read the silo's utilization: %w", err)
	}
	quota := quotaFrom(view)

	p.mu.Lock()
	changed := p.quota == nil || *p.quota != quota
	p.quota = &quota
	p.takenAt = time.Now()
	p.mu.Unlock()

	// Only bumped on an actual change. The offerings rebuild is keyed off this, and provisioned
	// totals move every time anything in the silo starts or stops, so bumping unconditionally would
	// rebuild the whole instance type list on every refresh for no reason.
	if changed {
		p.generation.Add(1)
		// Rendered rather than logged raw: memory and storage headroom in bytes is a 13 digit number
		// nobody can read at a glance, and this is the same rendering the quota errors use.
		log.FromContext(ctx).V(1).Info(
			"silo quota changed",
			"cpus-remaining", format(ResourceCPUs, quota.Remaining(ResourceCPUs)),
			"memory-remaining", format(ResourceMemory, quota.Remaining(ResourceMemory)),
			"storage-remaining", format(ResourceStorage, quota.Remaining(ResourceStorage)),
		)
	}
	return true, nil
}

// quotaFrom flattens the API's view into a Quota, treating absent counts as zero.
//
// A nil CPU count is the API declining to report one rather than a silo entitled to no CPUs, but
// both are handled the same way on purpose: a zero capacity with a nonzero provisioned total reads
// as "no headroom", which fails toward not launching rather than toward launching blind. The
// alternative, treating unknown as unlimited, would silently disable the whole check.
func quotaFrom(view *oxide.Utilization) Quota {
	quota := Quota{
		CapacityMemory:     uint64(view.Capacity.Memory),
		CapacityStorage:    uint64(view.Capacity.Storage),
		ProvisionedMemory:  uint64(view.Provisioned.Memory),
		ProvisionedStorage: uint64(view.Provisioned.Storage),
	}
	if view.Capacity.Cpus != nil {
		quota.CapacityCPUs = int64(*view.Capacity.Cpus)
	}
	if view.Provisioned.Cpus != nil {
		quota.ProvisionedCPUs = int64(*view.Provisioned.Cpus)
	}
	return quota
}

// format renders a quantity in the unit its resource is counted in, since bytes are unreadable at
// quota scale and CPU counts are not bytes.
func format(resource Resource, value int64) string {
	if resource == ResourceCPUs {
		return fmt.Sprintf("%d vCPUs", value)
	}
	const giB = 1 << 30
	if value%giB == 0 {
		return fmt.Sprintf("%d GiB", value/giB)
	}
	return fmt.Sprintf("%.1f GiB", float64(value)/float64(giB))
}
