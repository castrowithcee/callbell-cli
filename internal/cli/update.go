package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/selfupdate"
)

func newUpdateCommand(opts *Options, buildVersion string) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update callbell to the latest stable release",
		Long: "Update checks the latest stable GitHub release, verifies the selected archive with its\n" +
			"published SHA-256 checksum, and replaces a direct <prefix>/bin/callbell installation.\n" +
			"It also refreshes callbell.1 in the same prefix. Dev builds and symlink installations are\n" +
			"not replaced. Use --check to report availability without changing files.",
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			updater := opts.Updater
			if updater == nil {
				updater = selfupdate.New(buildVersion)
			}
			var (
				result selfupdate.Result
				err    error
			)
			if check {
				result, err = updater.Check(c.Context())
			} else {
				result, err = updater.Update(c.Context())
			}
			if err != nil {
				var unsupported *selfupdate.UnsupportedInstallationError
				if errors.Is(err, selfupdate.ErrDevelopmentBuild) || errors.As(err, &unsupported) {
					return &UsageError{err}
				}
				return err
			}
			return emit(c, opts, output.Object{Fields: []output.Field{
				{Name: "current", Value: result.Current},
				{Name: "latest", Value: result.Latest},
				{Name: "update_available", Value: result.UpdateAvailable},
				{Name: "updated", Value: result.Updated},
			}})
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only report whether a newer stable release is available")
	return cmd
}
