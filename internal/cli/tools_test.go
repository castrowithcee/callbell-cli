package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// catalogEnvironment configures one BookStack and one Telegram connection against local servers that fail
// the test if discovery ever calls them. Discovery is a local catalog view: it must answer from the
// configuration alone.
func catalogEnvironment(t *testing.T) (string, *atomic.Int32, string) {
	t.Helper()
	const providerCanary = "provider-body-canary-4b71"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":1,"name":"` + providerCanary + `"}],"total":1}`))
	}))
	t.Cleanup(server.Close)

	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, fmt.Sprintf(`version: 1
services:
  wiki:
    provider: bookstack
    base_url: %s
  telegram:
    provider: telegram
    base_url: %s
credentials:
  reader:
    type: env
    values:
      token-id: TOOLS_TOKEN_ID
      token-secret: TOOLS_TOKEN_SECRET
  bot:
    type: env
    values:
      bot-token: TOOLS_BOT_TOKEN
connections:
  wiki:
    service: wiki
    credential: reader
  alerts:
    service: telegram
    credential: bot
    target: "-1009900112233"
defaults: {}
`, server.URL, server.URL))
	return cfg, &calls, providerCanary
}

// runTools drives the real command tree and the shipped provider registry with a credential resolver that
// counts every secret lookup, so a discovery run can prove it read none.
func runTools(t *testing.T, reads *atomic.Int32, args ...string) (int, string, string) {
	t.Helper()
	redactor := &redact.Redactor{}
	lookup := func(string) string {
		if reads != nil {
			reads.Add(1)
		}
		return ""
	}
	var stdout, stderr bytes.Buffer
	opts := &Options{
		Input: strings.NewReader(""), Redactor: redactor,
		Secrets: secret.NewWith(lookup, nil, nil, redactor),
	}
	code := run(newRootCommand(opts, defaultRegistry()), opts, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// toonOfJSON renders a JSON document with the encoder that the TOON specification tests verify. A command
// whose TOON output equals this rendering carries exactly the data of its JSON output.
func toonOfJSON(t *testing.T, document string) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("output is not valid JSON: %v: %s", err, document)
	}
	encoded, err := output.MarshalTOON(value)
	if err != nil {
		t.Fatalf("MarshalTOON() = %v", err)
	}
	return string(encoded) + "\n"
}

func jsonEqual(a, b []byte) bool {
	var first, second any
	return json.Unmarshal(a, &first) == nil && json.Unmarshal(b, &second) == nil &&
		reflect.DeepEqual(first, second)
}

func toolIDs(t *testing.T, stdout string) []string {
	t.Helper()
	var document struct {
		Tools []struct {
			ID string `json:"id"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("tools output is not valid JSON: %v: %s", err, stdout)
	}
	ids := make([]string, len(document.Tools))
	for i, tool := range document.Tools {
		ids[i] = tool.ID
	}
	return ids
}

// Acceptance 1: the catalog is a deterministic local TOON view of the configured BookStack and Telegram
// tools, produced without any provider I/O.
func TestToolsListsTheConfiguredCatalogAsTOON(t *testing.T) {
	cfg, calls, _ := catalogEnvironment(t)

	code, stdout, stderr := runTools(t, nil, "tools", "--config", cfg)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.HasPrefix(stdout, "tools[3]:\n") || !strings.HasSuffix(stdout, "\n") ||
		strings.Contains(stdout, "\r") {
		t.Errorf("stdout = %q, want an LF TOON document with a three-element tools header", stdout)
	}
	for _, want := range []string{
		"id: bookstack.pages.get", "id: bookstack.pages.list", "id: telegram.messages.send",
		"connections[1]: wiki", "connections[1]: alerts", "effect: create", "effect: read",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
	for i := 0; i < 3; i++ {
		if _, repeat, _ := runTools(t, nil, "tools", "--config", cfg); repeat != stdout {
			t.Fatalf("run %d = %q, want %q", i+2, repeat, stdout)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("provider calls = %d, want 0", got)
	}
}

// Acceptance 2: the namespace argument and --query restrict the same catalog.
func TestToolsFiltersByNamespaceAndQuery(t *testing.T) {
	cfg, _, _ := catalogEnvironment(t)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"complete catalog", nil, []string{"bookstack.pages.get", "bookstack.pages.list", "telegram.messages.send"}},
		{"namespace", []string{"bookstack"}, []string{"bookstack.pages.get", "bookstack.pages.list"}},
		{"namespace telegram", []string{"telegram"}, []string{"telegram.messages.send"}},
		{"query", []string{"--query", "pages"}, []string{"bookstack.pages.get", "bookstack.pages.list"}},
		{"namespace and query", []string{"bookstack", "--query", "list"}, []string{"bookstack.pages.list"}},
		{"query without a match", []string{"--query", "absent"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"tools"}, tt.args...)
			code, stdout, stderr := runTools(t, nil, append(args, "--config", cfg, "--output", "json")...)
			if code != exitOK || stderr != "" {
				t.Fatalf("exit=%d stderr=%q", code, stderr)
			}
			if got := toolIDs(t, stdout); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("tools = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("an unknown namespace is a usage error", func(t *testing.T) {
		code, stdout, stderr := runTools(t, nil, "tools", "bookstck", "--config", cfg)
		if code != exitUsage || stdout != "" || !strings.Contains(stderr, `unknown tool namespace "bookstck"`) {
			t.Errorf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("a second argument is a usage error", func(t *testing.T) {
		code, _, stderr := runTools(t, nil, "tools", "bookstack", "telegram", "--config", cfg)
		if code != exitUsage || !strings.Contains(stderr, "expected at most one tool namespace") {
			t.Errorf("exit=%d stderr=%q", code, stderr)
		}
	})
}

// Acceptance 3: one tool document carries the complete contract, and the TOON default and --output json
// are two renderings of the same data.
func TestToolDescribesOneCompleteContract(t *testing.T) {
	cfg, _, _ := catalogEnvironment(t)

	code, toon, stderr := runTools(t, nil, "tool", "bookstack.pages.list", "--config", cfg)
	if code != exitOK || stderr != "" {
		t.Fatalf("TOON exit=%d stderr=%q", code, stderr)
	}
	code, jsonOut, stderr := runTools(t, nil, "tool", "bookstack.pages.list", "--config", cfg, "--output", "json")
	if code != exitOK || stderr != "" {
		t.Fatalf("JSON exit=%d stderr=%q", code, stderr)
	}
	if got := toonOfJSON(t, jsonOut); got != toon {
		t.Errorf("TOON output = %q, want the TOON rendering of the JSON data %q", toon, got)
	}

	var document struct {
		Tool        capability.Descriptor `json:"tool"`
		Connections []string              `json:"connections"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &document); err != nil {
		t.Fatalf("tool output is not valid JSON: %v", err)
	}
	tool := document.Tool
	if tool.ID != "bookstack.pages.list" || tool.Version != 1 || tool.Description == "" ||
		len(tool.Tags) == 0 || len(tool.InputSchema) == 0 || len(tool.OutputSchema) == 0 ||
		len(tool.Examples) == 0 || len(tool.Fields) == 0 || tool.Risk.Effect != capability.EffectRead ||
		tool.Risk.Idempotency != capability.IdempotencySafe ||
		tool.Risk.Confirmation != capability.ConfirmationNone || tool.Risk.DataSensitivity == "" {
		t.Errorf("tool contract = %+v", tool)
	}
	if !reflect.DeepEqual(document.Connections, []string{"wiki"}) {
		t.Errorf("connections = %v, want [wiki]", document.Connections)
	}

	t.Run("there is no verb below a tool", func(t *testing.T) {
		code, stdout, stderr := runTools(t, nil, "tool", "show", "bookstack.pages.list", "--config", cfg)
		if code != exitUsage || stdout != "" || !strings.Contains(stderr, "expected exactly one tool ID") {
			t.Errorf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("an unknown tool is a stable error", func(t *testing.T) {
		code, stdout, stderr := runTools(t, nil, "tool", "absent.pages.list", "--config", cfg)
		if code != exitUsage || stdout != "" || !strings.HasPrefix(stderr, "callbell: unknown-operation:") {
			t.Errorf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("a scalar format cannot render a contract", func(t *testing.T) {
		code, stdout, stderr := runTools(t, nil, "tool", "bookstack.pages.list", "--config", cfg,
			"--output", "table")
		if code != exitUsage || stdout != "" ||
			!strings.Contains(stderr, "--output table cannot render a tool contract") {
			t.Errorf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})
}

// Acceptance 4: discovery reads no secret and publishes no secret, provider body, or payload canary.
func TestDiscoveryReadsNoSecretsAndLeaksNoCanary(t *testing.T) {
	const (
		tokenCanary  = "tools-token-canary-6d13"
		secretCanary = "tools-secret-canary-9f04"
		botCanary    = "tools-bot-canary-2e88"
	)
	cfg, calls, providerCanary := catalogEnvironment(t)
	t.Setenv("TOOLS_TOKEN_ID", tokenCanary)
	t.Setenv("TOOLS_TOKEN_SECRET", secretCanary)
	t.Setenv("TOOLS_BOT_TOKEN", botCanary)

	var reads atomic.Int32
	for _, args := range [][]string{
		{"tools", "--config", cfg},
		{"tools", "bookstack", "--config", cfg, "--output", "json"},
		{"tool", "telegram.messages.send", "--config", cfg},
		{"tool", "bookstack.pages.list", "--config", cfg, "--output", "json"},
	} {
		code, stdout, stderr := runTools(t, &reads, args...)
		if code != exitOK || stderr != "" {
			t.Fatalf("%v exit=%d stderr=%q", args, code, stderr)
		}
		for _, canary := range []string{
			tokenCanary, secretCanary, botCanary, providerCanary, "-1009900112233",
		} {
			if strings.Contains(stdout, canary) || strings.Contains(stderr, canary) {
				t.Errorf("%v published the canary %q", args, canary)
			}
		}
	}
	if reads.Load() != 0 {
		t.Errorf("secret lookups = %d, want 0", reads.Load())
	}
	if calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", calls.Load())
	}
}

// syntheticRegistry registers count operations of one synthetic provider, so a test can grow the catalog
// without growing anything else.
func syntheticRegistry(t *testing.T, count int) *capability.Registry {
	t.Helper()
	reg := capability.NewRegistry()
	if err := reg.RegisterProvider(config.ProviderMetadata{ID: "synthetic", Name: "Synthetic"}, nil); err != nil {
		t.Fatalf("RegisterProvider() = %v", err)
	}
	operations := make([]capability.Operation, count)
	for i := range operations {
		operations[i] = capability.Operation{
			Descriptor: capability.Descriptor{
				ID: fmt.Sprintf("synthetic.object%d.list", i), Version: 1, Provider: "synthetic",
				Description: fmt.Sprintf("List synthetic object %d", i),
				Risk: capability.Risk{
					Effect: capability.EffectRead, Idempotency: capability.IdempotencySafe,
					Confirmation: capability.ConfirmationNone, DataSensitivity: "synthetic",
				},
				InputSchema:  json.RawMessage(`{"type":"object"}`),
				OutputSchema: json.RawMessage(`{"type":"array"}`),
			},
			Handler: func(context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor,
				json.RawMessage) (any, error) {
				return []any{}, nil
			},
		}
	}
	if err := reg.Register("synthetic", operations...); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	return reg
}

func commandNames(cmd *cobra.Command) []string {
	names := make([]string, 0, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		names = append(names, sub.Name())
		for _, child := range sub.Commands() {
			names = append(names, sub.Name()+" "+child.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Acceptance 7: a large catalog grows the catalog data only. The command tree and the MCP tool list stay
// exactly as they are.
func TestLargeCatalogGrowsOnlyTheData(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, "version: 1\n")

	small := newRootCommand(&Options{}, syntheticRegistry(t, 1))
	large := newRootCommand(&Options{}, syntheticRegistry(t, 128))
	if !reflect.DeepEqual(commandNames(small), commandNames(large)) {
		t.Errorf("commands = %v, want %v", commandNames(large), commandNames(small))
	}
	if len(mcpTools()) != 3 {
		t.Errorf("MCP tools = %d, want the three fixed broker tools", len(mcpTools()))
	}

	var stdout, stderr bytes.Buffer
	opts := &Options{Input: strings.NewReader(""), Redactor: &redact.Redactor{}}
	code := run(newRootCommand(opts, syntheticRegistry(t, 128)), opts,
		[]string{"tools", "--config", cfg, "--output", "json"}, &stdout, &stderr)
	if code != exitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if got := len(toolIDs(t, stdout.String())); got != 128 {
		t.Errorf("catalog entries = %d, want 128", got)
	}
}

// Acceptance 6: the public surface is the tool taxonomy. The replaced trees are gone from the command
// tree, from the help, and from the generated manpages.
func TestPublicSurfaceIsTheToolTaxonomy(t *testing.T) {
	root := DocumentationCommand("test")
	want := []string{
		"config", "config validate", "credential", "credential delete", "credential set",
		"invoke", "mcp", "tool", "tools", "tui", "update",
	}
	if got := commandNames(root); !reflect.DeepEqual(got, want) {
		t.Errorf("commands = %v, want %v", got, want)
	}

	manuals := t.TempDir()
	header := &doc.GenManHeader{Section: "1", Source: "Callbell CLI test", Manual: "Callbell CLI Manual"}
	if err := doc.GenManTree(root, header, manuals); err != nil {
		t.Fatalf("GenManTree() = %v", err)
	}
	pages := map[string]bool{}
	entries, err := os.ReadDir(manuals)
	if err != nil {
		t.Fatalf("ReadDir() = %v", err)
	}
	for _, entry := range entries {
		pages[entry.Name()] = true
	}
	for _, page := range []string{"callbell-tools.1", "callbell-tool.1", "callbell-invoke.1"} {
		if !pages[page] {
			t.Errorf("manpage %s is missing: %v", page, pages)
		}
	}
	for _, page := range []string{
		"callbell-capabilities.1", "callbell-search.1", "callbell-describe.1", "callbell-knowledge.1",
		"callbell-tool-show.1",
	} {
		if pages[page] {
			t.Errorf("manpage %s still exists", page)
		}
	}

	var help bytes.Buffer
	root.SetOut(&help)
	if err := root.Help(); err != nil {
		t.Fatalf("Help() = %v", err)
	}
	for _, want := range []string{"callbell tools", "callbell tool <id>", "callbell invoke <id>", "TOON 4.1"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("help does not mention %q:\n%s", want, help.String())
		}
	}
	_, listed, found := strings.Cut(help.String(), "Available Commands:")
	if !found {
		t.Fatalf("help has no command list:\n%s", help.String())
	}
	listed, _, _ = strings.Cut(listed, "\nFlags:")
	for _, removed := range []string{"capabilities", "knowledge", "search", "describe"} {
		if strings.Contains(listed, "\n  "+removed+" ") {
			t.Errorf("help still offers the removed command %q:%s", removed, listed)
		}
	}
}

// The tool commands answer with the data the removed capabilities, describe, and search commands used to
// publish, which is the migration parity their removal rests on.
func TestToolCommandsKeepTheDataOfTheRemovedCommands(t *testing.T) {
	cfg, _, _ := catalogEnvironment(t)
	core, err := applicationCore(&Options{Config: cfg}, defaultRegistry(), false)
	if err != nil {
		t.Fatalf("applicationCore() = %v", err)
	}

	// capabilities listed name, risk, and description of every operation a connection offers.
	code, stdout, stderr := runTools(t, nil, "tools", "--config", cfg, "--output", "json")
	if code != exitOK || stderr != "" {
		t.Fatalf("tools exit=%d stderr=%q", code, stderr)
	}
	var listed struct {
		Tools []application.SearchHit `json:"tools"`
	}
	if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
		t.Fatalf("tools output = %q: %v", stdout, err)
	}
	catalog, err := core.Tools(application.SearchRequest{})
	if err != nil {
		t.Fatalf("Tools() = %v", err)
	}
	if !reflect.DeepEqual(listed.Tools, catalog.Operations) {
		t.Errorf("tools = %+v, want %+v", listed.Tools, catalog.Operations)
	}
	for _, descriptor := range defaultRegistry().All() {
		found := false
		for _, hit := range listed.Tools {
			found = found || (hit.ID == descriptor.ID && hit.Effect == descriptor.Risk.Effect &&
				hit.Description == descriptor.Description)
		}
		if !found {
			t.Errorf("tools is missing %q with its effect and description", descriptor.ID)
		}
	}

	// search filtered the same catalog by query.
	code, stdout, stderr = runTools(t, nil, "tools", "--config", cfg, "--query", "pages", "--output", "json")
	if code != exitOK || stderr != "" {
		t.Fatalf("query exit=%d stderr=%q", code, stderr)
	}
	searched, err := core.Search(application.SearchRequest{Query: "pages"})
	if err != nil {
		t.Fatalf("Search() = %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
		t.Fatalf("tools output = %q: %v", stdout, err)
	}
	if !reflect.DeepEqual(listed.Tools, searched.Operations) {
		t.Errorf("query result = %+v, want %+v", listed.Tools, searched.Operations)
	}

	// describe returned one complete descriptor and its connections.
	for _, id := range []string{"bookstack.pages.get", "bookstack.pages.list", "telegram.messages.send"} {
		code, stdout, stderr = runTools(t, nil, "tool", id, "--config", cfg, "--output", "json")
		if code != exitOK || stderr != "" {
			t.Fatalf("tool %s exit=%d stderr=%q", id, code, stderr)
		}
		var document struct {
			Tool        json.RawMessage `json:"tool"`
			Connections []string        `json:"connections"`
		}
		if err := json.Unmarshal([]byte(stdout), &document); err != nil {
			t.Fatalf("tool output = %q: %v", stdout, err)
		}
		described, err := core.Describe(application.DescribeRequest{Operation: id})
		if err != nil {
			t.Fatalf("Describe(%s) = %v", id, err)
		}
		wantOperation, err := json.Marshal(described.Operation)
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(document.Tool, wantOperation) {
			t.Errorf("tool %s = %s, want %s", id, document.Tool, wantOperation)
		}
		if !reflect.DeepEqual(document.Connections, described.Connections) {
			t.Errorf("tool %s connections = %v, want %v", id, document.Connections, described.Connections)
		}
	}
}
