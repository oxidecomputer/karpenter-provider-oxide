package main

import (
	"fmt"
	"os"

	"github.com/oxidecomputer/karpenter-provider-oxide/kwok/audit"
)

func main() {
	if err := audit.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
