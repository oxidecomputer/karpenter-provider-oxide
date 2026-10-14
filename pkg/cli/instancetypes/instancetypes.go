package instancetypes

import (
	"fmt"
	"strings"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cli/config"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype/describe"
	"github.com/spf13/cobra"
)

func NewCommand() *cobra.Command {
	var (
		output           string
		incrementPercent float64
	)
	cmd := &cobra.Command{
		Use:   "instance-types",
		Short: "Print the generated instance type catalog",
		Long: fmt.Sprintf(`Print the instance type catalog the controller would offer the scheduler.

The catalog is generated the same way the controller generates it: from
%s, or the default of %s%%, unless --increment-percent
overrides it. Silo quota and rack inventory are not consulted, so every entry is
shown as offered. Ephemeral storage depends on the OxideNodeClass boot disk and
is left out.`,
			config.EnvInstanceTypeIncrementPercent,
			formatFloat(instancetype.DefaultShapeIncrementPercent),
		),
		Example: `  # kubectl style table
  karpenter instance-types

  # every column
  karpenter instance-types -o wide

  # a reference page like the AWS provider's
  karpenter instance-types -o markdown > instance-types.md

  # a sparser catalog
  karpenter instance-types --increment-percent 50 -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := describe.ParseFormat(output)
			if err != nil {
				return err
			}
			cfg, err := config.ShapeConfigFromEnv()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("increment-percent") {
				cfg.IncrementPercent = incrementPercent
				if err := cfg.Validate(); err != nil {
					return err
				}
			}
			provider, err := instancetype.NewProviderWithShapeConfig(
				nil,
				availability.New(nil),
				cfg,
			)
			if err != nil {
				return err
			}
			catalog, err := describe.Describe(
				cfg.IncrementPercent,
				provider.InstanceTypes(instancetype.Options{}),
			)
			if err != nil {
				return err
			}
			return catalog.Render(cmd.OutOrStdout(), format)
		},
	}
	formats := make([]string, 0, len(describe.Formats()))
	for _, format := range describe.Formats() {
		formats = append(formats, string(format))
	}
	cmd.Flags().StringVarP(&output, "output", "o", string(describe.FormatTable),
		"Output format, one of "+strings.Join(formats, ", "))
	cmd.Flags().Float64Var(&incrementPercent, "increment-percent",
		instancetype.DefaultShapeIncrementPercent,
		"Geometric step between successive vCPU and memory values, overriding the environment")
	return cmd
}

func formatFloat(f float64) string {
	return fmt.Sprintf("%g", f)
}
