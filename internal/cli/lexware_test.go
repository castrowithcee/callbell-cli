package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// lexwareConfig configures two Lexware connections with separate API keys next to a BookStack connection.
// The Lexware gateway is fixed and never contacted here: every request in this file is refused by the
// core before any provider I/O, so the tests need no server and reach no organization.
func lexwareConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	return writeConfig(t, `version: 1
services:
  books:
    provider: lexware
    base_url: https://api.lexware.io
  wiki:
    provider: bookstack
    base_url: https://wiki.example.invalid
credentials:
  books-primary:
    type: env
    values:
      api-key: LEXWARE_PRIMARY_KEY
  books-archive:
    type: env
    values:
      api-key: LEXWARE_ARCHIVE_KEY
  reader:
    type: env
    values:
      token-id: WIKI_TOKEN_ID
      token-secret: WIKI_TOKEN_SECRET
connections:
  accounting:
    service: books
    credential: books-primary
  accounting-archive:
    service: books
    credential: books-archive
  wiki:
    service: wiki
    credential: reader
defaults: {}
`)
}

// runLexwareCLI drives the real command tree and the shipped registry with a resolver that counts secret
// lookups, so a run can prove it resolved no API key.
func runLexwareCLI(t *testing.T, reads *atomic.Int32, input string, args ...string) (int, string, string) {
	t.Helper()
	redactor := &redact.Redactor{}
	lookup := func(string) string {
		if reads != nil {
			reads.Add(1)
		}
		return ""
	}
	var stdout, stderr bytes.Buffer
	options := &Options{
		Input: strings.NewReader(input), Redactor: redactor,
		Secrets: secret.NewWith(lookup, nil, nil, redactor),
	}
	code := run(newRootCommand(options, defaultRegistry()), options, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The Lexware namespace publishes exactly the two read-only tools of the confirmed workflow, each with the
// number of connections that can run it and without contacting the provider.
func TestLexwareToolsAreDiscoverable(t *testing.T) {
	path := lexwareConfig(t)
	var reads atomic.Int32

	code, stdout, stderr := runLexwareCLI(t, &reads, "", "tools", "lexware", "--config", path)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"tools[2]{effect,id,title}:", "read,lexware.invoices.get,", "read,lexware.invoices.list,",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tools output does not contain %q:\n%s", want, stdout)
		}
	}
	if got := toolIDs(t, string(runLexwareJSON(t, "", "tools", "lexware", "--config", path))); len(got) != 2 {
		t.Errorf("lexware tools = %v, want exactly the two invoice tools", got)
	}
	if reads.Load() != 0 {
		t.Errorf("secret lookups = %d, want 0", reads.Load())
	}
}

// One tool document carries the complete contract, including the controlled filters. The fixed voucher
// filters are not arguments: an agent can neither widen nor read them through the contract.
func TestLexwareToolContractExposesOnlyControlledFilters(t *testing.T) {
	path := lexwareConfig(t)

	code, stdout, stderr := runLexwareCLI(t, nil, "", "tool", "lexware.invoices.list", "--config", path)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"id: lexware.invoices.list", "version: 1", "effect: read", "idempotency: safe",
		"confirmation: none", "requires_explicit_connection: true", "voucher_number", "voucher_date_from",
		"page", "size", "sort", "direction",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tool contract does not contain %q:\n%s", want, stdout)
		}
	}

	document := runLexwareJSON(t, "", "tool", "lexware.invoices.list", "--config", path)
	var described struct {
		Tool struct {
			InputSchema json.RawMessage `json:"input_schema"`
			Arguments   []struct {
				Name string `json:"name"`
			} `json:"arguments"`
		} `json:"tool"`
		Connections []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(document, &described); err != nil {
		t.Fatalf("tool document = %s: %v", document, err)
	}
	for _, forbidden := range []string{"voucherType", "voucher_type", "voucherStatus", "voucher_status", "archived"} {
		if strings.Contains(string(described.Tool.InputSchema), forbidden) {
			t.Errorf("the input schema offers the fixed filter %q: %s", forbidden, described.Tool.InputSchema)
		}
	}
	if len(described.Connections) != 2 {
		t.Errorf("connections = %v, want both configured Lexware connections", described.Connections)
	}
}

// Everything the contract does not allow is refused before a connection is opened and before any secret
// is resolved, so no request can reach the Lexware gateway.
func TestLexwareInvokeRefusalsHappenBeforeSecretsAndProviderIO(t *testing.T) {
	path := lexwareConfig(t)

	tests := []struct {
		name  string
		input string
		args  []string
		code  string
	}{
		{
			name: "no explicit connection", input: `{}`,
			args: []string{"invoke", "lexware.invoices.list", "--config", path},
			code: "connection-selection",
		},
		{
			name: "an unknown argument", input: `{"voucher_status":"paid"}`,
			args: []string{"invoke", "lexware.invoices.list", "--connection", "accounting", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a page size beyond the bound", input: `{"size":250}`,
			args: []string{"invoke", "lexware.invoices.list", "--connection", "accounting", "--config", path},
			code: "invalid-request",
		},
		{
			name: "an identifier that is not a UUID", input: `{"id":"1"}`,
			args: []string{"invoke", "lexware.invoices.get", "--connection", "accounting", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a connection of another provider", input: `{"id":"f3d3ae48-30d9-4b56-973a-b3159cbe743c"}`,
			args: []string{"invoke", "lexware.invoices.get", "--connection", "wiki", "--config", path},
			code: "unsupported-capability",
		},
		{
			name: "an unknown connection", input: `{}`,
			args: []string{"invoke", "lexware.invoices.list", "--connection", "absent", "--config", path},
			code: "unknown-connection",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reads atomic.Int32
			code, stdout, stderr := runLexwareCLI(t, &reads, tt.input, tt.args...)
			if code != exitUsage || stdout != "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !strings.Contains(stderr, "callbell: "+tt.code+":") {
				t.Errorf("stderr = %q, want the %s code", stderr, tt.code)
			}
			if reads.Load() != 0 {
				t.Errorf("secret lookups = %d, want none before the request was refused", reads.Load())
			}
		})
	}
}

// The MCP broker and the tool CLI publish the same Lexware contracts and the same error codes, because
// both use the application core.
func TestLexwareMCPAndCLIShareTheCoreContracts(t *testing.T) {
	path := lexwareConfig(t)
	options := &Options{Config: path, Redactor: &redact.Redactor{}}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"search","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.search","arguments":{"provider":"lexware"}}}`,
		`{"jsonrpc":"2.0","id":"describe","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.describe","arguments":{"operation":"lexware.invoices.list","version":1}}}`,
		`{"jsonrpc":"2.0","id":"invoke","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.invoke","arguments":{"operation":"lexware.invoices.get",` +
			`"connection":"accounting","arguments":{"id":"not-a-uuid"}}}}`,
	}, "\n") + "\n"

	responses, stderr := runMCPWithOptions(t, defaultRegistry(), input, options)
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}

	search := toolResultFrom(t, responses[`"search"`])
	var searched struct {
		Operations []application.SearchHit `json:"operations"`
	}
	decodeRaw(t, search.Structured, &searched)
	if len(searched.Operations) != 2 || searched.Operations[0].ID != "lexware.invoices.get" ||
		searched.Operations[1].ID != "lexware.invoices.list" {
		t.Fatalf("search operations = %+v", searched.Operations)
	}

	describe := toolResultFrom(t, responses[`"describe"`])
	describedByCLI := runLexwareJSON(t, "", "tool", "lexware.invoices.list", "--config", path, "--output", "json")
	assertMCPParity(t, describedByCLI, "tool", describe.Structured, "operation")
	assertMCPParity(t, describedByCLI, "connections", describe.Structured, "connections")

	invoke := toolResultFrom(t, responses[`"invoke"`])
	if !invoke.IsError {
		t.Fatalf("invoke = %+v, want the invalid identifier refused", invoke)
	}
	mcpCode, _, _ := strings.Cut(invoke.Content[0].Text, ":")
	code, _, cliStderr := runLexwareCLI(t, nil, `{"id":"not-a-uuid"}`,
		"invoke", "lexware.invoices.get", "--connection", "accounting", "--config", path)
	cliCode, _, _ := strings.Cut(strings.TrimPrefix(strings.SplitN(cliStderr, "\n", 2)[0], "callbell: "), ":")
	if code != exitUsage || mcpCode != cliCode || cliCode != "invalid-request" {
		t.Fatalf("MCP code=%q CLI code=%q exit=%d", mcpCode, cliCode, code)
	}
}

func runLexwareJSON(t *testing.T, input string, args ...string) []byte {
	t.Helper()
	if len(args) > 0 && args[len(args)-1] != "json" {
		args = append(args, "--output", "json")
	}
	code, stdout, stderr := runLexwareCLI(t, nil, input, args...)
	if code != exitOK || stderr != "" {
		t.Fatalf("CLI %v exit=%d stderr=%q", args, code, stderr)
	}
	return []byte(stdout)
}
