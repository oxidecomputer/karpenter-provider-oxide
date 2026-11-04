package framework

import (
	"context"
	"fmt"
	"os"
	"testing"
)

var (
	env      *Env
	setupErr error
)

// Main is a suite's TestMain. It loads the Env, registers the run's ssh key, runs the
// suite, then deletes the key. When the Env cannot be loaded every test skips with the
// reason, so a checkout without a cluster still compiles and passes.
func Main(m *testing.M) {
	os.Exit(run(context.Background(), m))
}

func run(ctx context.Context, m *testing.M) int {
	e, err := Load(ctx)
	if err != nil {
		setupErr = err
		return m.Run()
	}
	if err := e.loadDefaultPool(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	if err := e.registerKey(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "e2e: run %s, project %s, artifacts %s\n", e.Run, e.Project, e.Artifacts)
	env = e
	code := m.Run()
	if err := e.deleteKey(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// Setup hands a test the Env and marks it parallel, or skips it with the reason the Env
// could not be loaded.
func Setup(t *testing.T) *Env {
	t.Helper()
	if setupErr != nil {
		t.Skipf("e2e: %v", setupErr)
	}
	t.Parallel()
	return env
}
