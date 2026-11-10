package framework

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

var env *Env

func Main(m *testing.M) {
	os.Exit(run(context.Background(), m))
}

func run(ctx context.Context, m *testing.M) int {
	setupCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	e, err := Load(setupCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	if err := e.loadDefaultPool(setupCtx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	if err := e.registerKey(setupCtx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	cancel()
	fmt.Fprintf(os.Stderr, "e2e: run %s, project %s, artifacts %s\n", e.Run, e.Project, e.Artifacts)
	env = e
	code := m.Run()
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, time.Minute)
	defer cleanupCancel()
	if err := e.deleteKey(cleanupCtx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func Setup(t *testing.T) *Env {
	t.Helper()
	t.Parallel()
	return env
}
