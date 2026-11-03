package cloudinit

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/disk"
	"go.yaml.in/yaml/v3"
)

const cloudConfigHeader = "#cloud-config\n"

// config is the subset of cloud-config this renderer emits. cloud-init runs the
// modules in this order during its init stage: bootcmd, write_files,
// disk_setup (fs_setup), mounts. That is why the udev rules go through bootcmd:
// write_files runs too late to reload udev before fs_setup looks for a device.
type config struct {
	Bootcmd    [][]string  `yaml:"bootcmd,omitempty"`
	FSSetup    []fsSetup   `yaml:"fs_setup,omitempty"`
	Mounts     [][]string  `yaml:"mounts,omitempty"`
	WriteFiles []writeFile `yaml:"write_files,omitempty"`
}

// fsSetup formats a whole device. With partition none cloud-init runs mkfs
// unconditionally and adds the force flag itself; its once-per-instance
// semaphore is what keeps a reboot from formatting again.
type fsSetup struct {
	Device     string   `yaml:"device"`
	Filesystem string   `yaml:"filesystem"`
	Partition  string   `yaml:"partition"`
	Overwrite  bool     `yaml:"overwrite"`
	ExtraOpts  []string `yaml:"extra_opts,omitempty"`
}

type writeFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
}

func renderCloudConfig(layout *disk.Layout, consumerUnit string) (string, error) {
	if consumerUnit == "" || strings.ContainsAny(consumerUnit, "/ \t\n") {
		return "", fmt.Errorf("consumer unit %q is not a plain unit name", consumerUnit)
	}
	udev, err := udevCommand(layout.Rules)
	if err != nil {
		return "", err
	}
	cfg := config{Bootcmd: [][]string{udev}}
	for _, link := range layout.Links {
		cfg.Bootcmd = append(cfg.Bootcmd, linkCommand(link))
	}
	for _, format := range layout.Formats {
		cfg.FSSetup = append(cfg.FSSetup, fsSetup{
			Device:     format.Device,
			Filesystem: format.Filesystem,
			Partition:  "none",
			Overwrite:  false,
			ExtraOpts:  format.Options,
		})
	}
	for _, mount := range layout.Mounts {
		cfg.Mounts = append(cfg.Mounts, []string{
			mount.Device, mount.MountPoint, mount.Type, mount.Options, "0", "0",
		})
	}
	if dropIn := layout.DropIn(); dropIn != "" {
		cfg.WriteFiles = append(cfg.WriteFiles, writeFile{
			Path:        "/etc/systemd/system/" + consumerUnit + ".d/" + disk.DropInName,
			Permissions: "0644",
			Content:     dropIn,
		})
	}

	var out bytes.Buffer
	out.WriteString(cloudConfigHeader)
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return "", fmt.Errorf("unable to marshal the cloud-config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// udevCommand installs the rules naming the disks and applies them, once per
// instance. The rules persist, so later boots name the disks on their own.
func udevCommand(rules string) ([]string, error) {
	lines := strings.Split(strings.TrimSuffix(rules, "\n"), "\n")
	quoted := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.ContainsAny(line, "'\n") {
			return nil, fmt.Errorf("udev rule %q cannot be quoted for the shell", line)
		}
		quoted = append(quoted, "'"+line+"'")
	}
	script := "printf '%s\\n' " + strings.Join(quoted, " ") + " > " + disk.RulesPath +
		" && udevadm control --reload" +
		" && udevadm trigger --subsystem-match=block --action=change" +
		" && udevadm settle --timeout=60"
	return []string{"cloud-init-per", "instance", "karpenter-disk-names", "sh", "-c", script}, nil
}

// linkCommand places a raw block disk's device under its set's mountDir. It runs
// every boot and is idempotent.
func linkCommand(link disk.Link) []string {
	return []string{"sh", "-c", fmt.Sprintf(
		"install -d -m 0755 '%s' && ln -sfn '%s' '%s'",
		path.Dir(link.Path), link.Device, link.Path,
	)}
}
