//go:build e2e

package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
)

var sweepOnly = flag.Bool("sweep", false, "delete leftovers from any run and exit")

var (
	env      *Env
	setupErr error
)

func TestMain(m *testing.M) {
	flag.Parse()
	ctx := context.Background()
	os.Exit(run(ctx, m))
}

func run(ctx context.Context, m *testing.M) int {
	e, err := resolve(ctx)
	if err != nil {
		if *sweepOnly {
			fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
			return 1
		}
		setupErr = err
		return m.Run()
	}
	if err := e.sweep(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: sweep: %v\n", err)
		return 1
	}
	if *sweepOnly {
		return 0
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

// setup hands a test the Env, or skips it with the reason the Env could not be built.
func setup(t *testing.T) *Env {
	t.Helper()
	if setupErr != nil {
		t.Skipf("e2e: %v", setupErr)
	}
	t.Parallel()
	return env
}
