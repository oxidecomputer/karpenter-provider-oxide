package cli

import (
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cli/controller"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cli/instancetypes"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cli/version"
	"github.com/spf13/cobra"
)

// New builds the root command. With no sub-command it runs the controller,
// which is what the Helm chart relies on.
func New() *cobra.Command {
	root := &cobra.Command{
		Use:   "karpenter",
		Short: "Karpenter for the Oxide cloud computer",
		Long: `Karpenter for the Oxide cloud computer.

With no sub-command the controller runs, which is what the Helm chart does.
Controller settings are read from the environment and from the flags Karpenter
itself defines; pass them after the command and they are forwarded unchanged.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Karpenter parses os.Args itself, so unknown flags belong to it.
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(*cobra.Command, []string) error {
			return controller.Run()
		},
	}
	root.AddCommand(controller.NewCommand(), instancetypes.NewCommand(), version.NewCommand())
	return root
}
