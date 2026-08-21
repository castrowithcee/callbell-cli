package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/output"
)

// toonContract names the format version of the discovery output. TOON 4.1 is a working draft, so the
// public contract names the exact target version instead of migrating silently with the specification.
const toonContract = "TOON 4.1 (https://github.com/toon-format/spec/blob/v4.1.1/SPEC.md)"

// newProvidersCommand lists the namespaces of the tool catalog. Discovery cascades: this command answers
// which namespaces exist, "tools <namespace>" which tools one of them offers, and "tool <id>" one complete
// contract. Each step stays small enough to read, however many providers are compiled in.
func newProvidersCommand(opts *Options, registry *capability.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "providers",
		Short: "List the tool namespaces this installation offers",
		Long: "Providers lists every namespace of the tool catalog with the number of tools it offers and the\n" +
			"number of configured connections that can run them. A provider without a connection stays\n" +
			"listed with zero. It is answered from the local configuration alone: no provider is contacted\n" +
			"and no secret is read.\n\n" +
			"The tools of one namespace are one 'callbell tools <provider>' away.\n\n" +
			"The output is " + toonContract + " with LF line endings. --output json returns the same data as\n" +
			"JSON.",
		Args: noArgs,
		RunE: func(c *cobra.Command, args []string) error {
			format, err := discoveryFormat(c, opts)
			if err != nil {
				return err
			}
			core, err := applicationCore(opts, registry, false)
			if err != nil {
				return err
			}
			return emitDocument(c, format, map[string]any{"providers": core.Providers().Providers})
		},
	}
}

// newToolsCommand lists the tools of one namespace. A tool is one provider-qualified operation; the
// provider prefix is its namespace, not a command tree of its own, so the number of commands never grows
// with the catalog.
func newToolsCommand(opts *Options, registry *capability.Registry) *cobra.Command {
	var query string
	cmd := &cobra.Command{
		Use:   "tools <namespace>",
		Short: "List the tools of one namespace",
		Long: "Tools lists every tool of one namespace with its ID, its title, and whether it reads or changes\n" +
			"the remote system. It is answered from the local configuration alone: no provider is contacted\n" +
			"and no secret is read.\n\n" +
			"The namespace argument is the provider prefix of the tool IDs; 'callbell providers' lists the\n" +
			"namespaces. --query answers the same form for a targeted search and may be used without a\n" +
			"namespace, keeping only the tools whose ID, title, description, or tags contain every term.\n\n" +
			"Everything else about a tool, including its schemas and which connections can run it, is one\n" +
			"'callbell tool <tool-id>' away.\n\n" +
			"The output is " + toonContract + " with LF line endings. --output json returns the same data as\n" +
			"JSON.",
		Args: atMostOneArg("tool namespace"),
		RunE: func(c *cobra.Command, args []string) error {
			format, err := discoveryFormat(c, opts)
			if err != nil {
				return err
			}
			// Without a namespace and without a query this would print the whole catalog, which is the
			// answer the cascade exists to avoid. Naming the first step is more useful than that list.
			if len(args) == 0 && query == "" {
				return &UsageError{errors.New(
					"tools needs a namespace or --query; run 'callbell providers' to list the namespaces")}
			}
			request := application.SearchRequest{Query: query, Connection: opts.Connection}
			if len(args) == 1 {
				if _, ok := registry.ProviderMetadata(args[0]); !ok {
					return &UsageError{fmt.Errorf("unknown tool namespace %q", args[0])}
				}
				request.Provider = args[0]
			}
			core, err := applicationCore(opts, registry, false)
			if err != nil {
				return err
			}
			response, err := core.Tools(request)
			if err != nil {
				return classifyUserError(err)
			}
			return emitDocument(c, format, map[string]any{"tools": response.Tools})
		},
	}
	cmd.Flags().StringVar(&query, "query", "", "keep only the tools matching every term of this text")
	return cmd
}

// newToolCommand describes exactly one tool. The tool ID is the only argument: there is no second verb
// below it, because the tool itself is the leaf of the public taxonomy.
func newToolCommand(opts *Options, registry *capability.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "tool <tool-id>",
		Short: "Describe one tool contract",
		Long: "Tool prints the complete contract of one tool: version, description, tags, input and output\n" +
			"schema, risk metadata, secret-free examples, and the connections that can run it. Every\n" +
			"connection is named with its stable invoke value and the optional one-line description its\n" +
			"owner maintains. It is answered from the local configuration alone: no provider is contacted\n" +
			"and no secret is read.\n\n" +
			"The output is " + toonContract + " with LF line endings. --output json returns the same data as\n" +
			"JSON.",
		Args: exactlyOneArg("tool ID"),
		RunE: func(c *cobra.Command, args []string) error {
			format, err := discoveryFormat(c, opts)
			if err != nil {
				return err
			}
			core, err := applicationCore(opts, registry, false)
			if err != nil {
				return err
			}
			response, err := core.Describe(application.DescribeRequest{
				Operation: args[0], Connection: opts.Connection,
			})
			if err != nil {
				return classifyUserError(err)
			}
			return emitDocument(c, format, map[string]any{
				"tool": response.Operation, "connections": response.Connections,
			})
		},
	}
}

// newInvokeCommand runs one tool. The tool ID is positional and only the schema-dependent arguments come
// from stdin, so an agent can never smuggle a route, a header, or a credential past the contract.
func newInvokeCommand(opts *Options, registry *capability.Registry) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "invoke <tool-id>",
		Short: "Invoke one tool",
		Long: "Invoke runs the named tool through the application core, which validates the arguments against\n" +
			"the input schema before it selects a connection or contacts a provider.\n\n" +
			"The arguments are read from stdin as exactly one JSON object; empty input invokes the tool\n" +
			"without arguments. --connection selects the route when the configuration leaves more than one\n" +
			"possibility, and --confirm carries the confirmation a mutating tool requires for this request.\n\n" +
			"The result is written to stdout as JSON. Diagnostics and the audit event of a confirmed\n" +
			"mutation go to stderr.",
		Args: exactlyOneArg("tool ID"),
		RunE: func(c *cobra.Command, args []string) error {
			arguments, err := readInvokeArguments(c.InOrStdin())
			if err != nil {
				return &UsageError{err}
			}
			core, err := applicationCore(opts, registry, true)
			if err != nil {
				return err
			}
			var audit bytes.Buffer
			core.SetAudit(&audit)
			response, err := core.Invoke(c.Context(), application.InvokeRequest{
				Operation: args[0], Connection: opts.Connection, Arguments: arguments, Confirmed: confirm,
			})
			if err != nil {
				return withAudit(classifyUserError(err), audit.Bytes())
			}
			if err := writeEnvelope(c.OutOrStdout(), response); err != nil {
				return withAudit(err, audit.Bytes())
			}
			writeAudit(c.ErrOrStderr(), audit.Bytes(), opts.Redactor)
			return nil
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false,
		"confirm this exact request; required by a tool whose contract demands confirmation")
	return cmd
}

// discoveryFormat resolves the output format of the discovery commands. TOON is the default because these
// commands exist for agents; an explicit --output json is the interoperable alternative. The three scalar
// formats cannot render a nested tool contract, so asking for one is a usage error rather than a partial
// answer.
func discoveryFormat(c *cobra.Command, opts *Options) (output.Format, error) {
	if !c.Flags().Changed("output") {
		return output.FormatTOON, nil
	}
	switch opts.Format {
	case output.FormatTOON, output.FormatJSON:
		return opts.Format, nil
	}
	return "", &UsageError{fmt.Errorf("--output %s cannot render a tool contract, want %s or %s",
		opts.Format, output.FormatTOON, output.FormatJSON)}
}

// emitDocument writes one discovery document. Both formats render the same normalized JSON value, so the
// TOON default and --output json cannot drift apart: TOON is a rendering of the JSON data model here, not
// a second contract.
func emitDocument(c *cobra.Command, format output.Format, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode discovery document: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("encode discovery document: %w", err)
	}
	if format == output.FormatJSON {
		return json.NewEncoder(c.OutOrStdout()).Encode(document)
	}
	toon, err := output.MarshalTOON(document)
	if err != nil {
		return fmt.Errorf("encode discovery document: %w", err)
	}
	_, err = c.OutOrStdout().Write(append(toon, '\n'))
	return err
}

func exactlyOneArg(what string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return &UsageError{fmt.Errorf("expected exactly one %s, got %d", what, len(args))}
		}
		return nil
	}
}

func atMostOneArg(what string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) > 1 {
			return &UsageError{fmt.Errorf("expected at most one %s, got %d", what, len(args))}
		}
		return nil
	}
}
