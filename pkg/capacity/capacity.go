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

// Package capacity answers one question about a rack: could Oxide place an instance of this size
// right now?
//
// It is deliberately nothing but arithmetic over an immutable snapshot. Reading the rack is somebody
// else's job, and doing both in one place would make the interesting part, which is what a set of
// per-sled numbers implies about placement, only testable through a fake of the Oxide API.
//
// The question has to be asked per sled. An instance is placed on one sled, so a rack with four CPUs
// free on each of two sleds cannot place an eight CPU instance no matter what the totals say.
// Aggregating first is how a scheduler talks itself into launches the rack will refuse.
//
// Every number here is an upper bound on what can actually be placed, and that direction is chosen
// deliberately. A sled's usable memory is not all offerable to instances, and the control plane keeps
// reservations this API does not expose, so free space computed here can exceed what Nexus would
// actually hand out. Being optimistic costs a refused launch, which the circuit breaker in
// pkg/instancetype absorbs. Being pessimistic would take a shape out of consideration that the rack
// could have placed, and nothing downstream can recover from that: an offering nobody offers is never
// tried, so the mistake would never be discovered.
package capacity

import "time"

// Resources is an amount of the two things a sled places instances against. Storage is deliberately
// absent: Oxide disks are distributed across the rack rather than consumed from the sled the instance
// lands on, so it has no bearing on whether a sled can host a shape.
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
	// Provisionable is whether the sled will accept a new instance at all. A sled being drained or
	// expunged has free resources and will still refuse everything, so counting its headroom would
	// claim capacity that cannot be reached.
	Provisionable bool
}

// Free is what is left on the sled, floored at zero in both dimensions so that a sled reporting more
// used than capacity, which a snapshot taken mid-migration can, contributes nothing rather than
// wrapping around into an enormous number.
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

// Snapshot is one complete reading of the rack, and is immutable once built. It is passed by value,
// and readers of one are unaffected by a refresh replacing it.
type Snapshot struct {
	sleds    []Sled
	takenAt  time.Time
	freshFor time.Duration
	// generation identifies this reading. It changes whenever a new snapshot is taken, and carries no
	// other meaning; callers use it to notice that what they computed from the last one may no longer
	// hold.
	generation uint64
}

// NewSnapshot builds a snapshot. freshFor is how long the reading may be trusted to describe the
// rack, which is a property of how often whoever built it refreshes.
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

// Known reports whether this is a reading of anything at all. The zero Snapshot is what a caller with
// inventory switched off, or with inventory that has never successfully read the rack, holds.
func (s Snapshot) Known() bool {
	return !s.takenAt.IsZero()
}

// Fresh reports whether the snapshot is recent enough to be believed about what does not fit.
//
// This is the difference between the two things a snapshot can say. "This shape fits" is a hint
// either way, since the rack moves under it. "Nothing on this rack can take this shape" is a claim
// that suppresses an offering entirely, and a claim like that must not outlive the reading it came
// from: an inventory controller that has stopped refreshing, because its token lost fleet access or
// the system API is down, would otherwise freeze the last reading in place and hold provisioning at a
// standstill indefinitely.
func (s Snapshot) Fresh(now time.Time) bool {
	return s.Known() && now.Sub(s.takenAt) <= s.freshFor
}

// Generation identifies this reading of the rack.
func (s Snapshot) Generation() uint64 {
	return s.generation
}

// TakenAt is when the reading was made. The zero time means there has never been one.
func (s Snapshot) TakenAt() time.Time {
	return s.takenAt
}

// Sleds returns the sleds in the snapshot, for callers reporting on the rack rather than asking about
// a shape.
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

// FeasibleCount is how many instances of this size the rack could place right now, counting each sled
// separately and summing.
//
// It is not a promise that all of them would succeed: placing the first changes what is free for the
// second, and the whole snapshot is a reading from some moments ago. What it is good for is
// comparing two readings, where a count rising from zero is the rack having made room for a shape it
// had none for.
func (s Snapshot) FeasibleCount(request Resources) int {
	total := 0
	for _, sled := range s.sleds {
		total += fitsOn(sled, request)
	}
	return total
}

// fitsOn is how many instances of this size one sled could take, which is whichever of the two
// resources runs out first.
func fitsOn(sled Sled, request Resources) int {
	if !sled.Provisionable || request.CPUs <= 0 || request.Memory == 0 {
		return 0
	}
	free := sled.Free()
	byCPU := free.CPUs / request.CPUs
	byMemory := int(free.Memory / request.Memory)
	return min(byCPU, byMemory)
}
