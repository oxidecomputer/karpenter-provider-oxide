package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Env is what a Host needs from the suite: clients, where to put the probe pod,
// and how to label it so a sweep can find it.
type Env struct {
	Config    *rest.Config
	Clientset *kubernetes.Clientset
	Oxide     *oxide.Client
	Project   string
	Namespace string
	Labels    map[string]string
	// ProbeName is the probe pod's name for this test.
	ProbeName string
}

// Host provides access to a node's Kubernetes, Oxide, and guest state.
type Host struct {
	t    *testing.T
	env  Env
	node *corev1.Node

	// instanceID is the Oxide instance behind the node, from its provider ID.
	instanceID string
	diskPrefix string

	probeOnce sync.Once
	probe     *Probe
	probeErr  error
}

// Node returns the Host for a registered node. instanceID and diskPrefix come
// from the NodeClaim, which carries them whether or not the node registered.
func Node(t *testing.T, env Env, node *corev1.Node, instanceID, diskPrefix string) *Host {
	t.Helper()
	h := &Host{t: t, env: env, node: node, instanceID: instanceID, diskPrefix: diskPrefix}
	t.Cleanup(func() {
		if h.probe == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := h.probe.Delete(ctx); err != nil {
			t.Logf("deleting probe pod: %v", err)
		}
	})
	return h
}

// Name is the node's name.
func (h *Host) Name() string { return h.node.Name }

// KubeletConfiguration is the subset of the kubelet's live configuration the suite asserts on.
type KubeletConfiguration struct {
	MaxPods        int32             `json:"maxPods"`
	PodsPerCore    int32             `json:"podsPerCore"`
	KubeReserved   map[string]string `json:"kubeReserved"`
	SystemReserved map[string]string `json:"systemReserved"`
	EvictionHard   map[string]string `json:"evictionHard"`
	EvictionSoft   map[string]string `json:"evictionSoft"`
	ClusterDNS     []string          `json:"clusterDNS"`
	CPUCFSQuota    *bool             `json:"cpuCFSQuota"`
}

// KubeletConfig reads the live kubelet configuration through the node proxy's configz endpoint.
func (h *Host) KubeletConfig(ctx context.Context) (*KubeletConfiguration, error) {
	raw, err := h.env.Clientset.CoreV1().RESTClient().Get().
		Resource("nodes").Name(h.node.Name).SubResource("proxy").Suffix("configz").
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading configz of %s: %w", h.node.Name, err)
	}
	var out struct {
		KubeletConfig KubeletConfiguration `json:"kubeletconfig"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding configz of %s: %w", h.node.Name, err)
	}
	return &out.KubeletConfig, nil
}

// Instance is the Oxide instance behind the node.
func (h *Host) Instance(ctx context.Context) (*oxide.Instance, error) {
	return h.env.Oxide.InstanceView(ctx, oxide.InstanceViewParams{
		Instance: oxide.NameOrId(h.instanceID),
	})
}

// Disks are the disks attached to the instance, boot disk included.
func (h *Host) Disks(ctx context.Context) ([]oxide.Disk, error) {
	return h.env.Oxide.InstanceDiskListAllPages(ctx, oxide.InstanceDiskListParams{
		Instance: oxide.NameOrId(h.instanceID),
	})
}

// DiskPrefix is what every disk of the launch is named with.
func (h *Host) DiskPrefix() string { return h.diskPrefix }

// Mount is one entry of the guest's mount table.
type Mount struct {
	Target  string `json:"target"`
	Source  string `json:"source"`
	FSType  string `json:"fstype"`
	Options string `json:"options"`
}

// Mounts is the guest's mount table, flattened.
func (h *Host) Mounts(ctx context.Context) ([]Mount, error) {
	out, err := h.run(ctx, "findmnt", "-J")
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Filesystems []mountNode `json:"filesystems"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, fmt.Errorf("decoding findmnt output: %w", err)
	}
	var mounts []Mount
	var walk func(nodes []mountNode)
	walk = func(nodes []mountNode) {
		for _, n := range nodes {
			mounts = append(mounts, n.Mount)
			walk(n.Children)
		}
	}
	walk(parsed.Filesystems)
	return mounts, nil
}

type mountNode struct {
	Mount
	Children []mountNode `json:"children"`
}

// Mount returns the mount at target, or false.
func (h *Host) Mount(ctx context.Context, target string) (Mount, bool, error) {
	mounts, err := h.Mounts(ctx)
	if err != nil {
		return Mount{}, false, err
	}
	for _, m := range mounts {
		if m.Target == target {
			return m, true, nil
		}
	}
	return Mount{}, false, nil
}

// BlockDevice is one entry of the guest's block device tree.
type BlockDevice struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Serial     string `json:"serial"`
	FSType     string `json:"fstype"`
	MountPoint string `json:"mountpoint"`
	Size       int64  `json:"size"`
	Type       string `json:"type"`
}

// BlockDevices is the guest's block device tree, flattened.
func (h *Host) BlockDevices(ctx context.Context) ([]BlockDevice, error) {
	out, err := h.run(
		ctx,
		"lsblk",
		"-J",
		"-b",
		"-o",
		"NAME,PATH,SERIAL,FSTYPE,MOUNTPOINT,SIZE,TYPE",
	)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		BlockDevices []blockNode `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, fmt.Errorf("decoding lsblk output: %w", err)
	}
	var devices []BlockDevice
	var walk func(nodes []blockNode)
	walk = func(nodes []blockNode) {
		for _, n := range nodes {
			devices = append(devices, n.BlockDevice)
			walk(n.Children)
		}
	}
	walk(parsed.BlockDevices)
	return devices, nil
}

type blockNode struct {
	BlockDevice
	Children []blockNode `json:"children"`
}

// Links lists the symlinks directly under dir as name to raw target.
func (h *Host) Links(ctx context.Context, dir string) (map[string]string, error) {
	out, err := h.run(ctx, "find", dir, "-mindepth", "1", "-maxdepth", "1", "-type", "l",
		"-printf", "%f\t%l\n")
	if err != nil {
		return nil, err
	}
	links := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		name, target, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		links[name] = target
	}
	return links, nil
}

// Resolve follows a path to its canonical target.
func (h *Host) Resolve(ctx context.Context, path string) (string, error) {
	out, err := h.run(ctx, "readlink", "-f", path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Unit is a systemd unit's properties, as systemctl show prints them.
func (h *Host) Unit(ctx context.Context, name string) (map[string]string, error) {
	out, err := h.run(ctx, "systemctl", "show", "--no-pager", name)
	if err != nil {
		return nil, err
	}
	props := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			props[key] = value
		}
	}
	return props, nil
}

// CloudInitStatus is what cloud-init status reports once the boot has settled.
type CloudInitStatus struct {
	Status            string              `json:"status"`
	ExtendedStatus    string              `json:"extended_status"`
	Detail            string              `json:"detail"`
	Errors            []string            `json:"errors"`
	RecoverableErrors map[string][]string `json:"recoverable_errors"`
}

// CloudInit reads cloud-init's own account of the boot. A non-zero exit still
// yields the JSON cloud-init printed, since it exits 1 when it recorded errors.
func (h *Host) CloudInit(ctx context.Context) (*CloudInitStatus, error) {
	out, err := h.run(ctx, "cloud-init", "status", "--format", "json")
	if err != nil && strings.TrimSpace(out) == "" {
		return nil, err
	}
	var status CloudInitStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return nil, fmt.Errorf("decoding cloud-init status: %w", err)
	}
	return &status, nil
}

// File is the raw contents of a file on the host.
func (h *Host) File(ctx context.Context, path string) (string, error) {
	return h.run(ctx, "cat", path)
}

func (h *Host) run(ctx context.Context, command ...string) (string, error) {
	h.probeOnce.Do(func() {
		h.probe, h.probeErr = Create(
			ctx, h.env.Config, h.env.Namespace, h.node.Name, h.env.ProbeName, h.env.Labels,
		)
	})
	if h.probeErr != nil {
		return "", errors.Join(errors.New("probe pod unavailable"), h.probeErr)
	}
	return h.probe.Run(ctx, command...)
}
