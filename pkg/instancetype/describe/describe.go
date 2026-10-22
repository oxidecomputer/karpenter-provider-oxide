// Package describe turns the instance type catalog into human and machine
// readable documents, in the spirit of the instance type reference pages the
// AWS provider publishes.
package describe

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const none = "<none>"

type Catalog struct {
	IncrementPercent float64        `json:"incrementPercent"`
	InstanceTypes    []InstanceType `json:"instanceTypes"`
}

type InstanceType struct {
	Name           string            `json:"name"`
	VCPUs          int               `json:"vcpus"`
	MemoryGiB      int               `json:"memoryGiB"`
	CPUMemoryRatio string            `json:"cpuMemoryRatio"`
	Labels         map[string]string `json:"labels"`
	Resources      Resources         `json:"resources"`
	Overhead       Overhead          `json:"overhead"`
	// KubeletArgs are the reservation flags bootstrap passes to kubelet.
	KubeletArgs []string   `json:"kubeletArgs"`
	Offerings   []Offering `json:"offerings"`
}

type Resources struct {
	Capacity    map[string]string `json:"capacity"`
	Allocatable map[string]string `json:"allocatable"`
}

type Overhead struct {
	KubeReserved      map[string]string `json:"kubeReserved"`
	SystemReserved    map[string]string `json:"systemReserved"`
	EvictionThreshold map[string]string `json:"evictionThreshold"`
}

type Offering struct {
	CapacityType string  `json:"capacityType"`
	Zone         string  `json:"zone"`
	CPUPlatform  string  `json:"cpuPlatform"`
	Price        float64 `json:"price"`
}

func Describe(
	incrementPercent float64,
	instanceTypes []*cloudprovider.InstanceType,
) (Catalog, error) {
	catalog := Catalog{
		IncrementPercent: incrementPercent,
		InstanceTypes:    make([]InstanceType, 0, len(instanceTypes)),
	}
	for _, it := range instanceTypes {
		described, err := describeOne(it)
		if err != nil {
			return Catalog{}, err
		}
		catalog.InstanceTypes = append(catalog.InstanceTypes, described)
	}
	return catalog, nil
}

func describeOne(it *cloudprovider.InstanceType) (InstanceType, error) {
	specs, err := instancetype.InstanceTypeToSpecs(it.Name)
	if err != nil {
		return InstanceType{}, err
	}
	labels := map[string]string{}
	for _, req := range it.Requirements {
		if req.Operator() != corev1.NodeSelectorOpIn || req.Len() != 1 {
			continue
		}
		labels[req.Key] = req.Any()
	}
	offerings := make([]Offering, 0, len(it.Offerings))
	for _, offering := range it.Offerings {
		offerings = append(offerings, Offering{
			CapacityType: offering.Requirements.Get(karpv1.CapacityTypeLabelKey).Any(),
			Zone:         offering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			CPUPlatform:  offering.Requirements.Get(v1alpha1.LabelInstanceCPUPlatform).Any(),
			Price:        offering.Price,
		})
	}
	return InstanceType{
		Name:           it.Name,
		VCPUs:          specs.Ncpus,
		MemoryGiB:      specs.MemInGiB,
		CPUMemoryRatio: labels[v1alpha1.LabelInstanceCPUMemoryRatio],
		Labels:         labels,
		Resources: Resources{
			Capacity:    quantities(it.Capacity),
			Allocatable: quantities(it.Allocatable()),
		},
		Overhead: Overhead{
			KubeReserved:      quantities(it.Overhead.KubeReserved),
			SystemReserved:    quantities(it.Overhead.SystemReserved),
			EvictionThreshold: quantities(it.Overhead.EvictionThreshold),
		},
		KubeletArgs: instancetype.ReservedFor(specs, instancetype.Reserved{}).KubeletArgs(),
		Offerings:   offerings,
	}, nil
}

func quantities(list corev1.ResourceList) map[string]string {
	out := make(map[string]string, len(list))
	for name, quantity := range list {
		out[string(name)] = quantity.String()
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// resourceOrder puts the well known resources first and anything else after, alphabetically.
func resourceOrder(maps ...map[string]string) []string {
	seen := map[string]bool{}
	var extra []string
	for _, m := range maps {
		for key := range m {
			if !seen[key] {
				seen[key] = true
				extra = append(extra, key)
			}
		}
	}
	well := []string{"cpu", "memory", "pods"}
	ordered := make([]string, 0, len(extra))
	for _, key := range well {
		if seen[key] {
			ordered = append(ordered, key)
		}
	}
	extra = slices.DeleteFunc(extra, func(key string) bool { return slices.Contains(well, key) })
	sort.Strings(extra)
	return append(ordered, extra...)
}

// human renders a quantity for a table. Anything at least a MiB is a byte count
// and rounds down to the nearest MiB, or to GiB when whole, since the exact
// figures are long and only matter to the scheduler.
func human(raw string) string {
	if raw == "" {
		return none
	}
	quantity, err := resource.ParseQuantity(raw)
	if err != nil {
		return raw
	}
	bytes := quantity.Value()
	switch {
	case bytes < instancetype.MiB:
		return raw
	case bytes%instancetype.GiB == 0:
		return strconv.FormatInt(bytes/instancetype.GiB, 10) + "Gi"
	default:
		return strconv.FormatInt(bytes/instancetype.MiB, 10) + "Mi"
	}
}

func price(p float64) string {
	return strconv.FormatFloat(p, 'f', -1, 64)
}

func lowestPrice(offerings []Offering) string {
	if len(offerings) == 0 {
		return none
	}
	lowest := offerings[0].Price
	for _, offering := range offerings[1:] {
		lowest = min(lowest, offering.Price)
	}
	return price(lowest)
}

func (c Catalog) preface() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d instance types generated with a %s%% increment.",
		len(c.InstanceTypes), strconv.FormatFloat(c.IncrementPercent, 'f', -1, 64))
	return b.String()
}
