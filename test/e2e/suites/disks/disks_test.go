//go:build e2e

package disks

import (
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/disk"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"github.com/oxidecomputer/karpenter-provider-oxide/test/e2e/framework"
	"github.com/oxidecomputer/karpenter-provider-oxide/test/e2e/framework/host"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestDiskSets launches with every kind of data disk and checks the Oxide side, then
// the guest's view: mounts, stable device names, the raw link, the kubelet unit's
// mount dependency and a clean cloud-init.
func TestDiskSets(t *testing.T) {
	e := framework.Setup(t)
	g := gomega.NewWithT(t)
	ctx := t.Context()

	// type has no omitempty, so a Go client has to say Distributed where YAML gets the default.
	size, distributed := resource.MustParse("10Gi"), v1alpha1.DiskSetTypeDistributed
	nc := e.NodeClass("disks", func(nc *v1alpha1.OxideNodeClass) {
		nc.Spec.Storage.DiskSets = []v1alpha1.DiskSet{
			{Name: "data", Type: distributed, Size: size, MountPath: "/mnt/data"},
			{Name: "scratch", Type: distributed, Size: size, Count: 2,
				Filesystem: v1alpha1.FilesystemXFS, MountDir: "/mnt/scratch"},
			{Name: "raw", Type: distributed, Size: size, Filesystem: v1alpha1.FilesystemNone,
				MountDir: "/mnt/raw"},
		}
	})
	pool := e.NodePool(nc)
	pod := e.Pod(pool, "cpu=1,memory=256Mi")
	e.Create(t, nc, pool, pod)
	node := e.EventuallyNodeReady(g, pool)
	h := e.Host(t, node)

	// Data disks are named <prefix><index>-<set>-<ordinal>, index counting from 1 after the boot disk.
	type want struct {
		set, device, mount, fstype, link string
		index                            int
		ordinal                          int32
	}
	wants := []want{
		{set: "data", index: 1, mount: "/mnt/data", fstype: "ext4"},
		{set: "scratch", index: 2, mount: "/mnt/scratch/scratch-0", fstype: "xfs"},
		{set: "scratch", index: 3, ordinal: 1, mount: "/mnt/scratch/scratch-1", fstype: "xfs"},
		{set: "raw", index: 4, link: "/mnt/raw/raw-0"},
	}
	for i := range wants {
		wants[i].device = disk.DevicePath(wants[i].set, wants[i].ordinal)
	}
	diskName := func(w want) string {
		return fmt.Sprintf("%s%d-%s-%d", h.DiskPrefix(), w.index, w.set, w.ordinal)
	}

	disks, err := h.Disks(ctx)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	byName := map[string]int64{}
	for _, d := range disks {
		byName[string(d.Name)] = int64(d.Size)
	}
	g.Expect(byName).To(gomega.HaveLen(len(wants)+1), "disks attached: %v", byName)
	for _, w := range wants {
		g.Expect(byName).To(gomega.HaveKeyWithValue(diskName(w), size.Value()))
	}

	devices, err := h.BlockDevices(ctx)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	bySerial := map[string]host.BlockDevice{}
	for _, d := range devices {
		bySerial[d.Serial] = d
	}
	links, err := h.Links(ctx, disk.DeviceDir)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	mounts, err := h.Mounts(ctx)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	byTarget := map[string]host.Mount{}
	for _, m := range mounts {
		byTarget[m.Target] = m
	}

	for _, w := range wants {
		serial := storage.Serial(diskName(w))
		dev, ok := bySerial[serial]
		g.Expect(ok).To(gomega.BeTrue(), "no block device with serial %s: %+v", serial, devices)

		g.Expect(links).To(gomega.HaveKey(path.Base(w.device)), "links under %s", disk.DeviceDir)
		resolved, err := h.Resolve(ctx, w.device)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(resolved).
			To(gomega.Equal(dev.Path), "%s should name the disk with serial %s", w.device, serial)

		if w.link != "" {
			g.Expect(dev.FSType).To(gomega.BeEmpty(), "raw disk %s is formatted", w.device)
			g.Expect(dev.MountPoint).To(gomega.BeEmpty(), "raw disk %s is mounted", w.device)
			target, err := h.Resolve(ctx, w.link)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(target).To(gomega.Equal(dev.Path), "%s should link to the raw disk", w.link)
			continue
		}
		m, ok := byTarget[w.mount]
		g.Expect(ok).To(gomega.BeTrue(), "%s is not mounted", w.mount)
		g.Expect(m.FSType).To(gomega.Equal(w.fstype), "%s", w.mount)
		g.Expect(m.Source).To(gomega.Equal(dev.Path), "%s", w.mount)
		g.Expect(strings.Split(m.Options, ",")).To(gomega.ContainElement("noatime"), "%s", w.mount)
	}

	unit, err := h.Unit(ctx, "kubelet.service")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	requires := strings.Fields(unit["RequiresMountsFor"])
	for _, w := range wants {
		if w.mount != "" {
			g.Expect(requires).To(gomega.ContainElement(w.mount), "kubelet RequiresMountsFor")
		}
	}

	status, err := h.CloudInit(ctx)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(status.Status).To(gomega.Equal("done"), "cloud-init: %+v", status)
	g.Expect(status.Errors).To(gomega.BeEmpty(), "cloud-init errors")
	g.Expect(status.RecoverableErrors).NotTo(gomega.HaveKey("ERROR"), "cloud-init: %+v", status)
}
