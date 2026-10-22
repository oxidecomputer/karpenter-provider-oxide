package describe

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"sigs.k8s.io/yaml"
)

func catalog(t *testing.T) Catalog {
	t.Helper()
	provider, err := instancetype.NewProviderWithShapeConfig(
		nil, availability.New(nil), instancetype.ShapeConfig{IncrementPercent: 100},
	)
	if err != nil {
		t.Fatal(err)
	}
	described, err := Describe(100, provider.InstanceTypes(instancetype.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	return described
}

func TestDescribe(t *testing.T) {
	c := catalog(t)
	if len(c.InstanceTypes) != 84 {
		t.Fatalf("instance types = %d, want 84", len(c.InstanceTypes))
	}
	first := c.InstanceTypes[0]
	if first.Name != "1-1" || first.VCPUs != 1 || first.MemoryGiB != 1 ||
		first.CPUMemoryRatio != "1-1" {
		t.Errorf("first = %+v", first)
	}
	if first.Labels["node.kubernetes.io/instance-type"] != "1-1" {
		t.Errorf("labels = %v", first.Labels)
	}
	for _, key := range []string{"cpu", "memory", "pods"} {
		if first.Resources.Capacity[key] == "" || first.Resources.Allocatable[key] == "" {
			t.Errorf("resource %s missing: %+v", key, first.Resources)
		}
	}
	if first.Overhead.KubeReserved["memory"] != "256Mi" ||
		first.Overhead.EvictionThreshold["memory"] != "100Mi" {
		t.Errorf("overhead = %+v", first.Overhead)
	}
	if len(first.KubeletArgs) != 2 || !strings.HasPrefix(first.KubeletArgs[0], "kube-reserved=") {
		t.Errorf("kubelet args = %v", first.KubeletArgs)
	}
	if len(first.Offerings) != 3 || first.Offerings[0].Price != 1.125 ||
		first.Offerings[0].Zone != "rack" || first.Offerings[0].CPUPlatform != "amd_turin_v2" {
		t.Errorf("offerings = %+v", first.Offerings)
	}
}

func TestHuman(t *testing.T) {
	for raw, want := range map[string]string{
		"":           "<none>",
		"940m":       "940m",
		"110":        "110",
		"1Gi":        "1Gi",
		"100Mi":      "100Mi",
		"2874775962": "2741Mi",
		"1073741824": "1Gi",
	} {
		if got := human(raw); got != want {
			t.Errorf("human(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseFormat(t *testing.T) {
	for name, want := range map[string]Format{
		"": FormatTable, "table": FormatTable, "WIDE": FormatWide, "json": FormatJSON,
		"yml": FormatYAML, "md": FormatMarkdown, "asciidoc": FormatAsciidoc,
	} {
		got, err := ParseFormat(name)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("expected an error for xml")
	}
}

func TestRender(t *testing.T) {
	c := catalog(t)
	for _, format := range Formats() {
		t.Run(string(format), func(t *testing.T) {
			var out bytes.Buffer
			if err := c.Render(&out, format); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if !strings.Contains(text, "1-1") || !strings.Contains(text, "384-1536") {
				t.Errorf("%s output is missing instance types:\n%s", format, text)
			}
			switch format {
			case FormatTable:
				if !strings.HasPrefix(text, "NAME ") || strings.Contains(text, "KUBE-RESERVED") {
					t.Errorf("table header:\n%s", text[:200])
				}
			case FormatWide:
				if !strings.Contains(text, "KUBE-RESERVED-MEMORY") {
					t.Errorf("wide header:\n%s", text[:200])
				}
			case FormatJSON:
				var decoded Catalog
				if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
					t.Fatal(err)
				}
				if len(decoded.InstanceTypes) != len(c.InstanceTypes) {
					t.Errorf("json round trip lost entries")
				}
			case FormatYAML:
				var decoded Catalog
				if err := yaml.Unmarshal(out.Bytes(), &decoded); err != nil {
					t.Fatal(err)
				}
				if decoded.IncrementPercent != 100 {
					t.Errorf("yaml increment = %v", decoded.IncrementPercent)
				}
			case FormatMarkdown:
				for _, want := range []string{
					"# Instance Types", "## 1 vCPU", "### 1-1", "#### Labels", "| memory |",
				} {
					if !strings.Contains(text, want) {
						t.Errorf("markdown missing %q", want)
					}
				}
			case FormatAsciidoc:
				for _, want := range []string{
					"= Instance Types", "== 1 vCPU", "[[1-1]]", "==== Labels", "|===",
				} {
					if !strings.Contains(text, want) {
						t.Errorf("adoc missing %q", want)
					}
				}
			}
		})
	}
}
