package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
)

func testSpec() Spec {
	return Spec{
		ConsumerUnit: "kubelet.service",
		ConfigPath:   "/etc/example/join.yaml",
		Config:       "kind: JoinConfiguration\n",
		StartCmd:     "example join --config /etc/example/join.yaml",
	}
}

func assemble(t *testing.T, nodeClass *v1alpha1.OxideNodeClass, spec Spec) string {
	t.Helper()
	out, err := Assemble(context.Background(), userDataReader(), nodeClass, spec)
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	return out
}

func TestAssembleRendersThePreambleMergeUserDataExpects(t *testing.T) {
	out := assemble(t, &v1alpha1.OxideNodeClass{}, testSpec())
	if !strings.HasPrefix(out, bashPreamble) {
		t.Errorf("assembled user-data does not begin with the preamble:\n%s", out)
	}

	// The merge is what would break on a preamble mismatch, so exercise it.
	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{UserData: "echo custom\n"},
	}
	merged := assemble(t, nodeClass, testSpec())
	custom := strings.Index(merged, "echo custom")
	start := strings.Index(merged, "example join --config")
	if custom < 0 || start < 0 || custom >= start {
		t.Errorf("custom user-data does not run before the start command:\n%s", merged)
	}
}

func TestAssembleInstallsTheConfigBeforeStarting(t *testing.T) {
	out := assemble(t, &v1alpha1.OxideNodeClass{}, testSpec())
	for _, want := range []string{
		"install -d -m 0755 '/etc/example'",
		"cat <<'KARPENTER_INSTALL_EOF' > '/etc/example/join.yaml'",
		"kind: JoinConfiguration",
		"chmod 0600 '/etc/example/join.yaml'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("assembled user-data is missing %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "example join --config /etc/example/join.yaml\n") {
		t.Errorf("assembled user-data does not end with the start command:\n%s", out)
	}
}

func TestAssembleRejectsAConfigThatWouldCloseItsOwnHeredoc(t *testing.T) {
	spec := testSpec()
	spec.Config = "kind: JoinConfiguration\nKARPENTER_INSTALL_EOF\nrm -rf /\n"
	if _, err := Assemble(
		context.Background(),
		userDataReader(),
		&v1alpha1.OxideNodeClass{},
		spec,
	); err == nil {
		t.Error("Assemble() accepted a config containing the heredoc marker")
	}
}

func TestAssembleRejectsAConfigPathItCannotQuote(t *testing.T) {
	for name, path := range map[string]string{
		"relative": "etc/example/join.yaml",
		"quoted":   "/etc/example/it's.yaml",
		"newline":  "/etc/example/join.yaml\nrm -rf /",
	} {
		t.Run(name, func(t *testing.T) {
			spec := testSpec()
			spec.ConfigPath = path
			if _, err := Assemble(
				context.Background(),
				userDataReader(),
				&v1alpha1.OxideNodeClass{},
				spec,
			); err == nil {
				t.Errorf("Assemble() accepted config path %q", path)
			}
		})
	}
}
