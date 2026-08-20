package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

func TestDecodeAgentRequestSizeLimit(t *testing.T) {
	prefix, suffix := `{"padding":"`, `"}`
	exact := prefix + strings.Repeat("a", maxAgentRequestBytes-len(prefix)-len(suffix)) + suffix
	if len(exact) != maxAgentRequestBytes {
		t.Fatalf("fixture size = %d, want %d", len(exact), maxAgentRequestBytes)
	}

	var request struct {
		Padding string `json:"padding"`
	}
	if err := decodeAgentRequest(strings.NewReader(exact), &request); err != nil {
		t.Fatalf("decodeAgentRequest(at limit) = %v", err)
	}
	if len(request.Padding) != maxAgentRequestBytes-len(prefix)-len(suffix) {
		t.Errorf("padding length = %d", len(request.Padding))
	}

	over := exact + " "
	err := decodeAgentRequest(strings.NewReader(over), &request)
	if err == nil || !strings.Contains(err.Error(), "exceeds 1048576 bytes") {
		t.Fatalf("decodeAgentRequest(limit + 1) = %v", err)
	}
}

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
	if err := registry.Register("bookstack", capability.Operation{Descriptor: descriptor, Handler: handler}); err != nil {
		t.Fatal(err)
	}

	base := `{"operation":"bookstack.pages.get","arguments":{}}`
	input := base + strings.Repeat(" ", maxAgentRequestBytes-len(base)+1)
	var stdout, stderr bytes.Buffer
	opts := &Options{Input: strings.NewReader(input)}
	code := run(newRootCommand(opts, registry), opts, []string{"invoke", "--config", cfg}, &stdout, &stderr)

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
	if err := registry.Register("bookstack", capability.Operation{Descriptor: descriptor, Handler: handler}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	opts := &Options{Input: strings.NewReader(`{"operation":"bookstack.pages.get","arguments":{}}`)}
	code := run(newRootCommand(opts, registry), opts, []string{"invoke", "--config", cfg}, &stdout, &stderr)

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
