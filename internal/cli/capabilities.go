package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/provider/bookstack"
)

// defaultRegistry wires every provider implementation this build ships. Registration is static, so a
// failure here is a programming error rather than a runtime condition; TestDefaultRegistry proves it.
func defaultRegistry() *capability.Registry {
	reg := capability.NewRegistry()
	if err := bookstack.Register(reg); err != nil {
		panic("provider registration is static and must not fail: " + err.Error())
	}
	return reg
}

// catalog builds the discovery view over the configured connections.
func catalog(opts *Options, reg *capability.Registry) (*capability.Catalog, error) {
	path, err := config.Path(opts.Config)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, classifyUserError(err)
	}

	// A provider only contributes capabilities through a configured connection.
	connections := make(map[string]string, len(cfg.Connections))
	for name, conn := range cfg.Connections {
		connections[name] = cfg.Services[conn.Service].Provider
	}
	return capability.NewCatalog(reg, connections), nil
}

func newCapabilitiesCommand(opts *Options, reg *capability.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities",
		Short: "List the capabilities the configured connections offer",
		Long: "Without --connection the deduplicated union of all configured connections is listed.\n" +
			"With --connection only that connection's capabilities are listed.",
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cat, err := catalog(opts, reg)
			if err != nil {
				return err
			}
			operations, err := cat.List(opts.Connection)
			if err != nil {
				return classifyUserError(err)
			}

			rows := make([]output.Row, len(operations))
			for i, operation := range operations {
				rows[i] = output.Row{
					"name":        operation.ID,
					"risk":        string(operation.Risk.Effect),
					"description": operation.Description,
				}
			}
			return emit(c, opts, output.Collection{
				Columns: []string{"name", "risk", "description"},
				Rows:    rows,
			})
		},
	}
}

func newDescribeCommand(opts *Options, reg *capability.Registry) *cobra.Command {
	var short bool

	cmd := &cobra.Command{
		Use:   "describe <capability>",
		Short: "Show the contract of one capability",
		Args:  exactlyOneArg,
		RunE: func(c *cobra.Command, args []string) error {
			cat, err := catalog(opts, reg)
			if err != nil {
				return err
			}
			cap, err := cat.Describe(opts.Connection, args[0])
			if err != nil {
				return classifyUserError(err)
			}
			if short {
				return emit(c, opts, output.Object{
					Fields: []output.Field{{Name: "summary", Value: cap.Summary()}},
				})
			}
			return emit(c, opts, contractObject(cap))
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print the one-line summary instead of the full contract")

	return cmd
}

// contractObject flattens a capability contract into scalar fields. Required arguments carry a trailing
// "!" so the machine formats stay one flat record.
//
// callbell-dev: argument and field prose lives in the documentation; add nested values only when a
// provider result actually needs them.
func contractObject(d capability.Descriptor) output.Object {
	arguments := make([]string, len(d.Arguments))
	for i, a := range d.Arguments {
		arguments[i] = a.Name
		if a.Required {
			arguments[i] += "!"
		}
	}
	return output.Object{Fields: []output.Field{
		{Name: "name", Value: d.ID},
		{Name: "risk", Value: string(d.Risk.Effect)},
		{Name: "description", Value: d.Description},
		{Name: "arguments", Value: strings.Join(arguments, ",")},
		{Name: "fields", Value: strings.Join(capabilityFieldNames(d), ",")},
	}}
}

// validateCapabilityFields checks a projection against the declared contract before a provider is called.
// Project performs the same validation again on the real result in emit, keeping the declaration honest.
func validateCapabilityFields(reg *capability.Registry, provider, name string, fields []string) error {
	for _, operation := range reg.Provider(provider) {
		if operation.ID != name {
			continue
		}
		_, err := output.Project(output.Collection{Columns: capabilityFieldNames(operation)}, fields)
		return classifyUserError(err)
	}
	return classifyUserError(&capability.UnsupportedError{Capability: name})
}

func capabilityFieldNames(d capability.Descriptor) []string {
	fields := make([]string, len(d.Fields))
	for i, f := range d.Fields {
		fields[i] = f.Name
	}
	return fields
}

func exactlyOneArg(_ *cobra.Command, args []string) error {
	if len(args) != 1 {
		return &UsageError{fmt.Errorf("expected exactly one capability name, got %d", len(args))}
	}
	return nil
}
