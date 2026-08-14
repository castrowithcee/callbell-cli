package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/provider/bookstack"
	"github.com/castrowithcee/callbell-cli/internal/tui"
)

func newTUICommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Edit the configuration in a terminal interface",
		Long: "The editor manages services, credentials, connections, and domain defaults, and can test a\n" +
			"selected connection. It never asks for or displays secret values: a credential names\n" +
			"environment variables, and the editor only shows whether a named variable is set.",
		Args: noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if opts.Agent {
				return &UsageError{errors.New("the terminal editor cannot run in agent mode")}
			}
			path, err := config.Path(opts.Config)
			if err != nil {
				return err
			}
			store := config.NewStore(path)
			return classifyUserError(tui.Run(store, connectionTester(store, opts), opts.Redactor, os.Stdin, os.Stdout))
		},
	}
}

// connectionTester binds the editor to the shared core function. The editor itself knows no provider.
func connectionTester(store *config.Store, opts *Options) tui.Tester {
	return func(ctx context.Context, connection string) (provider.Class, error) {
		cfg, err := store.Load()
		if err != nil {
			return "", err
		}
		resolved, err := cfg.Resolve(connection, "")
		if err != nil {
			return "", err
		}
		if resolved.Provider != bookstack.Provider {
			return "", fmt.Errorf("connection %q uses provider %q, which cannot be tested yet",
				connection, resolved.Provider)
		}
		secrets, err := opts.resolver()
		if err != nil {
			return "", err
		}
		client, err := bookstack.Open(resolved, secrets, opts.Redactor)
		if err != nil {
			return "", err
		}
		return client.TestConnection(ctx), nil
	}
}
