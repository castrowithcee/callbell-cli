// Package cli defines the command surface of the callbell binary. Commands stay thin adapters: they parse
// global options, hand typed results to the encoders, and translate errors into exit codes.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

// version is overridden at build time with
// -ldflags "-X github.com/castrowithcee/callbell-cli/internal/cli.version=<v>".
var version = "dev"

// Exit codes are part of the public contract.
const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
)

// Options carries the global flags to the application core.
type Options struct {
	Config     string
	Connection string
	Agent      bool
	Output     string
	Fields     []string
	Limit      int

	// Format is resolved from Output and Agent before a command runs.
	Format output.Format
	// Redactor removes secret values from anything the process prints.
	Redactor *redact.Redactor
}

// UsageError marks a usage or validation problem, which maps to exit code 2. Every other error is a
// runtime error and maps to exit code 1.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }

func (e *UsageError) Unwrap() error { return e.Err }

// Run executes the root command against the given streams and returns the process exit code. It never
// terminates the process, so callers and tests share the same path.
func Run(args []string, stdout, stderr io.Writer) int {
	opts := &Options{Redactor: &redact.Redactor{}}
	return run(newRootCommand(opts, defaultRegistry()), opts, args, stdout, stderr)
}

func run(cmd *cobra.Command, opts *Options, args []string, stdout, stderr io.Writer) int {
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	err := cmd.Execute()
	if err == nil {
		return exitOK
	}

	// Redaction happens before anything is shown, including unexpected provider errors.
	fmt.Fprintf(stderr, "callbell: %s: %s\n", codeFor(err), opts.Redactor.Error(err))
	code := exitCode(err)
	if code == exitUsage {
		fmt.Fprint(stderr, cmd.UsageString())
	}
	return code
}

// exitCode maps an error from the command layer to the documented process exit code.
func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var usage *UsageError
	if errors.As(err, &usage) {
		return exitUsage
	}
	return exitRuntime
}

func newRootCommand(opts *Options, reg *capability.Registry) *cobra.Command {
	if opts.Redactor == nil {
		opts.Redactor = &redact.Redactor{}
	}

	cmd := &cobra.Command{
		Use:   "callbell",
		Short: "Command-line client for self-hosted knowledge and service backends",
		Long: "Callbell CLI is a single command-line entry point to the knowledge and service backends you\n" +
			"already run. It gives people and automated agents the same predictable interface.",
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return &UsageError{fmt.Errorf("unknown command %q", args[0])}
			}
			return nil
		},
		PersistentPreRunE: func(c *cobra.Command, _ []string) error { return resolveFormat(c, opts) },
		// Without a subcommand there is nothing to do yet, so the root command prints its own help.
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}

	// Help and version follow Cobra conventions and go to stdout. Version output stays deterministic.
	cmd.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &UsageError{err} })

	f := cmd.PersistentFlags()
	f.StringVar(&opts.Config, "config", "", "path to the configuration file")
	f.StringVar(&opts.Connection, "connection", "", "name of the connection to use")
	f.BoolVar(&opts.Agent, "agent", false, "agent mode: machine-readable output without prose or color")
	f.StringVar(&opts.Output, "output", string(output.FormatTable), "output format: table, json, or compact")
	f.StringSliceVar(&opts.Fields, "fields", nil, "restrict the output to these fields, in this order")
	f.IntVar(&opts.Limit, "limit", output.DefaultLimit, "maximum number of records; 0 means no limit")

	cmd.AddCommand(
		newConfigCommand(opts),
		newCapabilitiesCommand(opts, reg),
		newDescribeCommand(opts, reg),
		newKnowledgeCommand(opts),
		newTUICommand(opts),
	)

	return cmd
}

// resolveFormat decides the output format once, before any command runs. An explicit --output always
// wins; otherwise agent mode selects the compact format.
func resolveFormat(c *cobra.Command, opts *Options) error {
	if c.Flags().Changed("output") {
		format, err := output.ParseFormat(opts.Output)
		if err != nil {
			return &UsageError{err}
		}
		opts.Format = format
		return nil
	}
	if opts.Agent {
		opts.Format = output.FormatCompact
		return nil
	}
	opts.Format = output.FormatTable
	return nil
}

// emit projects, limits, and encodes a result to stdout. Only payload data reaches stdout.
func emit(c *cobra.Command, opts *Options, result output.Result) error {
	projected, err := output.Project(result, opts.Fields)
	if err != nil {
		return classifyUserError(err)
	}
	return output.Encode(c.OutOrStdout(), opts.Format, output.Limit(projected, opts.Limit))
}
