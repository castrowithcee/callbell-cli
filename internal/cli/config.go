package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/config"
)

func newConfigCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the callbell configuration",
		Args:  noArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Check that the configuration file is complete and consistent",
		Long: "Validate reads the configuration file and reports every schema and reference problem it\n" +
			"finds. It contacts no provider and reads no secret values. Success is silent.",
		Args: noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := config.Path(opts.Config)
			if err != nil {
				return err
			}
			_, err = config.Load(path)
			return classifyUserError(err)
		},
	})

	return cmd
}

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return &UsageError{fmt.Errorf("unexpected argument %q", args[0])}
	}
	return nil
}
