package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/provider/bookstack"
)

const (
	// domainKnowledge is the domain these commands resolve their default connection for.
	domainKnowledge     = "knowledge"
	capabilityPagesList = bookstack.Provider + ".pages.list"
	capabilityPagesGet  = bookstack.Provider + ".pages.get"
)

func newKnowledgeCommand(opts *Options, reg *capability.Registry) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge",
		Short: "Read from a knowledge base",
		Args:  noArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}

	pages := &cobra.Command{
		Use:   "pages",
		Short: "Work with the pages of a knowledge base",
		Args:  noArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}

	var offset int
	list := &cobra.Command{
		Use:   "list",
		Short: "List pages",
		Long: "The number of pages follows --limit and the projection follows --fields; both are passed to\n" +
			"the provider. --limit 0 returns every page.",
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := validateCapabilityFields(reg, bookstack.Provider, capabilityPagesList, opts.Fields); err != nil {
				return err
			}
			limit := opts.Limit
			if limit < 0 {
				limit = 0
			}
			result, err := invokeKnowledge(c.Context(), opts, reg, capabilityPagesList, struct {
				Limit  int `json:"limit"`
				Offset int `json:"offset"`
			}{Limit: limit, Offset: offset}, true)
			if err != nil {
				return err
			}
			return emit(c, opts, result)
		},
	}
	list.Flags().IntVar(&offset, "offset", 0, "number of pages to skip")

	get := &cobra.Command{
		Use:   "get <id>",
		Short: "Read one page",
		Long: "Page content is untrusted data. It is passed through to the selected output format and is\n" +
			"never rendered or interpreted.",
		Args: exactlyOneArg,
		RunE: func(c *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return &UsageError{fmt.Errorf("page id %q is not a positive number", args[0])}
			}
			if err := validateCapabilityFields(reg, bookstack.Provider, capabilityPagesGet, opts.Fields); err != nil {
				return err
			}
			result, err := invokeKnowledge(c.Context(), opts, reg, capabilityPagesGet, struct {
				ID int64 `json:"id"`
			}{ID: id}, false)
			if err != nil {
				return err
			}
			return emit(c, opts, result)
		},
	}

	pages.AddCommand(list, get)
	cmd.AddCommand(pages)
	return cmd
}

// invokeKnowledge preserves the Fachkommando connection and output contracts while dispatching through
// the same application operation path as callbell invoke.
func invokeKnowledge(ctx context.Context, opts *Options, reg *capability.Registry, operation string,
	arguments any, collection bool) (output.Result, error) {
	descriptor, _, ok := reg.Lookup(operation)
	if !ok {
		return nil, classifyUserError(&application.UnknownOperationError{Operation: operation})
	}
	path, err := config.Path(opts.Config)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path, reg)
	if err != nil {
		return nil, classifyUserError(err)
	}
	resolved, err := cfg.Resolve(opts.Connection, domainKnowledge)
	if err != nil {
		return nil, classifyUserError(err)
	}

	if resolved.Provider != bookstack.Provider {
		return nil, classifyUserError(&capability.UnsupportedError{
			Connection: resolved.Name,
			Capability: domainKnowledge + ".pages",
		})
	}
	secrets, err := opts.resolver()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("encode %s arguments: %w", operation, err)
	}
	response, err := application.New(reg, cfg, secrets, opts.Redactor).Invoke(ctx, application.InvokeRequest{
		Operation: operation, Version: descriptor.Version, Connection: resolved.Name, Arguments: raw,
	})
	if err != nil {
		return nil, classifyUserError(err)
	}
	return decodeKnowledgeResult(response.Result, capabilityFieldNames(descriptor), collection)
}

func decodeKnowledgeResult(raw json.RawMessage, fields []string, collection bool) (output.Result, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if collection {
		var values []map[string]any
		if err := decoder.Decode(&values); err != nil {
			return nil, fmt.Errorf("decode operation result: %w", err)
		}
		rows := make([]output.Row, len(values))
		for i, value := range values {
			rows[i] = output.Row{}
			for _, field := range fields {
				rows[i][field] = operationScalar(value[field])
			}
		}
		return output.Collection{Columns: fields, Rows: rows}, nil
	}

	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode operation result: %w", err)
	}
	result := output.Object{Fields: make([]output.Field, len(fields))}
	for i, field := range fields {
		result.Fields[i] = output.Field{Name: field, Value: operationScalar(value[field])}
	}
	return result, nil
}

func operationScalar(value any) any {
	number, ok := value.(json.Number)
	if !ok {
		return value
	}
	if integer, err := number.Int64(); err == nil {
		return integer
	}
	floating, _ := number.Float64()
	return floating
}
