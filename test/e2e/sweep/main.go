// Clean up any leftover resources created by the test suite in K8s or Oxide (like ssh keys)
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/oxidecomputer/karpenter-provider-oxide/test/e2e/framework"
)

func main() {
	ctx := context.Background()
	e, err := framework.Load(ctx)
	if err == nil {
		err = e.Sweep(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: sweep: %v\n", err)
		os.Exit(1)
	}
}
