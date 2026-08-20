package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
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
			client, err := openKnowledge(opts, reg)
			if err != nil {
				return err
			}
			if err := validateCapabilityFields(reg, bookstack.Provider, capabilityPagesList, opts.Fields); err != nil {
				return err
			}
			result, err := client.ListPages(c.Context(), opts.Limit, offset)
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
			client, err := openKnowledge(opts, reg)
			if err != nil {
				return err
			}
			if err := validateCapabilityFields(reg, bookstack.Provider, capabilityPagesGet, opts.Fields); err != nil {
				return err
			}
			result, err := client.GetPage(c.Context(), strconv.FormatInt(id, 10))
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

// openKnowledge resolves the connection for the knowledge domain and opens its provider.
func openKnowledge(opts *Options, reg *capability.Registry) (*bookstack.Client, error) {
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
	client, err := bookstack.Open(resolved, secrets, opts.Redactor)
	if err != nil {
		return nil, classifyUserError(err)
	}
	return client, nil
}
