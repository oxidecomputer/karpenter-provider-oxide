package framework

import "testing"

func TestRunFailsWhenTheEnvironmentCannotLoad(t *testing.T) {
	t.Setenv("E2E_TIMEOUT", "invalid")
	if code := run(t.Context(), nil); code != 1 {
		t.Fatalf("exit code = %d, want failure", code)
	}
}
