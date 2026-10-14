package instancetypes

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cli/config"
)

func TestInstanceTypesCommand(t *testing.T) {
	t.Setenv(config.EnvInstanceTypeIncrementPercent, "100")

	run := func(args ...string) (string, error) {
		cmd := NewCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}

	t.Run("environment", func(t *testing.T) {
		out, err := run()
		if err != nil {
			t.Fatal(err)
		}
		if lines := strings.Count(out, "\n"); lines != 85 {
			t.Errorf("lines = %d, want header plus 84 rows:\n%s", lines, out)
		}
	})

	t.Run("flag overrides environment", func(t *testing.T) {
		out, err := run("--increment-percent", "50", "-o", "json")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `"incrementPercent": 50`) {
			t.Errorf("flag did not override:\n%s", out[:200])
		}
	})

	for _, args := range [][]string{
		{"-o", "xml"},
		{"--increment-percent", "0"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := run(args...); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
