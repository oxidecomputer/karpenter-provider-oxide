package cloudinit

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"os/exec"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/disk"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func reader() client.Reader {
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
}

func nodeClass(sets ...v1alpha1.DiskSet) *v1alpha1.OxideNodeClass {
	return &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot:     v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
				DiskSets: sets,
			},
		},
		Status: v1alpha1.OxideNodeClassStatus{
			Images: []v1alpha1.OxideImage{{
				ID:        "11111111-1111-1111-1111-111111111111",
				SizeBytes: 4 << 30,
			}},
		},
	}
}

func diskSet(name string, count int32, fs, mountPath, mountDir string) v1alpha1.DiskSet {
	return v1alpha1.DiskSet{
		Name:       name,
		Type:       v1alpha1.DiskSetTypeDistributed,
		Count:      count,
		Size:       resource.MustParse("100Gi"),
		Filesystem: fs,
		MountPath:  mountPath,
		MountDir:   mountDir,
	}
}

func sourcedSet(name, mountPath string, readOnly bool) v1alpha1.DiskSet {
	set := diskSet(name, 1, "", mountPath, "")
	// A read-only sourced disk must match its source size.
	set.Size = resource.MustParse("4Gi")
	set.ImageSelector = []v1alpha1.ImageSelector{{Name: "some-image"}}
	set.ReadOnly = &readOnly
	return set
}

func spec(t *testing.T, nc *v1alpha1.OxideNodeClass) bootstrap.Spec {
	t.Helper()
	s := bootstrap.Spec{
		ConsumerUnit: "kubelet.service",
		ConfigPath:   "/etc/example/join.yaml",
		Config:       "kind: JoinConfiguration\n",
		StartCmd:     "example join --config /etc/example/join.yaml",
	}
	if len(nc.Spec.Storage.DiskSets) == 0 {
		return s
	}
	for _, set := range nc.Spec.Storage.DiskSets {
		if len(set.ImageSelector) == 0 {
			continue
		}
		if nc.Status.Storage == nil {
			nc.Status.Storage = &v1alpha1.StorageStatus{}
		}
		nc.Status.Storage.DiskSets = append(nc.Status.Storage.DiskSets, v1alpha1.DiskSetStatus{
			Name: set.Name,
			Images: []v1alpha1.OxideImage{
				{ID: "22222222-2222-2222-2222-222222222222", SizeBytes: 4 << 30},
			},
		})
	}
	plan, err := storage.Build("inst-test", "launch-token", nc)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}
	s.Storage = &plan
	return s
}

func render(t *testing.T, nc *v1alpha1.OxideNodeClass) string {
	t.Helper()
	out, err := New(reader()).Render(context.Background(), nc, spec(t, nc))
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return out
}

type parsedPart struct {
	contentType string
	encoding    string
	body        string
}

func parts(t *testing.T, out string) []parsedPart {
	t.Helper()
	tp := textproto.NewReader(bufio.NewReader(strings.NewReader(out)))
	header, err := tp.ReadMIMEHeader()
	if err != nil {
		t.Fatalf("user-data does not start with MIME headers: %v\n%s", err, out)
	}
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/mixed" {
		t.Fatalf("Content-Type = %q (%v), want multipart/mixed", header.Get("Content-Type"), err)
	}
	var parsed []parsedPart
	mr := multipart.NewReader(tp.R, params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			return parsed
		}
		if err != nil {
			t.Fatalf("reading a part: %v", err)
		}
		raw, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("reading a part body: %v", err)
		}
		body := string(raw)
		encoding := p.Header.Get("Content-Transfer-Encoding")
		if encoding == "base64" {
			decoded, err := io.ReadAll(
				base64.NewDecoder(base64.StdEncoding, strings.NewReader(body)),
			)
			if err != nil {
				t.Fatalf("decoding a base64 part: %v", err)
			}
			body = string(decoded)
		}
		mediaType, _, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("part Content-Type %q: %v", p.Header.Get("Content-Type"), err)
		}
		parsed = append(parsed, parsedPart{contentType: mediaType, encoding: encoding, body: body})
	}
}

// twoParts returns the cloud-config body and the shell script body.
func twoParts(t *testing.T, out string) (string, string) {
	t.Helper()
	parsed := parts(t, out)
	if len(parsed) != 2 {
		t.Fatalf("got %d parts, want a cloud-config and a shell script", len(parsed))
	}
	if parsed[0].contentType != "text/cloud-config" ||
		parsed[1].contentType != "text/x-shellscript" {
		t.Fatalf("part types = %q, %q", parsed[0].contentType, parsed[1].contentType)
	}
	return parsed[0].body, parsed[1].body
}

type parsedConfig struct {
	Bootcmd    [][]string  `yaml:"bootcmd"`
	FSSetup    []fsSetup   `yaml:"fs_setup"`
	Mounts     [][]string  `yaml:"mounts"`
	WriteFiles []writeFile `yaml:"write_files"`
}

func parseConfig(t *testing.T, body string) parsedConfig {
	t.Helper()
	if !strings.HasPrefix(body, cloudConfigHeader) {
		t.Fatalf("cloud-config part does not start with %q:\n%s", cloudConfigHeader, body)
	}
	var cfg parsedConfig
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("cloud-config part is not YAML: %v\n%s", err, body)
	}
	return cfg
}

func TestRenderWithoutDisksIsAPlainScript(t *testing.T) {
	out := render(t, nodeClass())
	if !strings.HasPrefix(out, preamble) {
		t.Errorf("user-data does not begin with the bash preamble:\n%s", out)
	}
	if strings.Contains(out, "multipart") || strings.Contains(out, cloudConfigHeader) {
		t.Errorf("a diskless node got a multipart document:\n%s", out)
	}
	for _, want := range []string{
		"install -d -m 0755 '/etc/example'\n",
		"cat <<'KARPENTER_INSTALL_EOF' > '/etc/example/join.yaml'\nkind: JoinConfiguration\nKARPENTER_INSTALL_EOF\n",
		"chmod 0600 '/etc/example/join.yaml'\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("user-data is missing %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "example join --config /etc/example/join.yaml\n") {
		t.Errorf("user-data does not end with the start command:\n%s", out)
	}
}

func TestRenderOrdersEnvironmentUserConfigAndStart(t *testing.T) {
	nc := nodeClass()
	nc.Spec.UserData = "#!/bin/bash\necho custom setup"
	nc.Spec.Env = []v1alpha1.EnvVar{{Name: "HTTP_PROXY", Value: "http://proxy.example:3128/a'b"}}
	out := render(t, nc)

	environment := strings.Index(out, "export HTTP_PROXY='http://proxy.example:3128/a'\\''b'\n")
	custom := strings.Index(out, "echo custom setup\n")
	config := strings.Index(out, "cat <<'KARPENTER_INSTALL_EOF'")
	start := strings.Index(out, "example join --config")
	if environment < len(preamble) || custom < 0 || config < 0 || start < 0 {
		t.Fatalf("user-data is missing a section:\n%s", out)
	}
	if environment >= custom || custom >= config || config >= start {
		t.Errorf("sections are out of order: env=%d custom=%d config=%d start=%d",
			environment, custom, config, start)
	}
}

func TestRenderWithDisksIsMultipart(t *testing.T) {
	out := render(
		t,
		nodeClass(diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/kubelet", "")),
	)
	config, script := twoParts(t, out)
	parseConfig(t, config)
	if !strings.HasPrefix(script, preamble) {
		t.Errorf("shell part does not begin with the preamble:\n%s", script)
	}
	if !strings.HasSuffix(script, "example join --config /etc/example/join.yaml\n") {
		t.Errorf("shell part does not end with the start command:\n%s", script)
	}
	for _, p := range parts(t, out) {
		if p.encoding != "7bit" {
			t.Errorf("an ASCII %s part is %s encoded, want it readable", p.contentType, p.encoding)
		}
	}
}

func TestCloudConfigPreparesEveryDisk(t *testing.T) {
	nc := nodeClass(
		diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/kubelet", ""),
		diskSet("local-pv", 2, v1alpha1.FilesystemXFS, "", "/mnt/local-pv"),
		diskSet("raw", 1, v1alpha1.FilesystemNone, "", "/mnt/raw"),
	)
	s := spec(t, nc)
	config, _ := twoParts(t, render(t, nc))
	cfg := parseConfig(t, config)

	if len(cfg.Bootcmd) != 2 {
		t.Fatalf("bootcmd = %v, want the udev install and one raw link", cfg.Bootcmd)
	}
	udev := cfg.Bootcmd[0]
	if len(udev) != 6 || udev[0] != "cloud-init-per" || udev[1] != "instance" || udev[3] != "sh" {
		t.Errorf("udev bootcmd = %v, want it run once per instance through sh", udev)
	}
	for _, want := range []string{
		"> " + disk.RulesPath,
		"udevadm control --reload",
		"udevadm trigger --subsystem-match=block --action=change",
		"udevadm settle",
	} {
		if !strings.Contains(udev[5], want) {
			t.Errorf("udev bootcmd does not %q:\n%s", want, udev[5])
		}
	}
	for _, d := range s.Storage.Data() {
		if !strings.Contains(udev[5], `ATTRS{serial}=="`+d.Serial+`"`) {
			t.Errorf("udev bootcmd does not name the disk with serial %s", d.Serial)
		}
	}
	if link := strings.Join(cfg.Bootcmd[1], " "); !strings.Contains(link,
		"install -d -m 0755 '/mnt/raw' && ln -sfn '/dev/karpenter/raw-0' '/mnt/raw/raw-0'") {
		t.Errorf("raw link bootcmd = %q", link)
	}

	if len(cfg.FSSetup) != 3 {
		t.Fatalf("fs_setup = %+v, want three formatted disks", cfg.FSSetup)
	}
	for _, fs := range cfg.FSSetup {
		if fs.Partition != "none" || fs.Overwrite {
			t.Errorf("fs_setup %s = %+v, want the whole device and no overwrite", fs.Device, fs)
		}
		if fs.Filesystem == v1alpha1.FilesystemExt4 &&
			!strings.Contains(strings.Join(fs.ExtraOpts, " "), "nodiscard") {
			t.Errorf("ext4 fs_setup lost its mkfs options: %v", fs.ExtraOpts)
		}
	}

	wantMounts := [][]string{
		{"/dev/karpenter/local-pv-0", "/mnt/local-pv/local-pv-0", "xfs"},
		{"/dev/karpenter/local-pv-1", "/mnt/local-pv/local-pv-1", "xfs"},
		{"/dev/karpenter/kubernetes-0", "/var/lib/kubelet", "ext4"},
	}
	if len(cfg.Mounts) != len(wantMounts) {
		t.Fatalf("mounts = %v, want %d entries", cfg.Mounts, len(wantMounts))
	}
	for i, mount := range cfg.Mounts {
		if len(mount) != 6 || mount[0] != wantMounts[i][0] || mount[1] != wantMounts[i][1] ||
			mount[2] != wantMounts[i][2] {
			t.Errorf("mounts[%d] = %v, want %v", i, mount, wantMounts[i])
		}
		if mount[3] != "defaults,noatime,x-systemd.device-timeout=60s" {
			t.Errorf(
				"mounts[%d] options = %q, which cloud-init would otherwise default to nofail",
				i,
				mount[3],
			)
		}
	}

	if len(cfg.WriteFiles) != 1 {
		t.Fatalf("write_files = %+v, want only the drop-in", cfg.WriteFiles)
	}
	dropIn := cfg.WriteFiles[0]
	if dropIn.Path != "/etc/systemd/system/kubelet.service.d/10-karpenter-disks.conf" {
		t.Errorf("drop-in path = %q", dropIn.Path)
	}
	if !strings.Contains(
		dropIn.Content,
		"RequiresMountsFor=/mnt/local-pv/local-pv-0 /mnt/local-pv/local-pv-1 /var/lib/kubelet\n",
	) {
		t.Errorf("drop-in does not require every mount:\n%s", dropIn.Content)
	}
}

func TestShellChecksMountsBeforeInstallingTheConfig(t *testing.T) {
	nc := nodeClass(
		diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/kubelet", ""),
		sourcedSet("models", "/mnt/models", true),
	)
	_, script := twoParts(t, render(t, nc))

	check := strings.Index(script, "for mp in '/mnt/models' '/var/lib/kubelet'; do\n")
	config := strings.Index(script, "cat <<'KARPENTER_INSTALL_EOF'")
	if check < 0 || config < 0 || check >= config {
		t.Errorf("the mount check does not precede the config install:\n%s", script)
	}
	if !strings.Contains(script, `mountpoint -q "$mp" ||`) {
		t.Errorf("the mount check does not use mountpoint:\n%s", script)
	}
	relabel := "restorecon -R -F '/var/lib/kubelet'; fi\n"
	if !strings.Contains(script, relabel) {
		t.Errorf("the formatted disk is not relabelled:\n%s", script)
	}
	if strings.Contains(script, "restorecon -R -F '/mnt/models'") ||
		strings.Contains(script, "'/mnt/models' '/var/lib/kubelet'; fi") {
		t.Errorf("a read-only sourced disk is relabelled:\n%s", script)
	}
}

func TestSourcedDiskIsMountedAutoAndReadOnly(t *testing.T) {
	config, _ := twoParts(t, render(t, nodeClass(sourcedSet("models", "/mnt/models", true))))
	cfg := parseConfig(t, config)
	if len(cfg.FSSetup) != 0 {
		t.Errorf("a sourced disk would be formatted: %+v", cfg.FSSetup)
	}
	if len(cfg.Mounts) != 1 || cfg.Mounts[0][2] != "auto" ||
		!strings.HasSuffix(cfg.Mounts[0][3], ",ro") {
		t.Errorf("mounts = %v, want one auto, read-only entry", cfg.Mounts)
	}
}

func TestRenderedShellParses(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	nc := nodeClass(
		diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/kubelet", ""),
		diskSet("raw", 1, v1alpha1.FilesystemNone, "", "/mnt/raw"),
	)
	nc.Spec.Env = []v1alpha1.EnvVar{{Name: "HTTP_PROXY", Value: "http://proxy.example:3128/a'b"}}
	_, script := twoParts(t, render(t, nc))
	for name, body := range map[string]string{
		"with disks":    script,
		"without disks": render(t, nodeClass()),
	} {
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(body)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("the shell part %s is not valid bash: %v\n%s", name, err, output)
		}
	}
}

func TestNonASCIIUserDataIsBase64Encoded(t *testing.T) {
	nc := nodeClass(diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/kubelet", ""))
	nc.Spec.UserData = "echo café\n"
	parsed := parts(t, render(t, nc))
	if parsed[1].encoding != "base64" {
		t.Fatalf("a non-ASCII shell part is %q encoded, want base64", parsed[1].encoding)
	}
	if !strings.Contains(parsed[1].body, "echo café\n") {
		t.Errorf("the decoded shell part lost the user-data:\n%s", parsed[1].body)
	}
	if parsed[0].encoding != "7bit" {
		t.Errorf("the ASCII cloud-config part is %q encoded", parsed[0].encoding)
	}
}

func TestRenderRejectsAConfigThatWouldCloseItsOwnHeredoc(t *testing.T) {
	nc := nodeClass()
	s := spec(t, nc)
	s.Config = "kind: JoinConfiguration\nKARPENTER_INSTALL_EOF\nrm -rf /\n"
	if _, err := New(reader()).Render(context.Background(), nc, s); err == nil {
		t.Error("Render() accepted a config containing the heredoc marker")
	}
}

func TestRenderRejectsAConfigPathItCannotQuote(t *testing.T) {
	for name, configPath := range map[string]string{
		"relative": "etc/example/join.yaml",
		"quoted":   "/etc/example/it's.yaml",
		"newline":  "/etc/example/join.yaml\nrm -rf /",
	} {
		t.Run(name, func(t *testing.T) {
			nc := nodeClass()
			s := spec(t, nc)
			s.ConfigPath = configPath
			if _, err := New(reader()).Render(context.Background(), nc, s); err == nil {
				t.Errorf("Render() accepted config path %q", configPath)
			}
		})
	}
}

func TestRenderRejectsContentContainingTheBoundary(t *testing.T) {
	nc := nodeClass(diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/kubelet", ""))
	nc.Spec.UserData = "echo " + boundary + "\n"
	if _, err := New(reader()).Render(context.Background(), nc, spec(t, nc)); err == nil {
		t.Error("Render() accepted user-data containing the MIME boundary")
	}
}

func TestRenderRejectsAnUnusableConsumerUnit(t *testing.T) {
	nc := nodeClass(diskSet("kubernetes", 1, v1alpha1.FilesystemExt4, "/var/lib/kubelet", ""))
	s := spec(t, nc)
	s.ConsumerUnit = "../kubelet.service"
	if _, err := New(reader()).Render(context.Background(), nc, s); err == nil {
		t.Error("Render() accepted a consumer unit containing a path separator")
	}
}
