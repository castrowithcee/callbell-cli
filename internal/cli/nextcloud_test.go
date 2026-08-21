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

// nextcloudConfig configures two Nextcloud instances, two identities of the first one, and one identity
// bound to two root folders, next to a BookStack connection. No instance is ever contacted here: every
// request in this file is refused by the core before any provider I/O, so the tests need no server.
func nextcloudConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	return writeConfig(t, `version: 1
services:
  cloud-main:
    provider: nextcloud
    base_url: https://cloud.example.invalid
  cloud-partner:
    provider: nextcloud
    base_url: https://partner.example.invalid/nextcloud
  wiki:
    provider: bookstack
    base_url: https://wiki.example.invalid
credentials:
  cloud-reader:
    type: env
    values:
      user-id: NEXTCLOUD_READER_USER
      app-password: NEXTCLOUD_READER_PASSWORD
  cloud-auditor:
    type: env
    values:
      user-id: NEXTCLOUD_AUDITOR_USER
      app-password: NEXTCLOUD_AUDITOR_PASSWORD
  partner-reader:
    type: env
    values:
      user-id: NEXTCLOUD_PARTNER_USER
      app-password: NEXTCLOUD_PARTNER_PASSWORD
  reader:
    type: env
    values:
      token-id: WIKI_TOKEN_ID
      token-secret: WIKI_TOKEN_SECRET
connections:
  files-reports:
    service: cloud-main
    credential: cloud-reader
    target: Reports
  files-archive:
    service: cloud-main
    credential: cloud-reader
    target: Archive/2026
  files-audit:
    service: cloud-main
    credential: cloud-auditor
    target: Audit
  files-partner:
    service: cloud-partner
    credential: partner-reader
    target: Shared/Callbell
  wiki:
    service: wiki
    credential: reader
defaults: {}
`)
}

// runNextcloudCLI drives the real command tree and the shipped registry with a resolver that counts
// secret lookups, so a run can prove it resolved no app password.
func runNextcloudCLI(t *testing.T, reads *atomic.Int32, input string, args ...string) (int, string, string) {
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

// The Nextcloud namespace publishes exactly the two read-only Files tools, with the connections that can
// run them and without contacting an instance.
func TestNextcloudToolsAreDiscoverable(t *testing.T) {
	path := nextcloudConfig(t)
	var reads atomic.Int32

	code, stdout, stderr := runNextcloudCLI(t, &reads, "", "tools", "nextcloud", "--config", path)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"id: nextcloud.files.list", "id: nextcloud.files.stat", "effect: read",
		"connections[4]: files-archive,files-audit,files-partner,files-reports",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tools output does not contain %q:\n%s", want, stdout)
		}
	}
	if got := toolIDs(t, string(runNextcloudJSON(t, "", "tools", "nextcloud", "--config", path))); len(got) != 2 {
		t.Errorf("nextcloud tools = %v, want exactly the two files tools", got)
	}
	if reads.Load() != 0 {
		t.Errorf("secret lookups = %d, want 0", reads.Load())
	}
}

// One tool document carries the complete contract. The instance, the identity, and the root folder are
// not part of it: an agent can name neither them nor a URL, a method, or a depth through the contract.
func TestNextcloudToolContractKeepsInstanceIdentityAndRootOutOfTheArguments(t *testing.T) {
	path := nextcloudConfig(t)

	code, stdout, stderr := runNextcloudCLI(t, nil, "", "tool", "nextcloud.files.list", "--config", path)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"id: nextcloud.files.list", "version: 1", "effect: read", "idempotency: safe",
		"confirmation: none", "requires_explicit_connection: true", "path",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("tool contract does not contain %q:\n%s", want, stdout)
		}
	}

	document := runNextcloudJSON(t, "", "tool", "nextcloud.files.list", "--config", path)
	var described struct {
		Tool struct {
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tool"`
		Connections []string `json:"connections"`
	}
	if err := json.Unmarshal(document, &described); err != nil {
		t.Fatalf("tool document = %s: %v", document, err)
	}
	for _, forbidden := range []string{
		"base_url", "instance", "user", "password", "root", "href", "depth", "method", "url", "recursive",
	} {
		if strings.Contains(string(described.Tool.InputSchema), forbidden) {
			t.Errorf("the input schema offers %q: %s", forbidden, described.Tool.InputSchema)
		}
	}
	if len(described.Connections) != 4 {
		t.Errorf("connections = %v, want all four configured Nextcloud connections", described.Connections)
	}
}

// Everything the contract does not allow is refused before a connection is opened and before any secret
// is resolved, so no request can reach an instance. Four connections are never picked silently.
func TestNextcloudInvokeRefusalsHappenBeforeSecretsAndProviderIO(t *testing.T) {
	path := nextcloudConfig(t)

	tests := []struct {
		name  string
		input string
		args  []string
		code  string
	}{
		{
			name: "four connections without an explicit one", input: `{}`,
			args: []string{"invoke", "nextcloud.files.list", "--config", path},
			code: "connection-selection",
		},
		{
			name: "an instance as an argument", input: `{"base_url":"https://evil.example.invalid"}`,
			args: []string{"invoke", "nextcloud.files.list", "--connection", "files-reports", "--config", path},
			code: "invalid-request",
		},
		{
			name: "an identity as an argument", input: `{"user":"other"}`,
			args: []string{"invoke", "nextcloud.files.list", "--connection", "files-reports", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a depth as an argument", input: `{"depth":"infinity"}`,
			args: []string{"invoke", "nextcloud.files.list", "--connection", "files-reports", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a path that traverses the root", input: `{"path":"../Audit"}`,
			args: []string{"invoke", "nextcloud.files.list", "--connection", "files-reports", "--config", path},
			code: "invalid-request",
		},
		{
			name: "an absolute path", input: `{"path":"/remote.php/dav/files/other/Audit"}`,
			args: []string{"invoke", "nextcloud.files.stat", "--connection", "files-reports", "--config", path},
			code: "invalid-request",
		},
		{
			name: "an encoded separator", input: `{"path":"2026%2F..%2FAudit"}`,
			args: []string{"invoke", "nextcloud.files.stat", "--connection", "files-reports", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a double encoded separator", input: `{"path":"2026%252F.."}`,
			args: []string{"invoke", "nextcloud.files.stat", "--connection", "files-reports", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a backslash separator", input: `{"path":"2026\\..\\Audit"}`,
			args: []string{"invoke", "nextcloud.files.stat", "--connection", "files-reports", "--config", path},
			code: "invalid-request",
		},
		{
			name: "a connection of another provider", input: `{}`,
			args: []string{"invoke", "nextcloud.files.list", "--connection", "wiki", "--config", path},
			code: "unsupported-capability",
		},
		{
			name: "an unknown connection", input: `{}`,
			args: []string{"invoke", "nextcloud.files.list", "--connection", "absent", "--config", path},
			code: "unknown-connection",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reads atomic.Int32
			code, stdout, stderr := runNextcloudCLI(t, &reads, tt.input, tt.args...)
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

// The MCP broker and the tool CLI publish the same Nextcloud contracts and the same error codes, because
// both use the application core.
func TestNextcloudMCPAndCLIShareTheCoreContracts(t *testing.T) {
	path := nextcloudConfig(t)
	options := &Options{Config: path, Redactor: &redact.Redactor{}}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"search","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.search","arguments":{"provider":"nextcloud"}}}`,
		`{"jsonrpc":"2.0","id":"describe","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.describe","arguments":{"operation":"nextcloud.files.list","version":1}}}`,
		`{"jsonrpc":"2.0","id":"invoke","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.invoke","arguments":{"operation":"nextcloud.files.stat",` +
			`"connection":"files-reports","arguments":{"path":"../Audit"}}}}`,
		`{"jsonrpc":"2.0","id":"ambiguous","method":"tools/call","params":{` + mcpTestMeta +
			`,"name":"callbell.invoke","arguments":{"operation":"nextcloud.files.list","arguments":{}}}}`,
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
	if len(searched.Operations) != 2 || searched.Operations[0].ID != "nextcloud.files.list" ||
		searched.Operations[1].ID != "nextcloud.files.stat" {
		t.Fatalf("search operations = %+v", searched.Operations)
	}

	describe := toolResultFrom(t, responses[`"describe"`])
	describedByCLI := runNextcloudJSON(t, "", "tool", "nextcloud.files.list", "--config", path, "--output", "json")
	assertMCPParity(t, describedByCLI, "tool", describe.Structured, "operation")
	assertMCPParity(t, describedByCLI, "connections", describe.Structured, "connections")

	invoke := toolResultFrom(t, responses[`"invoke"`])
	if !invoke.IsError {
		t.Fatalf("invoke = %+v, want the traversal refused", invoke)
	}
	mcpCode, _, _ := strings.Cut(invoke.Content[0].Text, ":")
	code, _, cliStderr := runNextcloudCLI(t, nil, `{"path":"../Audit"}`,
		"invoke", "nextcloud.files.stat", "--connection", "files-reports", "--config", path)
	cliCode, _, _ := strings.Cut(strings.TrimPrefix(strings.SplitN(cliStderr, "\n", 2)[0], "callbell: "), ":")
	if code != exitUsage || mcpCode != cliCode || cliCode != "invalid-request" {
		t.Fatalf("MCP code=%q CLI code=%q exit=%d", mcpCode, cliCode, code)
	}

	// The broker does not pick one of the four roots either.
	ambiguous := toolResultFrom(t, responses[`"ambiguous"`])
	if !ambiguous.IsError || !strings.HasPrefix(ambiguous.Content[0].Text, "connection-selection:") {
		t.Fatalf("ambiguous invoke = %+v, want an explicit connection to be required", ambiguous)
	}
}

func runNextcloudJSON(t *testing.T, input string, args ...string) []byte {
	t.Helper()
	if len(args) > 0 && args[len(args)-1] != "json" {
		args = append(args, "--output", "json")
	}
	code, stdout, stderr := runNextcloudCLI(t, nil, input, args...)
	if code != exitOK || stderr != "" {
		t.Fatalf("CLI %v exit=%d stderr=%q", args, code, stderr)
	}
	return []byte(stdout)
}
