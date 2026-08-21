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

// twentyConfig configures two Twenty workspaces, a managed one and a self-hosted one, with separate API
// keys, next to a BookStack connection. No workspace is ever contacted here: every request in this file is
// refused by the core before any provider I/O, so the tests need no server and reach no CRM.
func twentyConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	return writeConfig(t, `version: 1
services:
  crm-cloud:
    provider: twentycrm
    base_url: https://api.twenty.com
  crm-selfhosted:
    provider: twentycrm
    base_url: https://crm.example.invalid
  wiki:
    provider: bookstack
    base_url: https://wiki.example.invalid
credentials:
  crm-cloud-reader:
    type: env
    values:
      api-key: TWENTY_CLOUD_KEY
  crm-selfhosted-reader:
    type: env
    values:
      api-key: TWENTY_SELFHOSTED_KEY
  reader:
    type: env
    values:
      token-id: WIKI_TOKEN_ID
      token-secret: WIKI_TOKEN_SECRET
connections:
  crm:
    service: crm-cloud
    credential: crm-cloud-reader
  crm-internal:
    service: crm-selfhosted
    credential: crm-selfhosted-reader
  wiki:
    service: wiki
    credential: reader
defaults: {}
`)
}

// runTwentyCLI drives the real command tree and the shipped registry with a resolver that counts secret
// lookups, so a run can prove it resolved no API key.
func runTwentyCLI(t *testing.T, reads *atomic.Int32, input string, args ...string) (int, string, string) {
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

// The Twenty namespace publishes exactly the two read-only company tools, with the connections that can
// run them and without contacting a workspace.
func TestTwentyToolsAreDiscoverable(t *testing.T) {
	path := twentyConfig(t)
	var reads atomic.Int32

	code, stdout, stderr := runTwentyCLI(t, &reads, "", "tools", "twentycrm", "--config", path)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"tools[2]{connections,id}:", "2,twentycrm.companies.get", "2,twentycrm.companies.list",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tools output does not contain %q:\n%s", want, stdout)
		}
	}
	if got := toolIDs(t, string(runTwentyJSON(t, "", "tools", "twentycrm", "--config", path))); len(got) != 2 {
		t.Errorf("twentycrm tools = %v, want exactly the two company tools", got)
	}
	if reads.Load() != 0 {
		t.Errorf("secret lookups = %d, want 0", reads.Load())
	}
}

// One tool document carries the complete contract. The workspace-specific data model is not part of it:
// an agent can neither name a Twenty field nor a URL through the contract.
func TestTwentyToolContractExposesOnlyTheStableCoreProjection(t *testing.T) {
	path := twentyConfig(t)

	code, stdout, stderr := runTwentyCLI(t, nil, "", "tool", "twentycrm.companies.list", "--config", path)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"id: twentycrm.companies.list", "version: 1", "effect: read", "idempotency: safe",
		"confirmation: none", "requires_explicit_connection: true", "name_contains", "domain_contains",
		"limit", "sort", "direction", "cursor",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tool contract does not contain %q:\n%s", want, stdout)
		}
	}

	document := runTwentyJSON(t, "", "tool", "twentycrm.companies.list", "--config", path)
	var described struct {
		Tool struct {
			InputSchema  json.RawMessage `json:"input_schema"`
			OutputSchema json.RawMessage `json:"output_schema"`
		} `json:"tool"`
		Connections []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(document, &described); err != nil {
		t.Fatalf("tool document = %s: %v", document, err)
	}
	for _, forbidden := range []string{"base_url", "origin", "workspace", "depth", "filter", "order_by"} {
		if strings.Contains(string(described.Tool.InputSchema), forbidden) {
			t.Errorf("the input schema offers %q: %s", forbidden, described.Tool.InputSchema)
		}
	}
	// The projection stays the stable core; a workspace-specific field is not part of the contract.
	for _, forbidden := range []string{"annual_revenue", "people", "opportunities", "custom"} {
		if strings.Contains(string(described.Tool.OutputSchema), forbidden) {
			t.Errorf("the output schema promises %q: %s", forbidden, described.Tool.OutputSchema)
		}
	}
	if len(described.Connections) != 2 {
		t.Errorf("connections = %v, want both configured Twenty connections", described.Connections)
	}
}

// Everything the contract does not allow is refused before a connection is opened and before any secret is
// resolved, so no request can reach a workspace. Two workspaces are never picked silently.
func TestTwentyInvokeRefusalsHappenBeforeSecretsAndProviderIO(t *testing.T) {
	path := twentyConfig(t)

	tests := []struct {
		name  string
		input string
		args  []string
		code  string
	}{
		{
			name: "two workspaces without an explicit connection", input: `{}`,
			args: []string{"invoke", "twentycrm.companies.list", "--config", path},
			code: "connection-selection",
		},
		{
			name: "an unknown argument", input: `{"depth":1}`,
			args: []string{"invoke", "twentycrm.companies.list", "--connection", "crm", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a page size beyond the bound", input: `{"limit":200}`,
			args: []string{"invoke", "twentycrm.companies.list", "--connection", "crm", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a filter separator in the search term", input: `{"name_contains":"Acme\",or(name[ilike]:\"%"}`,
			args: []string{"invoke", "twentycrm.companies.list", "--connection", "crm", "--config", path},
			code: "invalid-request",
		},
		{
			name: "an identifier that is not a UUID", input: `{"id":"1"}`,
			args: []string{"invoke", "twentycrm.companies.get", "--connection", "crm", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a connection of another provider", input: `{"id":"8f14e45f-ceea-467a-9e60-3b4b5e0f0e21"}`,
			args: []string{"invoke", "twentycrm.companies.get", "--connection", "wiki", "--config", path},
			code: "unsupported-capability",
		},
		{
			name: "an unknown connection", input: `{}`,
			args: []string{"invoke", "twentycrm.companies.list", "--connection", "absent", "--config", path},
			code: "unknown-connection",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reads atomic.Int32
			code, stdout, stderr := runTwentyCLI(t, &reads, tt.input, tt.args...)
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

// The MCP broker and the tool CLI publish the same Twenty contracts and the same error codes, because both
// use the application core.
func TestTwentyMCPAndCLIShareTheCoreContracts(t *testing.T) {
	path := twentyConfig(t)
	options := &Options{Config: path, Redactor: &redact.Redactor{}}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"search","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.search","arguments":{"provider":"twentycrm"}}}`,
		`{"jsonrpc":"2.0","id":"describe","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.describe","arguments":{"operation":"twentycrm.companies.list","version":1}}}`,
		`{"jsonrpc":"2.0","id":"invoke","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.invoke","arguments":{"operation":"twentycrm.companies.get",` +
			`"connection":"crm","arguments":{"id":"not-a-uuid"}}}}`,
		`{"jsonrpc":"2.0","id":"ambiguous","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.invoke","arguments":{"operation":"twentycrm.companies.list","arguments":{}}}}`,
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
	if len(searched.Operations) != 2 || searched.Operations[0].ID != "twentycrm.companies.get" ||
		searched.Operations[1].ID != "twentycrm.companies.list" {
		t.Fatalf("search operations = %+v", searched.Operations)
	}

	describe := toolResultFrom(t, responses[`"describe"`])
	describedByCLI := runTwentyJSON(t, "", "tool", "twentycrm.companies.list", "--config", path, "--output", "json")
	assertMCPParity(t, describedByCLI, "tool", describe.Structured, "operation")
	assertMCPParity(t, describedByCLI, "connections", describe.Structured, "connections")

	invoke := toolResultFrom(t, responses[`"invoke"`])
	if !invoke.IsError {
		t.Fatalf("invoke = %+v, want the invalid identifier refused", invoke)
	}
	mcpCode, _, _ := strings.Cut(invoke.Content[0].Text, ":")
	code, _, cliStderr := runTwentyCLI(t, nil, `{"id":"not-a-uuid"}`,
		"invoke", "twentycrm.companies.get", "--connection", "crm", "--config", path)
	cliCode, _, _ := strings.Cut(strings.TrimPrefix(strings.SplitN(cliStderr, "\n", 2)[0], "callbell: "), ":")
	if code != exitUsage || mcpCode != cliCode || cliCode != "invalid-request" {
		t.Fatalf("MCP code=%q CLI code=%q exit=%d", mcpCode, cliCode, code)
	}

	// The broker does not pick one of the two workspaces either.
	ambiguous := toolResultFrom(t, responses[`"ambiguous"`])
	if !ambiguous.IsError || !strings.HasPrefix(ambiguous.Content[0].Text, "connection-selection:") {
		t.Fatalf("ambiguous invoke = %+v, want an explicit connection to be required", ambiguous)
	}
}

func runTwentyJSON(t *testing.T, input string, args ...string) []byte {
	t.Helper()
	if len(args) > 0 && args[len(args)-1] != "json" {
		args = append(args, "--output", "json")
	}
	code, stdout, stderr := runTwentyCLI(t, nil, input, args...)
	if code != exitOK || stderr != "" {
		t.Fatalf("CLI %v exit=%d stderr=%q", args, code, stderr)
	}
	return []byte(stdout)
}
