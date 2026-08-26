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

// Package utilization tracks how much of the silo's quota is already spent.
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

// ErrNoSnapshot reports an unavailable quota snapshot.
var ErrNoSnapshot = errors.New("no utilization snapshot has been fetched yet")

// Resource is one of the three dimensions a silo quota is expressed in.
type Resource string

const (
	ResourceCPUs    Resource = "cpus"
	ResourceMemory  Resource = "memory"
	ResourceStorage Resource = "storage"
)

// Quota is the silo's capacity and what has been provisioned against it, as of one read.
type Quota struct {
	CapacityCPUs    int64
	CapacityMemory  uint64
	CapacityStorage uint64

	ProvisionedCPUs    int64
	ProvisionedMemory  uint64
	ProvisionedStorage uint64
}

// Remaining returns the headroom left in one dimension.
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
type Request struct {
	CPUs    int64
	Memory  uint64
	Storage uint64
}

// Admits checks a request against quota.
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

// Explain describes a quota refusal.
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

// Provider serves the silo's quota from a periodically refreshed snapshot.
type Provider struct {
	ox *oxide.Client

	minRefreshInterval time.Duration

	mu    sync.RWMutex
	quota *Quota

	takenAt time.Time

	lastAttempt time.Time

	refreshing sync.Mutex

	generation atomic.Uint64
}

func NewProvider(ox *oxide.Client, minRefreshInterval time.Duration) *Provider {
	return &Provider{
		ox:                 ox,
		minRefreshInterval: minRefreshInterval,
	}
}

// Quota returns the current snapshot.
func (p *Provider) Quota() (Quota, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.quota == nil {
		return Quota{}, ErrNoSnapshot
	}
	return *p.quota, nil
}

// Generation identifies the current quota.
func (p *Provider) Generation() uint64 {
	return p.generation.Load()
}

// Refresh re-reads the silo's quota, and reports whether it actually did.
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

	if changed {
		p.generation.Add(1)

		log.FromContext(ctx).V(1).Info(
			"silo quota changed",
			"cpus-remaining", format(ResourceCPUs, quota.Remaining(ResourceCPUs)),
			"memory-remaining", format(ResourceMemory, quota.Remaining(ResourceMemory)),
			"storage-remaining", format(ResourceStorage, quota.Remaining(ResourceStorage)),
		)
	}
	return true, nil
}

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
