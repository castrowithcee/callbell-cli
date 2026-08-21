package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

const mcpTestMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

func TestMCPCommandServesStdio(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + mcpTestMeta + `}}` + "\n"
	var stdout, stderr bytes.Buffer
	options := &Options{Input: strings.NewReader(input), Redactor: &redact.Redactor{}}
	code := run(newRootCommand(options, fakeRegistry(t)), options, []string{"mcp"}, &stdout, &stderr)
	if code != exitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	responses := decodeMCPResponses(t, stdout.String())
	var result struct {
		Tools []mcpTool `json:"tools"`
	}
	decodeRaw(t, responses["1"].Result, &result)
	if len(result.Tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(result.Tools))
	}
}

func TestMCPDiscoveryAndFixedTools(t *testing.T) {
	registry := fakeRegistry(t)
	for i := 0; i < 100; i++ {
		descriptor := capability.Descriptor{
			ID: fmt.Sprintf("bookstack.synthetic%d.get", i), Version: 1,
			Description: "Synthetic operation", Provider: "bookstack",
			Risk: capability.Risk{
				Effect: capability.EffectRead, Idempotency: capability.IdempotencySafe,
				Confirmation: capability.ConfirmationNone, DataSensitivity: "test",
			},
			InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		}
		if err := registry.Register("bookstack", capability.Operation{
			Descriptor: descriptor,
			Handler: func(context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor,
				json.RawMessage) (any, error) {
				return map[string]any{}, nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	input := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + mcpTestMeta + `}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + mcpTestMeta + `}}` + "\n"
	responses, stderr := runMCP(t, registry, input)
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	discover := responses["1"].Result
	if versions := stringFieldSlice(t, discover, "supportedVersions"); !reflect.DeepEqual(versions, []string{mcpProtocolVersion}) {
		t.Fatalf("supported versions = %v", versions)
	}
	capabilities := objectField(t, discover, "capabilities")
	if _, ok := capabilities["tools"]; !ok {
		t.Fatalf("capabilities = %s", capabilities)
	}

	var listed struct {
		ResultType string    `json:"resultType"`
		Tools      []mcpTool `json:"tools"`
		TTL        int       `json:"ttlMs"`
		CacheScope string    `json:"cacheScope"`
	}
	decodeRaw(t, responses["2"].Result, &listed)
	if listed.ResultType != "complete" || len(listed.Tools) != 3 ||
		listed.TTL != mcpCacheTTLMillis || listed.CacheScope != "public" {
		t.Fatalf("tools/list = %+v", listed)
	}
	want := []string{"callbell.search", "callbell.describe", "callbell.invoke"}
	for i, tool := range listed.Tools {
		if tool.Name != want[i] || len(tool.InputSchema) == 0 {
			t.Errorf("tool %d = %+v, want %q with schema", i, tool, want[i])
		}
	}
}

func TestMCPToolsUseApplicationCoreContracts(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	t.Setenv("CALLBELL_CREDENTIAL_STORE", "none")
	path := writeConfig(t, validConfig)
	registry := fakeRegistry(t)
	searchRequest := `{"query":"page"}`
	describeRequest := `{"operation":"bookstack.pages.get","version":1}`
	invokeRequest := `{"operation":"bookstack.pages.get","connection":"wiki","arguments":{}}`
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"search","method":"tools/call","params":{` + mcpTestMeta + `,"name":"callbell.search","arguments":` + searchRequest + `}}`,
		`{"jsonrpc":"2.0","id":"describe","method":"tools/call","params":{` + mcpTestMeta + `,"name":"callbell.describe","arguments":` + describeRequest + `}}`,
		`{"jsonrpc":"2.0","id":"invoke","method":"tools/call","params":{` + mcpTestMeta + `,"name":"callbell.invoke","arguments":` + invokeRequest + `}}`,
	}, "\n") + "\n"

	responses, stderr := runMCPWithOptions(t, registry, input, &Options{Config: path, Redactor: &redact.Redactor{}})
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}

	search := toolResultFrom(t, responses[`"search"`])
	if search.IsError {
		t.Fatalf("search returned an error: %s", search.Content[0].Text)
	}
	var searchResult struct {
		Operations []application.SearchHit `json:"operations"`
	}
	decodeRaw(t, search.Structured, &searchResult)
	if len(searchResult.Operations) != 2 {
		t.Fatalf("search operations = %d, want 2", len(searchResult.Operations))
	}
	// callbell.search keeps the request-bound agent contract; the CLI index deliberately publishes less.
	// What must agree is which tools they name, in which order.
	indexed := toolSummaries(t, string(runFakeCLIJSON(t, "", "tools", "--query", "page", "--config", path,
		"--output", "json")))
	if len(indexed) != len(searchResult.Operations) {
		t.Fatalf("index = %+v, want the %d searched operations", indexed, len(searchResult.Operations))
	}
	for i, tool := range indexed {
		if tool.ID != searchResult.Operations[i].ID ||
			tool.Connections != len(searchResult.Operations[i].Connections) {
			t.Errorf("index[%d] = %+v, want %+v", i, tool, searchResult.Operations[i])
		}
	}

	describe := toolResultFrom(t, responses[`"describe"`])
	var described application.DescribeResponse
	decodeRaw(t, describe.Structured, &described)
	wantConnections := []application.ConnectionRef{
		{Name: "wiki", Description: "read-only account on the team wiki"},
	}
	if describe.IsError || described.Operation.ID != "bookstack.pages.get" ||
		!reflect.DeepEqual(described.Connections, wantConnections) {
		t.Fatalf("describe = %+v, structured = %+v", describe, described)
	}
	describedByCLI := runFakeCLIJSON(t, "", "tool", "bookstack.pages.get", "--config", path, "--output", "json")
	assertMCPParity(t, describedByCLI, "tool", describe.Structured, "operation")
	assertMCPParity(t, describedByCLI, "connections", describe.Structured, "connections")

	invoke := toolResultFrom(t, responses[`"invoke"`])
	var invoked application.InvokeResponse
	decodeRaw(t, invoke.Structured, &invoked)
	var providerResult map[string]any
	if err := json.Unmarshal(invoked.Result, &providerResult); err != nil {
		t.Fatal(err)
	}
	if invoke.IsError || invoked.Operation != "bookstack.pages.get" || providerResult["html"] != "<p>Page</p>" {
		t.Fatalf("invoke = %+v, structured = %+v", invoke, invoked)
	}
	assertMCPParity(t, runFakeCLIJSON(t, "{}", "invoke", "bookstack.pages.get", "--connection", "wiki",
		"--config", path), "data", invoke.Structured, "")

	for _, result := range []mcpToolResult{search, describe, invoke} {
		var textValue any
		if err := json.Unmarshal([]byte(result.Content[0].Text), &textValue); err != nil {
			t.Fatalf("text content is not JSON: %q: %v", result.Content[0].Text, err)
		}
		var structuredValue any
		if err := json.Unmarshal(result.Structured, &structuredValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(textValue, structuredValue) {
			t.Errorf("text and structured content differ: %#v != %#v", textValue, structuredValue)
		}
	}
}

func TestMCPAndJSONCLIErrorCodeParity(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	path := writeConfig(t, validConfig)
	request := `{"operation":"absent.operation.get"}`
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` + mcpTestMeta +
		`,"name":"callbell.describe","arguments":` + request + `}}` + "\n"
	responses, _ := runMCPWithOptions(t, fakeRegistry(t), input, &Options{Config: path})
	mcpResult := toolResultFrom(t, responses["1"])
	if !mcpResult.IsError {
		t.Fatalf("MCP result = %+v, want tool error", mcpResult)
	}
	mcpCode, _, _ := strings.Cut(mcpResult.Content[0].Text, ":")

	code, stdout, stderr := runFakeCLI(t, "tool", "absent.operation.get", "--config", path)
	if code != exitUsage || stdout != "" {
		t.Fatalf("JSON CLI exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	firstLine, _, _ := strings.Cut(stderr, "\n")
	cliDiagnostic := strings.TrimPrefix(firstLine, "callbell: ")
	cliCode, _, _ := strings.Cut(cliDiagnostic, ":")
	if mcpCode != cliCode || cliCode != "unknown-operation" {
		t.Fatalf("MCP code=%q JSON CLI code=%q", mcpCode, cliCode)
	}
}

func TestMCPToolErrorsAreSafeAndProtocolErrorsStaySeparate(t *testing.T) {
	const canary = "secret-provider-detail-7294"
	t.Setenv("CALLBELL_CREDENTIAL_STORE", "none")
	registry, path := mcpTestRegistry(t, func(context.Context, *config.Resolved, *secret.Resolver,
		*redact.Redactor, json.RawMessage) (any, error) {
		return nil, errors.New("provider failed with " + canary)
	})
	redactor := &redact.Redactor{}
	redactor.Add(canary)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` + mcpTestMeta + `,"name":"callbell.invoke","arguments":{"operation":"fake.pages.get","connection":"primary","arguments":{}}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{` + mcpTestMeta + `,"name":"callbell.absent","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`,
	}, "\n") + "\n"
	responses, stderr := runMCPWithOptions(t, registry, input, &Options{Config: path, Redactor: redactor})

	execution := toolResultFrom(t, responses["1"])
	if !execution.IsError || !strings.HasPrefix(execution.Content[0].Text, "runtime:") ||
		!strings.Contains(execution.Content[0].Text, redact.Marker) {
		t.Fatalf("tool execution error = %+v", execution)
	}
	if responses["1"].Error != nil {
		t.Fatalf("execution failure became a protocol error: %+v", responses["1"].Error)
	}
	if responses["2"].Error == nil || responses["2"].Error.Code != mcpInvalidParams {
		t.Fatalf("unknown tool response = %+v", responses["2"])
	}
	if responses["3"].Error == nil || responses["3"].Error.Code != mcpUnsupportedVersion {
		t.Fatalf("unsupported version response = %+v", responses["3"])
	}
	if strings.Contains(encodeResponses(t, responses)+stderr, canary) {
		t.Fatal("MCP output leaked the registered secret canary")
	}
}

func TestMCPConfirmedMutationKeepsAuditOffProtocol(t *testing.T) {
	const message = "private-message-canary-1842"
	registry := capability.NewRegistry()
	if err := registry.RegisterProvider(config.ProviderMetadata{ID: "fake", Name: "Fake"}, nil); err != nil {
		t.Fatal(err)
	}
	descriptor := capability.Descriptor{
		ID: "fake.messages.send", Version: 1, Description: "Send a fake message", Provider: "fake",
		RequiresExplicitConnection: true,
		Risk: capability.Risk{
			Effect: capability.EffectCreate, Idempotency: capability.IdempotencyNonIdempotent,
			Confirmation: capability.ConfirmationRequired, OpenWorld: true, DataSensitivity: "message",
		},
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
	}
	if err := registry.Register("fake", capability.Operation{
		Descriptor: descriptor,
		Handler: func(context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor,
			json.RawMessage) (any, error) {
			return map[string]any{"ok": true}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, `version: 1
services:
  fake:
    provider: fake
    base_url: https://example.invalid
credentials:
  sender:
    type: keyring
connections:
  target:
    service: fake
    credential: sender
defaults: {}
`)
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` + mcpTestMeta +
		`,"name":"callbell.invoke","arguments":{"operation":"fake.messages.send","connection":"target","arguments":{"text":"` +
		message + `"},"confirm":true}}}` + "\n"
	responses, stderr := runMCPWithOptions(t, registry, input, &Options{
		Config: path, Redactor: &redact.Redactor{}, Secrets: secret.NewWith(nil, nil, nil, nil),
	})
	if result := toolResultFrom(t, responses["1"]); result.IsError {
		t.Fatalf("mutation result = %+v", result)
	}
	var audit map[string]any
	if err := json.Unmarshal([]byte(stderr), &audit); err != nil {
		t.Fatalf("stderr is not one audit event: %q: %v", stderr, err)
	}
	if len(audit) != 6 || audit["operation"] != descriptor.ID || audit["connection"] != "target" ||
		audit["confirmed"] != true || audit["result"] != "success" {
		t.Fatalf("audit = %#v", audit)
	}
	if strings.Contains(stderr, message) || strings.Contains(encodeResponses(t, responses), `"request_id"`) {
		t.Fatalf("message or audit crossed streams: stdout=%s stderr=%s", encodeResponses(t, responses), stderr)
	}
}

func TestMCPCancellationStopsCoreAndSuppressesResponse(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	registry, path := mcpTestRegistry(t, func(ctx context.Context, _ *config.Resolved, _ *secret.Resolver,
		_ *redact.Redactor, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	options := &Options{Config: path, Redactor: &redact.Redactor{}, Secrets: secret.NewWith(nil, nil, nil, nil)}
	var stdout, stderr bytes.Buffer
	server := newMCPServer(options, registry, &stdout, &stderr)
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.serve(context.Background(), reader) }()

	call := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{` + mcpTestMeta + `,"name":"callbell.invoke","arguments":{"operation":"fake.pages.get","connection":"primary","arguments":{}}}}` + "\n"
	if _, err := io.WriteString(writer, call); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	if _, err := io.WriteString(writer, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"test"}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach the application handler")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("cancelled request wrote stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestMCPDeadlineReturnsTimeoutToolError(t *testing.T) {
	registry, path := mcpTestRegistry(t, func(ctx context.Context, _ *config.Resolved, _ *secret.Resolver,
		_ *redact.Redactor, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	var stdout, stderr bytes.Buffer
	server := newMCPServer(&Options{
		Config: path, Redactor: &redact.Redactor{}, Secrets: secret.NewWith(nil, nil, nil, nil),
	}, registry, &stdout, &stderr)
	server.timeout = 20 * time.Millisecond
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` + mcpTestMeta + `,"name":"callbell.invoke","arguments":{"operation":"fake.pages.get","connection":"primary","arguments":{}}}}` + "\n"
	if err := server.serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	responses := decodeMCPResponses(t, stdout.String())
	result := toolResultFrom(t, responses["1"])
	if !result.IsError || result.Content[0].Text != "timeout: request deadline exceeded" {
		t.Fatalf("deadline result = %+v", result)
	}
}

func TestMCPMessageLimitDoesNotDesynchronizeFollowingMessages(t *testing.T) {
	oversized := strings.Repeat("x", maxMCPMessageBytes+1)
	valid := `{"jsonrpc":"2.0","id":2,"method":"server/discover","params":{` + mcpTestMeta + `}}`
	responses, _ := runMCP(t, fakeRegistry(t), oversized+"\n"+valid+"\n")
	if responses["null"].Error == nil || responses["null"].Error.Code != mcpParseError {
		t.Fatalf("oversized response = %+v", responses["null"])
	}
	if len(responses["2"].Result) == 0 {
		t.Fatal("valid message after oversized input was not served")
	}
}

func TestMCPInFlightLimitStopsBeforeCore(t *testing.T) {
	var stdout, stderr bytes.Buffer
	server := newMCPServer(&Options{}, fakeRegistry(t), &stdout, &stderr)
	for i := 0; i < maxMCPInFlight; i++ {
		server.pending[fmt.Sprintf("%d", i)] = &mcpPending{}
	}
	line := `{"jsonrpc":"2.0","id":"overflow","method":"tools/call","params":{` + mcpTestMeta +
		`,"name":"callbell.search","arguments":{}}}`
	server.handle(context.Background(), []byte(line))
	responses := decodeMCPResponses(t, stdout.String())
	response := responses[`"overflow"`]
	if response.Error == nil || response.Error.Code != mcpInternalError {
		t.Fatalf("overflow response = %+v", response)
	}
	if stderr.Len() != 0 || len(server.pending) != maxMCPInFlight {
		t.Fatalf("stderr=%q pending=%d", stderr.String(), len(server.pending))
	}
}

func TestMCPServeReturnsStdoutWriteError(t *testing.T) {
	want := errors.New("synthetic stdout failure")
	server := newMCPServer(&Options{}, fakeRegistry(t), mcpFailWriter{err: want}, io.Discard)
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + mcpTestMeta + `}}` + "\n"
	if err := server.serve(context.Background(), strings.NewReader(input)); !errors.Is(err, want) {
		t.Fatalf("serve() error = %v, want wrapped %v", err, want)
	}
}

type decodedMCPResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *mcpRPCError    `json:"error"`
}

type mcpToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Structured json.RawMessage `json:"structuredContent"`
	IsError    bool            `json:"isError"`
}

func runMCP(t *testing.T, registry *capability.Registry, input string) (map[string]decodedMCPResponse, string) {
	t.Helper()
	return runMCPWithOptions(t, registry, input, &Options{Redactor: &redact.Redactor{}})
}

func runMCPWithOptions(t *testing.T, registry *capability.Registry, input string,
	options *Options) (map[string]decodedMCPResponse, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	server := newMCPServer(options, registry, &stdout, &stderr)
	if err := server.serve(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatalf("serve() = %v", err)
	}
	return decodeMCPResponses(t, stdout.String()), stderr.String()
}

func decodeMCPResponses(t *testing.T, text string) map[string]decodedMCPResponse {
	t.Helper()
	responses := map[string]decodedMCPResponse{}
	decoder := json.NewDecoder(strings.NewReader(text))
	for {
		var response decodedMCPResponse
		if err := decoder.Decode(&response); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode MCP response from %q: %v", text, err)
		}
		responses[string(response.ID)] = response
	}
	return responses
}

func toolResultFrom(t *testing.T, response decodedMCPResponse) mcpToolResult {
	t.Helper()
	if response.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", response.Error)
	}
	var result mcpToolResult
	decodeRaw(t, response.Result, &result)
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("tool content = %+v", result.Content)
	}
	return result
}

func mcpTestRegistry(t *testing.T, handler capability.Handler) (*capability.Registry, string) {
	t.Helper()
	registry := capability.NewRegistry()
	if err := registry.RegisterProvider(config.ProviderMetadata{ID: "fake", Name: "Fake"}, nil); err != nil {
		t.Fatal(err)
	}
	descriptor := capability.Descriptor{
		ID: "fake.pages.get", Version: 1, Description: "Read a fake page", Provider: "fake",
		Risk: capability.Risk{
			Effect: capability.EffectRead, Idempotency: capability.IdempotencySafe,
			Confirmation: capability.ConfirmationNone, DataSensitivity: "test",
		},
		InputSchema:  json.RawMessage(`{"type":"object","additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}
	if err := registry.Register("fake", capability.Operation{Descriptor: descriptor, Handler: handler}); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, `version: 1
services:
  fake:
    provider: fake
    base_url: https://example.invalid
credentials:
  reader:
    type: keyring
connections:
  primary:
    service: fake
    credential: reader
defaults: {}
`)
	return registry, path
}

func decodeRaw(t *testing.T, raw json.RawMessage, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func objectField(t *testing.T, raw json.RawMessage, name string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	decodeRaw(t, raw, &object)
	var field map[string]json.RawMessage
	decodeRaw(t, object[name], &field)
	return field
}

func stringFieldSlice(t *testing.T, raw json.RawMessage, name string) []string {
	t.Helper()
	var object map[string]json.RawMessage
	decodeRaw(t, raw, &object)
	var field []string
	decodeRaw(t, object[name], &field)
	return field
}

func encodeResponses(t *testing.T, responses map[string]decodedMCPResponse) string {
	t.Helper()
	encoded, err := json.Marshal(responses)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// runFakeCLIJSON runs one public CLI command with the fake registry and returns its JSON document.
func runFakeCLIJSON(t *testing.T, input string, args ...string) []byte {
	t.Helper()
	code, stdout, stderr := runFakeCLIInput(t, input, args...)
	if code != exitOK || stderr != "" {
		t.Fatalf("CLI %v exit=%d stderr=%q", args, code, stderr)
	}
	return []byte(stdout)
}

// assertMCPParity proves that the public tool CLI and the fixed MCP broker tool publish the same data. The
// CLI document names it in the tool taxonomy, the broker in the core contract; the payload is identical.
func assertMCPParity(t *testing.T, cliDocument []byte, cliKey string, mcpDocument json.RawMessage, mcpKey string) {
	t.Helper()
	cliValue, mcpValue := jsonMember(t, cliDocument, cliKey), jsonMember(t, mcpDocument, mcpKey)
	if !jsonEqual(cliValue, mcpValue) {
		t.Fatalf("CLI %q = %s, MCP %q = %s", cliKey, cliValue, mcpKey, mcpValue)
	}
}

func jsonMember(t *testing.T, document json.RawMessage, key string) json.RawMessage {
	t.Helper()
	if key == "" {
		return document
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(document, &members); err != nil {
		t.Fatalf("document %s is not a JSON object: %v", document, err)
	}
	member, ok := members[key]
	if !ok {
		t.Fatalf("document %s has no member %q", document, key)
	}
	return member
}

type mcpFailWriter struct{ err error }

func (w mcpFailWriter) Write([]byte) (int, error) { return 0, w.err }
