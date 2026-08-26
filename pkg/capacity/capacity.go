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

// Package capacity evaluates rack headroom.
package capacity

import "time"

// Resources is an amount of the two things a sled places instances against.
type Resources struct {
	CPUs   int
	Memory uint64
}

// Sled is one sled's contribution to what the rack can place.
type Sled struct {
	ID string
	// Capacity is what the sled has in total, and Used is what the instances on it are holding.
	Capacity Resources
	Used     Resources
	// Provisionable is whether the sled will accept a new instance at all.
	Provisionable bool
}

// Free returns non-negative sled headroom.
func (s Sled) Free() Resources {
	free := Resources{}
	if s.Capacity.CPUs > s.Used.CPUs {
		free.CPUs = s.Capacity.CPUs - s.Used.CPUs
	}
	if s.Capacity.Memory > s.Used.Memory {
		free.Memory = s.Capacity.Memory - s.Used.Memory
	}
	return free
}

// Snapshot is one complete reading of the rack, and is immutable once built.
type Snapshot struct {
	sleds    []Sled
	takenAt  time.Time
	freshFor time.Duration

	generation uint64
}

// NewSnapshot builds a snapshot.
func NewSnapshot(
	sleds []Sled,
	takenAt time.Time,
	freshFor time.Duration,
	generation uint64,
) Snapshot {
	return Snapshot{
		sleds:      sleds,
		takenAt:    takenAt,
		freshFor:   freshFor,
		generation: generation,
	}
}

// Known reports whether this is a reading of anything at all.
func (s Snapshot) Known() bool {
	return !s.takenAt.IsZero()
}

// Fresh reports whether the snapshot can constrain capacity.
func (s Snapshot) Fresh(now time.Time) bool {
	return s.Known() && now.Sub(s.takenAt) <= s.freshFor
}

// Generation identifies this reading of the rack.
func (s Snapshot) Generation() uint64 {
	return s.generation
}

// TakenAt is when the reading was made.
func (s Snapshot) TakenAt() time.Time {
	return s.takenAt
}

// Sleds returns the snapshot's sleds.
func (s Snapshot) Sleds() []Sled {
	return s.sleds
}

// CanFit reports whether any single sled could take an instance of this size right now.
func (s Snapshot) CanFit(request Resources) bool {
	for _, sled := range s.sleds {
		if fitsOn(sled, request) > 0 {
			return true
		}
	}
	return false
}

// FeasibleCount is how many instances of this size the rack could place right now.
func (s Snapshot) FeasibleCount(request Resources) int {
	total := 0
	for _, sled := range s.sleds {
		total += fitsOn(sled, request)
	}
	return total
}

func fitsOn(sled Sled, request Resources) int {
	if !sled.Provisionable || request.CPUs <= 0 || request.Memory == 0 {
		return 0
	}
	free := sled.Free()
	byCPU := free.CPUs / request.CPUs
	byMemory := int(free.Memory / request.Memory)
	return min(byCPU, byMemory)
}
