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

package capacity

import (
	"fmt"
	"slices"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
)

// shapeState translates the breaker's internal state into the published one. They are spelled
// differently on purpose: the API uses Kubernetes-style capitalization, and pinning the mapping here
// means renaming an internal state cannot silently change what users are reading.
func shapeState(state instancetype.State) string {
	switch state {
	case instancetype.StateBackoff:
		return v1alpha1.ShapeStateBackoff
	case instancetype.StateHalfOpen:
		return v1alpha1.ShapeStateHalfOpen
	default:
		return string(state)
	}
}

// rangeDescription says what an unavailable range covers, in the terms an operator thinks in.
//
// The two conditions are joined by "and" rather than listed, because the conjunction is the whole
// meaning and the thing that gets misread: a range starting at 11 CPUs and 22 GiB does not rule out
// every shape with 11 CPUs, and it does not rule out every shape with 22 GiB. It rules out the ones
// that ask for both. Sixteen CPUs with 16 GiB sits outside it and launches fine.
//
// A dimension can be absent from the threshold, which a zero says and this spells out as "any". The
// difference matters more here than anywhere: ">=129 vCPUs and >=0 GiB" reads like a memory condition
// that is trivially met, and an operator scanning the list has to work out that it is not a condition
// at all.
func rangeDescription(corner instancetype.Specs) string {
	switch {
	case corner.Ncpus <= 0 && corner.MemInGiB <= 0:
		return "every shape"
	case corner.MemInGiB <= 0:
		return fmt.Sprintf(">=%d vCPUs, any memory", corner.Ncpus)
	case corner.Ncpus <= 0:
		return fmt.Sprintf(">=%d GiB, any vCPUs", corner.MemInGiB)
	default:
		return fmt.Sprintf(">=%d vCPUs and >=%d GiB", corner.Ncpus, corner.MemInGiB)
	}
}

// headroomDescription says what outgrowing a sled's headroom covers.
//
// It is an "or" where a refusal range is an "and", and the words are the only thing carrying that
// distinction to a reader scanning the list. A sled takes a shape only when the shape fits in both of
// its free resources, so wanting more of either is enough to be out - which is exactly why this is one
// sentence naming both numbers rather than two entries each naming one, where each half read as a
// constraint that had to be met on its own.
//
// A dimension the catalog cannot exceed is not a condition at all, and saying ">384 vCPUs" for it
// would read as one. Those are spelled "any", the same way an absent corner dimension is.
func headroomDescription(headroom instancetype.Specs) string {
	limits := instancetype.CatalogLimits()
	cpusBind := headroom.Ncpus < limits.Ncpus
	memoryBinds := headroom.MemInGiB < limits.MemInGiB
	switch {
	case headroom.Ncpus <= 0 && headroom.MemInGiB <= 0:
		return "every shape"
	case !cpusBind && !memoryBinds:
		return "no shape"
	case !cpusBind:
		return fmt.Sprintf(">%d GiB, any vCPUs", headroom.MemInGiB)
	case !memoryBinds:
		return fmt.Sprintf(">%d vCPUs, any memory", headroom.Ncpus)
	default:
		return fmt.Sprintf(">%d vCPUs or >%d GiB", headroom.Ncpus, headroom.MemInGiB)
	}
}

// sortRanges orders unavailable ranges smallest first, by CPU and then memory.
//
// The order is part of the published view rather than an incidental detail. These come out of a map,
// so without sorting the object would be rewritten every period with the same contents in a different
// arrangement, and smallest-first is the order that reads as a boundary: the first entry is the
// smallest thing the provider is refusing, and everything at least that large is refused with it.
//
// Inventory ranges have no corner, so they sort ahead of every refusal. That is where they belong
// anyway: what the rack has no room for at all is the wider fact, and a refusal inside a region the
// rack cannot place is a detail of it.
func sortRanges(ranges []v1alpha1.UnavailableRange) {
	slices.SortFunc(ranges, func(a, b v1alpha1.UnavailableRange) int {
		if delta := a.FromNcpus - b.FromNcpus; delta != 0 {
			return delta
		}
		if delta := a.FromMemoryGiB - b.FromMemoryGiB; delta != 0 {
			return delta
		}
		if delta := a.MaxNcpus - b.MaxNcpus; delta != 0 {
			return delta
		}
		return a.MaxMemoryGiB - b.MaxMemoryGiB
	})
}

// sortSleds orders sleds by ID, which is stable across readings and so keeps the object from churning
// on the order the rack happened to be listed in.
func sortSleds(sleds []v1alpha1.SledCapacity) {
	slices.SortFunc(sleds, func(a, b v1alpha1.SledCapacity) int {
		return strings.Compare(a.ID, b.ID)
	})
}
