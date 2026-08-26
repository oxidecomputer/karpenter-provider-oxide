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

func sortSleds(sleds []v1alpha1.SledCapacity) {
	slices.SortFunc(sleds, func(a, b v1alpha1.SledCapacity) int {
		return strings.Compare(a.ID, b.ID)
	})
}
