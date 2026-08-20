package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

func TestOversizedInvokeStopsBeforeHandler(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, validConfig)

	calls := 0
	descriptor := capability.Descriptor{
		ID: "bookstack.pages.get", Version: 1, Description: "Read one page", Provider: "bookstack",
		Risk: capability.Risk{
			Effect: capability.EffectRead, Idempotency: capability.IdempotencySafe,
			Confirmation: capability.ConfirmationNone, DataSensitivity: "test",
		},
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}
	handler := capability.Handler(func(context.Context, *config.Resolved, *secret.Resolver,
		*redact.Redactor, json.RawMessage) (any, error) {
		calls++
		return map[string]any{}, nil
	})
	registry := capability.NewRegistry()
	registerBookstackTestMetadata(t, registry)
	if err := registry.Register("bookstack", capability.Operation{Descriptor: descriptor, Handler: handler}); err != nil {
		t.Fatal(err)
	}

	base := `{"id":1}`
	input := base + strings.Repeat(" ", maxAgentRequestBytes-len(base)+1)
	var stdout, stderr bytes.Buffer
	opts := &Options{Input: strings.NewReader(input)}
	code := run(newRootCommand(opts, registry), opts,
		[]string{"invoke", "bookstack.pages.get", "--config", cfg}, &stdout, &stderr)

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "callbell: invalid-request:") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if calls != 0 {
		t.Errorf("handler calls = %d, want 0", calls)
	}
}

func TestInvokeWithoutMatchingConnectionIsASelectionError(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, "version: 1\n")

	calls := 0
	descriptor := capability.Descriptor{
		ID: "bookstack.pages.get", Version: 1, Description: "Read one page", Provider: "bookstack",
		Risk: capability.Risk{
			Effect: capability.EffectRead, Idempotency: capability.IdempotencySafe,
			Confirmation: capability.ConfirmationNone, DataSensitivity: "test",
		},
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}
	handler := capability.Handler(func(context.Context, *config.Resolved, *secret.Resolver,
		*redact.Redactor, json.RawMessage) (any, error) {
		calls++
		return map[string]any{}, nil
	})
	registry := capability.NewRegistry()
	registerBookstackTestMetadata(t, registry)
	if err := registry.Register("bookstack", capability.Operation{Descriptor: descriptor, Handler: handler}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	opts := &Options{Input: strings.NewReader("")}
	code := run(newRootCommand(opts, registry), opts,
		[]string{"invoke", "bookstack.pages.get", "--config", cfg}, &stdout, &stderr)

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	first, _, _ := strings.Cut(stderr.String(), "\n")
	if !strings.HasPrefix(first, "callbell: connection-selection: no configured connection can invoke operation") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if strings.Contains(first, "--connection") {
		t.Errorf("diagnostic contains an obsolete flag hint: %q", first)
	}
	if calls != 0 {
		t.Errorf("handler calls = %d, want 0", calls)
	}
}

func TestConfirmedMutationWritesMinimalAuditToStderr(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, `version: 1
services:
  telegram:
    provider: fake
    base_url: https://example.invalid
credentials:
  bot:
    type: keyring
connections:
  alerts:
    service: telegram
    credential: bot
    target: "-1001"
defaults: {}
`)
	registry := capability.NewRegistry()
	if err := registry.RegisterProvider(config.ProviderMetadata{ID: "fake", Name: "Fake"}, nil); err != nil {
		t.Fatal(err)
	}
	descriptor := capability.Descriptor{
		ID: "fake.messages.send", Version: 1, Description: "Send one message", Provider: "fake",
		RequiresExplicitConnection: true,
		Risk: capability.Risk{
			Effect: capability.EffectCreate, Idempotency: capability.IdempotencyNonIdempotent,
			Confirmation: capability.ConfirmationRequired, OpenWorld: true, DataSensitivity: "message",
		},
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"message_id":{"type":"integer"},"date":{"type":"integer"}},"required":["message_id","date"],"additionalProperties":false}`),
	}
	if err := registry.Register("fake", capability.Operation{
		Descriptor: descriptor,
		Handler: func(context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor,
			json.RawMessage) (any, error) {
			return map[string]any{"message_id": int64(7), "date": int64(1787220000)}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	opts := &Options{Input: strings.NewReader(`{"text":"private message"}`), Redactor: &redact.Redactor{}}
	code := run(newRootCommand(opts, registry), opts,
		[]string{"invoke", "fake.messages.send", "--connection", "alerts", "--confirm", "--config", cfg},
		&stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"message_id":7`) || !strings.Contains(stdout.String(), `"date":1787220000`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &event); err != nil {
		t.Fatalf("stderr is not one audit JSON event: %q: %v", stderr.String(), err)
	}
	for _, key := range []string{"request_id", "operation", "connection", "confirmed", "result", "time"} {
		if _, ok := event[key]; !ok {
			t.Errorf("audit is missing %q: %#v", key, event)
		}
	}
	for _, forbidden := range []string{"private message", "-1001", "message_id", "arguments", "token", "header"} {
		if strings.Contains(stderr.String(), forbidden) {
			t.Errorf("audit contains forbidden value %q: %s", forbidden, stderr.String())
		}
	}
}

func TestConfirmedProviderFailureKeepsCodeFirstAndAuditLast(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, `version: 1
services:
  telegram:
    provider: fake
    base_url: https://example.invalid
credentials:
  bot:
    type: keyring
connections:
  alerts:
    service: telegram
    credential: bot
    target: "-1001"
defaults: {}
`)
	registry := capability.NewRegistry()
	if err := registry.RegisterProvider(config.ProviderMetadata{ID: "fake", Name: "Fake"}, nil); err != nil {
		t.Fatal(err)
	}
	descriptor := capability.Descriptor{
		ID: "fake.messages.send", Version: 1, Description: "Send one message", Provider: "fake",
		RequiresExplicitConnection: true,
		Risk: capability.Risk{
			Effect: capability.EffectCreate, Idempotency: capability.IdempotencyNonIdempotent,
			Confirmation: capability.ConfirmationRequired, OpenWorld: true, DataSensitivity: "message",
		},
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}
	const canary = "provider-body-canary-8c21"
	if err := registry.Register("fake", capability.Operation{
		Descriptor: descriptor,
		Handler: func(context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor,
			json.RawMessage) (any, error) {
			return nil, &provider.Error{
				Class: provider.ClassPermission, Op: "send message", Message: "private " + canary,
			}
		},
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	redactor := &redact.Redactor{}
	redactor.Add(canary)
	opts := &Options{Input: strings.NewReader(`{"text":"private message"}`), Redactor: redactor}
	code := run(newRootCommand(opts, registry), opts,
		[]string{"invoke", "fake.messages.send", "--connection", "alerts", "--confirm", "--config", cfg},
		&stdout, &stderr)
	if code != exitRuntime || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "callbell: permission: send message: private [redacted]") {
		t.Fatalf("stderr lines = %#v", lines)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &event); err != nil {
		t.Fatalf("audit line = %q: %v", lines[1], err)
	}
	if event["result"] != string(provider.ClassPermission) || event["operation"] != descriptor.ID ||
		event["connection"] != "alerts" || event["confirmed"] != true {
		t.Fatalf("audit event = %#v", event)
	}
	for _, forbidden := range []string{"private message", "-1001", canary, "arguments", "header", "provider body"} {
		if strings.Contains(lines[1], forbidden) {
			t.Errorf("audit contains forbidden value %q: %s", forbidden, lines[1])
		}
	}
}

func TestExplicitConnectionDiagnosticStopsBeforeSecretsAndAudit(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, `version: 1
services:
  telegram:
    provider: fake
    base_url: https://example.invalid
credentials:
  bot:
    type: env
    values:
      token: FAKE_BOT_TOKEN
connections:
  alerts:
    service: telegram
    credential: bot
    target: "-1001"
defaults:
  connections:
    fake: alerts
`)
	registry := capability.NewRegistry()
	if err := registry.RegisterProvider(config.ProviderMetadata{
		ID: "fake", Name: "Fake", SecretRoles: []config.SecretRole{{Name: "token"}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	descriptor := capability.Descriptor{
		ID: "fake.messages.send", Version: 1, Description: "Send one message", Provider: "fake",
		RequiresExplicitConnection: true,
		Risk: capability.Risk{
			Effect: capability.EffectCreate, Idempotency: capability.IdempotencyNonIdempotent,
			Confirmation: capability.ConfirmationRequired, OpenWorld: true, DataSensitivity: "message",
		},
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}
	handlerCalls := 0
	if err := registry.Register("fake", capability.Operation{
		Descriptor: descriptor,
		Handler: func(context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor,
			json.RawMessage) (any, error) {
			handlerCalls++
			return map[string]any{}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	secretReads := 0
	resolver := secret.NewWith(func(string) string { secretReads++; return "canary-token" }, nil, nil, nil)
	var stdout, stderr bytes.Buffer
	opts := &Options{
		Input: strings.NewReader(`{"text":"private message"}`), Redactor: &redact.Redactor{},
		Secrets: resolver,
	}
	code := run(newRootCommand(opts, registry), opts,
		[]string{"invoke", "fake.messages.send", "--confirm", "--config", cfg}, &stdout, &stderr)
	if code != exitUsage || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	first, _, _ := strings.Cut(stderr.String(), "\n")
	want := `callbell: connection-selection: operation "fake.messages.send" requires an explicit connection in this invoke request`
	if first != want {
		t.Fatalf("first stderr line = %q, want %q", first, want)
	}
	if handlerCalls != 0 || secretReads != 0 || strings.Contains(stderr.String(), `"request_id"`) {
		t.Fatalf("handler=%d secrets=%d stderr=%q", handlerCalls, secretReads, stderr.String())
	}
}
