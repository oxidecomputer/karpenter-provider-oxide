package version

import (
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// Set by the release build; see .goreleaser.yaml.
var (
	Version = "dev"
	Commit  = ""
)

func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var b strings.Builder
			fmt.Fprintf(&b, "version: %s\n", Version)
			if Commit != "" {
				fmt.Fprintf(&b, "commit: %s\n", Commit)
			}
			fmt.Fprintf(&b, "go: %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
			_, err := io.WriteString(cmd.OutOrStdout(), b.String())
			return err
		},
	}
}
