package main

import (
	"fmt"
	"os"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cli"
)

func main() {
	if err := cli.New().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
