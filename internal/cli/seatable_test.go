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

// seatableConfig configures two SeaTable instances, two bases of the cloud instance, and two API tokens
// for the same base, next to a BookStack connection. No instance is ever contacted here: every request in
// this file is refused by the core before any provider I/O, so the tests need no server.
func seatableConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	return writeConfig(t, `version: 1
services:
  tables-cloud:
    provider: seatable
    base_url: https://cloud.seatable.io
  tables-onprem:
    provider: seatable
    base_url: https://seatable.example.invalid
  wiki:
    provider: bookstack
    base_url: https://wiki.example.invalid
credentials:
  sales-base-reader:
    type: env
    values:
      api-token: SEATABLE_SALES_TOKEN
  sales-base-auditor:
    type: env
    values:
      api-token: SEATABLE_SALES_AUDIT_TOKEN
  support-base-reader:
    type: env
    values:
      api-token: SEATABLE_SUPPORT_TOKEN
  onprem-base-reader:
    type: env
    values:
      api-token: SEATABLE_ONPREM_TOKEN
  reader:
    type: env
    values:
      token-id: WIKI_TOKEN_ID
      token-secret: WIKI_TOKEN_SECRET
connections:
  sales-rows:
    service: tables-cloud
    credential: sales-base-reader
    target: Kunden
  sales-rows-audit:
    service: tables-cloud
    credential: sales-base-auditor
    target: Kunden/Aktive
  support-rows:
    service: tables-cloud
    credential: support-base-reader
    target: "id:0001"
  onprem-rows:
    service: tables-onprem
    credential: onprem-base-reader
    target: Tickets
  wiki:
    service: wiki
    credential: reader
defaults: {}
`)
}

// runSeatableCLI drives the real command tree and the shipped registry with a resolver that counts secret
// lookups, so a run can prove it resolved no API token.
func runSeatableCLI(t *testing.T, reads *atomic.Int32, input string, args ...string) (int, string, string) {
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

// The SeaTable namespace publishes exactly the two read-only row tools, with the connections that can run
// them and without contacting an instance.
func TestSeaTableToolsAreDiscoverable(t *testing.T) {
	path := seatableConfig(t)
	var reads atomic.Int32

	code, stdout, stderr := runSeatableCLI(t, &reads, "", "tools", "seatable", "--config", path)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"tools[2]{connections,id}:", "4,seatable.rows.get", "4,seatable.rows.list",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tools output does not contain %q:\n%s", want, stdout)
		}
	}
	if got := toolIDs(t, string(runSeatableJSON(t, "", "tools", "seatable", "--config", path))); len(got) != 2 {
		t.Errorf("seatable tools = %v, want exactly the two row tools", got)
	}
	if reads.Load() != 0 {
		t.Errorf("secret lookups = %d, want 0", reads.Load())
	}
}

// One tool document carries the complete contract. The base, the table, and the view are not part of it:
// an agent can name neither them nor a URL through the contract.
func TestSeaTableToolContractKeepsBaseTableAndViewOutOfTheArguments(t *testing.T) {
	path := seatableConfig(t)

	code, stdout, stderr := runSeatableCLI(t, nil, "", "tool", "seatable.rows.list", "--config", path)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"id: seatable.rows.list", "version: 1", "effect: read", "idempotency: safe",
		"confirmation: none", "requires_explicit_connection: true", "start", "limit",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tool contract does not contain %q:\n%s", want, stdout)
		}
	}

	document := runSeatableJSON(t, "", "tool", "seatable.rows.list", "--config", path)
	var described struct {
		Tool struct {
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tool"`
		Connections []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(document, &described); err != nil {
		t.Fatalf("tool document = %s: %v", document, err)
	}
	for _, forbidden := range []string{
		"base_url", "base_uuid", "table_id", "table_name", "view_id", "view_name", "token", "sql",
	} {
		if strings.Contains(string(described.Tool.InputSchema), forbidden) {
			t.Errorf("the input schema offers %q: %s", forbidden, described.Tool.InputSchema)
		}
	}
	if len(described.Connections) != 4 {
		t.Errorf("connections = %v, want all four configured SeaTable connections", described.Connections)
	}
}

// Everything the contract does not allow is refused before a connection is opened and before any secret is
// resolved, so no request can reach an instance. Four connections are never picked silently.
func TestSeaTableInvokeRefusalsHappenBeforeSecretsAndProviderIO(t *testing.T) {
	path := seatableConfig(t)

	tests := []struct {
		name  string
		input string
		args  []string
		code  string
	}{
		{
			name: "four connections without an explicit one", input: `{}`,
			args: []string{"invoke", "seatable.rows.list", "--config", path},
			code: "connection-selection",
		},
		{
			name: "a table as an argument", input: `{"table_name":"Gehaelter"}`,
			args: []string{"invoke", "seatable.rows.list", "--connection", "sales-rows", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a base as an argument", input: `{"base_uuid":"5c264e76-0e5a-448a-9f34-580b551364ca"}`,
			args: []string{"invoke", "seatable.rows.list", "--connection", "sales-rows", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a page size beyond the bound", input: `{"limit":1000}`,
			args: []string{"invoke", "seatable.rows.list", "--connection", "sales-rows", "--config", path},
			code: "invalid-request",
		},
		{
			name: "an offset beyond the bound", input: `{"start":10001}`,
			args: []string{"invoke", "seatable.rows.list", "--connection", "sales-rows", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a row identifier outside the documented form", input: `{"row_id":"1"}`,
			args: []string{"invoke", "seatable.rows.get", "--connection", "sales-rows", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a row identifier that traverses the route", input: `{"row_id":"../../../metadata/x"}`,
			args: []string{"invoke", "seatable.rows.get", "--connection", "sales-rows", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a connection of another provider", input: `{"row_id":"Qtf7xPmoRaiFyQPO1aENTj"}`,
			args: []string{"invoke", "seatable.rows.get", "--connection", "wiki", "--config", path},
			code: "unsupported-capability",
		},
		{
			name: "an unknown connection", input: `{}`,
			args: []string{"invoke", "seatable.rows.list", "--connection", "absent", "--config", path},
			code: "unknown-connection",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reads atomic.Int32
			code, stdout, stderr := runSeatableCLI(t, &reads, tt.input, tt.args...)
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

// The MCP broker and the tool CLI publish the same SeaTable contracts and the same error codes, because
// both use the application core.
func TestSeaTableMCPAndCLIShareTheCoreContracts(t *testing.T) {
	path := seatableConfig(t)
	options := &Options{Config: path, Redactor: &redact.Redactor{}}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"search","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.search","arguments":{"provider":"seatable"}}}`,
		`{"jsonrpc":"2.0","id":"describe","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.describe","arguments":{"operation":"seatable.rows.list","version":1}}}`,
		`{"jsonrpc":"2.0","id":"invoke","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.invoke","arguments":{"operation":"seatable.rows.get",` +
			`"connection":"sales-rows","arguments":{"row_id":"too-short"}}}}`,
		`{"jsonrpc":"2.0","id":"ambiguous","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.invoke","arguments":{"operation":"seatable.rows.list","arguments":{}}}}`,
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
	if len(searched.Operations) != 2 || searched.Operations[0].ID != "seatable.rows.get" ||
		searched.Operations[1].ID != "seatable.rows.list" {
		t.Fatalf("search operations = %+v", searched.Operations)
	}

	describe := toolResultFrom(t, responses[`"describe"`])
	describedByCLI := runSeatableJSON(t, "", "tool", "seatable.rows.list", "--config", path, "--output", "json")
	assertMCPParity(t, describedByCLI, "tool", describe.Structured, "operation")
	assertMCPParity(t, describedByCLI, "connections", describe.Structured, "connections")

	invoke := toolResultFrom(t, responses[`"invoke"`])
	if !invoke.IsError {
		t.Fatalf("invoke = %+v, want the invalid identifier refused", invoke)
	}
	mcpCode, _, _ := strings.Cut(invoke.Content[0].Text, ":")
	code, _, cliStderr := runSeatableCLI(t, nil, `{"row_id":"too-short"}`,
		"invoke", "seatable.rows.get", "--connection", "sales-rows", "--config", path)
	cliCode, _, _ := strings.Cut(strings.TrimPrefix(strings.SplitN(cliStderr, "\n", 2)[0], "callbell: "), ":")
	if code != exitUsage || mcpCode != cliCode || cliCode != "invalid-request" {
		t.Fatalf("MCP code=%q CLI code=%q exit=%d", mcpCode, cliCode, code)
	}

	// The broker does not pick one of the four bases either.
	ambiguous := toolResultFrom(t, responses[`"ambiguous"`])
	if !ambiguous.IsError || !strings.HasPrefix(ambiguous.Content[0].Text, "connection-selection:") {
		t.Fatalf("ambiguous invoke = %+v, want an explicit connection to be required", ambiguous)
	}
}

func runSeatableJSON(t *testing.T, input string, args ...string) []byte {
	t.Helper()
	if len(args) > 0 && args[len(args)-1] != "json" {
		args = append(args, "--output", "json")
	}
	code, stdout, stderr := runSeatableCLI(t, nil, input, args...)
	if code != exitOK || stderr != "" {
		t.Fatalf("CLI %v exit=%d stderr=%q", args, code, stderr)
	}
	return []byte(stdout)
}
