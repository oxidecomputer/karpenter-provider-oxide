package describe

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"sigs.k8s.io/yaml"
)

type Format string

const (
	FormatTable    Format = "table"
	FormatWide     Format = "wide"
	FormatJSON     Format = "json"
	FormatYAML     Format = "yaml"
	FormatMarkdown Format = "markdown"
	FormatAsciidoc Format = "adoc"
)

func Formats() []Format {
	return []Format{
		FormatTable, FormatWide, FormatJSON, FormatYAML, FormatMarkdown, FormatAsciidoc,
	}
}

// ParseFormat accepts a format name, case-insensitively, with a few aliases.
func ParseFormat(name string) (Format, error) {
	switch strings.ToLower(name) {
	case "table", "":
		return FormatTable, nil
	case "wide":
		return FormatWide, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	case "markdown", "md":
		return FormatMarkdown, nil
	case "adoc", "asciidoc":
		return FormatAsciidoc, nil
	}
	return "", fmt.Errorf("unknown output format %q, want one of %v", name, Formats())
}

func (c Catalog) Render(w io.Writer, format Format) error {
	switch format {
	case FormatTable:
		return c.renderTable(w, false)
	case FormatWide:
		return c.renderTable(w, true)
	case FormatJSON:
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(c)
	case FormatYAML:
		out, err := yaml.Marshal(c)
		if err != nil {
			return err
		}
		_, err = w.Write(out)
		return err
	case FormatMarkdown:
		return c.renderMarkdown(w)
	case FormatAsciidoc:
		return c.renderAsciidoc(w)
	}
	return fmt.Errorf("unknown output format %q", format)
}

func (c Catalog) renderTable(w io.Writer, wide bool) error {
	var b strings.Builder
	header := []string{
		"NAME", "VCPUS", "MEMORY", "RATIO",
		"ALLOCATABLE-CPU", "ALLOCATABLE-MEMORY", "PODS", "PRICE",
	}
	if wide {
		header = append(header,
			"CAPACITY-MEMORY", "KUBE-RESERVED-CPU", "KUBE-RESERVED-MEMORY",
			"EVICTION-MEMORY", "CAPACITY-TYPE", "ZONE",
		)
	}
	b.WriteString(strings.Join(header, "\t") + "\n")
	for _, it := range c.InstanceTypes {
		row := []string{
			it.Name,
			strconv.Itoa(it.VCPUs),
			strconv.Itoa(it.MemoryGiB) + "Gi",
			it.CPUMemoryRatio,
			human(it.Resources.Allocatable["cpu"]),
			human(it.Resources.Allocatable["memory"]),
			human(it.Resources.Allocatable["pods"]),
			lowestPrice(it.Offerings),
		}
		if wide {
			capacityType, zone := none, none
			if len(it.Offerings) > 0 {
				capacityType, zone = it.Offerings[0].CapacityType, it.Offerings[0].Zone
			}
			row = append(row,
				human(it.Resources.Capacity["memory"]),
				human(it.Overhead.KubeReserved["cpu"]),
				human(it.Overhead.KubeReserved["memory"]),
				human(it.Overhead.EvictionThreshold["memory"]),
				capacityType, zone,
			)
		}
		b.WriteString(strings.Join(row, "\t") + "\n")
	}
	tw := tabwriter.NewWriter(w, 0, 8, 3, ' ', 0)
	if _, err := io.WriteString(tw, b.String()); err != nil {
		return err
	}
	return tw.Flush()
}

// byVCPU groups entries by vCPU count, preserving catalog order.
func (c Catalog) byVCPU() (order []int, groups map[int][]InstanceType) {
	groups = map[int][]InstanceType{}
	for _, it := range c.InstanceTypes {
		if _, ok := groups[it.VCPUs]; !ok {
			order = append(order, it.VCPUs)
		}
		groups[it.VCPUs] = append(groups[it.VCPUs], it)
	}
	return order, groups
}

func (c Catalog) renderMarkdown(w io.Writer) error {
	var b strings.Builder
	b.WriteString("# Instance Types\n\n")
	b.WriteString(c.preface())
	b.WriteString("\n\n")
	b.WriteString("Memory figures below are rounded down to the nearest MiB. ")
	b.WriteString("The `json` and `yaml` outputs carry the exact quantities.\n\n")
	b.WriteString(
		"| Name | vCPUs | Memory | Ratio | Allocatable CPU | Allocatable memory | Price |\n",
	)
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, it := range c.InstanceTypes {
		fmt.Fprintf(&b, "| [%s](#%s) | %d | %dGi | %s | %s | %s | %s |\n",
			it.Name, it.Name, it.VCPUs, it.MemoryGiB, it.CPUMemoryRatio,
			human(it.Resources.Allocatable["cpu"]),
			human(it.Resources.Allocatable["memory"]),
			lowestPrice(it.Offerings),
		)
	}

	order, groups := c.byVCPU()
	for _, vcpus := range order {
		fmt.Fprintf(&b, "\n## %d vCPU\n", vcpus)
		for _, it := range groups[vcpus] {
			fmt.Fprintf(&b, "\n### %s\n", it.Name)
			b.WriteString("#### Labels\n\n| Label | Value |\n|---|---|\n")
			for _, key := range sortedKeys(it.Labels) {
				fmt.Fprintf(&b, "| %s | %s |\n", key, it.Labels[key])
			}
			b.WriteString(
				"\n#### Resources\n\n| Resource | Capacity | Allocatable |\n|---|---|---|\n",
			)
			for _, key := range resourceOrder(it.Resources.Capacity, it.Resources.Allocatable) {
				fmt.Fprintf(&b, "| %s | %s | %s |\n", key,
					human(it.Resources.Capacity[key]), human(it.Resources.Allocatable[key]))
			}
			b.WriteString("\n#### Overhead\n\n")
			b.WriteString("| Resource | Kube reserved | System reserved | Eviction threshold |\n")
			b.WriteString("|---|---|---|---|\n")
			for _, key := range resourceOrder(
				it.Overhead.KubeReserved, it.Overhead.SystemReserved, it.Overhead.EvictionThreshold,
			) {
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", key,
					human(it.Overhead.KubeReserved[key]),
					human(it.Overhead.SystemReserved[key]),
					human(it.Overhead.EvictionThreshold[key]))
			}
			b.WriteString("\n#### Kubelet\n\n```\n")
			for _, arg := range it.KubeletArgs {
				b.WriteString("--" + arg + "\n")
			}
			b.WriteString("```\n")
			b.WriteString("\n#### Offerings\n\n| Capacity type | Zone | Price |\n|---|---|---|\n")
			for _, offering := range it.Offerings {
				fmt.Fprintf(&b, "| %s | %s | %s |\n",
					offering.CapacityType, offering.Zone, price(offering.Price))
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func (c Catalog) renderAsciidoc(w io.Writer) error {
	var b strings.Builder
	b.WriteString("= Instance Types\n:toc: left\n:toclevels: 2\n\n")
	b.WriteString(c.preface())
	b.WriteString("\n\n")
	b.WriteString("Memory figures below are rounded down to the nearest MiB. ")
	b.WriteString("The `json` and `yaml` outputs carry the exact quantities.\n\n")
	b.WriteString("[cols=\"2,1,1,1,1,1,1\",options=\"header\"]\n|===\n")
	b.WriteString(
		"| Name | vCPUs | Memory | Ratio | Allocatable CPU | Allocatable memory | Price\n\n",
	)
	for _, it := range c.InstanceTypes {
		fmt.Fprintf(&b, "| <<%s>>\n| %d\n| %dGi\n| %s\n| %s\n| %s\n| %s\n\n",
			it.Name, it.VCPUs, it.MemoryGiB, it.CPUMemoryRatio,
			human(it.Resources.Allocatable["cpu"]),
			human(it.Resources.Allocatable["memory"]),
			lowestPrice(it.Offerings),
		)
	}
	b.WriteString("|===\n")

	order, groups := c.byVCPU()
	for _, vcpus := range order {
		fmt.Fprintf(&b, "\n== %d vCPU\n", vcpus)
		for _, it := range groups[vcpus] {
			fmt.Fprintf(&b, "\n[[%s]]\n=== %s\n", it.Name, it.Name)
			b.WriteString(
				"\n==== Labels\n\n[cols=\"2,1\",options=\"header\"]\n|===\n| Label | Value\n\n",
			)
			for _, key := range sortedKeys(it.Labels) {
				fmt.Fprintf(&b, "| %s\n| %s\n\n", key, it.Labels[key])
			}
			b.WriteString("|===\n")
			b.WriteString("\n==== Resources\n\n[cols=\"2,1,1\",options=\"header\"]\n|===\n")
			b.WriteString("| Resource | Capacity | Allocatable\n\n")
			for _, key := range resourceOrder(it.Resources.Capacity, it.Resources.Allocatable) {
				fmt.Fprintf(&b, "| %s\n| %s\n| %s\n\n", key,
					human(it.Resources.Capacity[key]), human(it.Resources.Allocatable[key]))
			}
			b.WriteString("|===\n")
			b.WriteString("\n==== Overhead\n\n[cols=\"2,1,1,1\",options=\"header\"]\n|===\n")
			b.WriteString("| Resource | Kube reserved | System reserved | Eviction threshold\n\n")
			for _, key := range resourceOrder(
				it.Overhead.KubeReserved, it.Overhead.SystemReserved, it.Overhead.EvictionThreshold,
			) {
				fmt.Fprintf(&b, "| %s\n| %s\n| %s\n| %s\n\n", key,
					human(it.Overhead.KubeReserved[key]),
					human(it.Overhead.SystemReserved[key]),
					human(it.Overhead.EvictionThreshold[key]))
			}
			b.WriteString("|===\n")
			b.WriteString("\n==== Kubelet\n\n----\n")
			for _, arg := range it.KubeletArgs {
				b.WriteString("--" + arg + "\n")
			}
			b.WriteString("----\n")
			b.WriteString("\n==== Offerings\n\n[cols=\"1,1,1\",options=\"header\"]\n|===\n")
			b.WriteString("| Capacity type | Zone | Price\n\n")
			for _, offering := range it.Offerings {
				fmt.Fprintf(&b, "| %s\n| %s\n| %s\n\n",
					offering.CapacityType, offering.Zone, price(offering.Price))
			}
			b.WriteString("|===\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
