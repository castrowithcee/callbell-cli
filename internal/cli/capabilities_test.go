package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// fakeRegistry registers two capabilities for the provider the test configuration uses. No provider
// implementation is involved: discovery answers from the registry alone.
func registerBookstackTestMetadata(t *testing.T, reg *capability.Registry) {
	t.Helper()
	if err := reg.RegisterProvider(config.ProviderMetadata{
		ID: "bookstack", Name: "BookStack",
		SecretRoles: []config.SecretRole{{Name: "token-id"}, {Name: "token-secret"}},
		Target:      config.TargetMetadata{Label: "target"},
	}, nil); err != nil {
		t.Fatalf("RegisterProvider() = %v", err)
	}
}

func fakeRegistry(t *testing.T) *capability.Registry {
	t.Helper()
	reg := capability.NewRegistry()
	registerBookstackTestMetadata(t, reg)
	err := reg.Register("bookstack",
		capability.Operation{
			Descriptor: capability.Descriptor{
				ID:          "bookstack.pages.list",
				Version:     1,
				Description: "List pages",
				Risk: capability.Risk{
					Effect:          capability.EffectRead,
					Idempotency:     capability.IdempotencySafe,
					Confirmation:    capability.ConfirmationNone,
					DataSensitivity: "test-data",
				},
				Provider:     "bookstack",
				InputSchema:  json.RawMessage(`{"type":"object"}`),
				OutputSchema: json.RawMessage(`{"type":"array"}`),
				Arguments:    []capability.Argument{{Name: "limit", Description: "Maximum number of pages"}},
				Fields:       []capability.Field{{Name: "id"}, {Name: "name"}},
			},
			Handler: capability.Handler(func(context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor, json.RawMessage) (any, error) {
				return []map[string]any{{"id": 1, "name": "Page"}}, nil
			}),
		},
		capability.Operation{
			Descriptor: capability.Descriptor{
				ID:          "bookstack.pages.get",
				Version:     1,
				Description: "Read one page",
				Risk: capability.Risk{
					Effect:          capability.EffectRead,
					Idempotency:     capability.IdempotencySafe,
					Confirmation:    capability.ConfirmationNone,
					DataSensitivity: "test-data",
				},
				Provider:     "bookstack",
				InputSchema:  json.RawMessage(`{"type":"object"}`),
				OutputSchema: json.RawMessage(`{"type":"object"}`),
				Arguments:    []capability.Argument{{Name: "id", Description: "Page identifier", Required: true}},
				Fields:       []capability.Field{{Name: "html"}},
			},
			Handler: capability.Handler(func(context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor, json.RawMessage) (any, error) {
				return map[string]any{"html": "<p>Page</p>"}, nil
			}),
		},
	)
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	return reg
}

// runDiscovery drives the real command tree with a fake provider registry.
func runDiscovery(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	opts := &Options{}
	code := run(newRootCommand(opts, fakeRegistry(t)), opts, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func runAgentDiscovery(t *testing.T, request string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	redactor := &redact.Redactor{}
	opts := &Options{
		Input: strings.NewReader(request), Redactor: redactor,
		Secrets: secret.NewWith(nil, nil, nil, redactor),
	}
	code := run(newRootCommand(opts, fakeRegistry(t)), opts, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCapabilitiesCommand(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, validConfig)

	t.Run("union of all configured connections", func(t *testing.T) {
		code, stdout, stderr := runDiscovery(t, "capabilities", "--config", cfg, "--agent")

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
		}
		want := "name|risk|description\n" +
			"bookstack.pages.get|read|Read one page\n" +
			"bookstack.pages.list|read|List pages\n"
		if stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})

	t.Run("connection filter", func(t *testing.T) {
		code, stdout, _ := runDiscovery(t, "capabilities", "--connection", "wiki", "--config", cfg, "--agent")

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d", code, exitOK)
		}
		if !strings.Contains(stdout, "bookstack.pages.list|read|List pages") {
			t.Errorf("stdout = %q", stdout)
		}
	})

	t.Run("projection restricts and orders the columns", func(t *testing.T) {
		code, stdout, stderr := runDiscovery(t, "capabilities", "--config", cfg, "--agent", "--fields", "risk,name")

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
		}
		want := "risk|name\nread|bookstack.pages.get\nread|bookstack.pages.list\n"
		if stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("unknown projection field is a usage error", func(t *testing.T) {
		code, stdout, stderr := runDiscovery(t, "capabilities", "--config", cfg, "--fields", "absent")

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "callbell: usage: unknown field \"absent\"") {
			t.Errorf("stderr = %q", stderr)
		}
	})

	t.Run("an empty projection keeps every field", func(t *testing.T) {
		code, stdout, stderr := runDiscovery(t, "capabilities", "--config", cfg, "--agent", "--fields", "")

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
		}
		if !strings.HasPrefix(stdout, "name|risk|description\n") {
			t.Errorf("stdout = %q, want every column", stdout)
		}
	})

	t.Run("a repeated projection field is a usage error", func(t *testing.T) {
		code, _, stderr := runDiscovery(t, "capabilities", "--config", cfg, "--fields", "name,name")

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if !strings.Contains(stderr, `field "name" is requested more than once`) {
			t.Errorf("stderr = %q", stderr)
		}
	})

	t.Run("a negative limit removes the cap", func(t *testing.T) {
		_, stdout, _ := runDiscovery(t, "capabilities", "--config", cfg, "--agent", "--limit", "-1")

		if strings.Count(stdout, "\n") != 3 {
			t.Errorf("stdout = %q, want the header and both capabilities", stdout)
		}
	})

	t.Run("limit truncates the collection", func(t *testing.T) {
		_, stdout, _ := runDiscovery(t, "capabilities", "--config", cfg, "--agent", "--limit", "1")

		want := "name|risk|description\nbookstack.pages.get|read|Read one page\n"
		if stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("table format is the human default", func(t *testing.T) {
		_, stdout, _ := runDiscovery(t, "capabilities", "--config", cfg)

		if !strings.HasPrefix(stdout, "NAME") || !strings.Contains(stdout, "bookstack.pages.list") {
			t.Errorf("stdout = %q, want a table with a header", stdout)
		}
	})

	t.Run("explicit output wins over agent mode", func(t *testing.T) {
		_, stdout, _ := runDiscovery(t, "capabilities", "--config", cfg, "--agent", "--output", "json")

		if !strings.HasPrefix(stdout, `[{"name":"bookstack.pages.get","risk":"read"`) {
			t.Errorf("stdout = %q, want JSON", stdout)
		}
	})

	t.Run("unknown output format is a usage error", func(t *testing.T) {
		code, stdout, stderr := runDiscovery(t, "capabilities", "--config", cfg, "--output", "yaml")

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, `unknown output format "yaml"`) {
			t.Errorf("stderr = %q", stderr)
		}
	})

	t.Run("unknown connection is a usage error", func(t *testing.T) {
		code, stdout, stderr := runDiscovery(t, "capabilities", "--connection", "absent", "--config", cfg)

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, `unknown connection "absent"`) {
			t.Errorf("stderr = %q", stderr)
		}
	})

	t.Run("output is byte identical across runs", func(t *testing.T) {
		_, first, _ := runDiscovery(t, "capabilities", "--config", cfg)
		for i := 0; i < 5; i++ {
			if _, got, _ := runDiscovery(t, "capabilities", "--config", cfg); got != first {
				t.Fatalf("run %d = %q, want %q", i+2, got, first)
			}
		}
	})
}

func TestDescribeCommand(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, validConfig)

	t.Run("full JSON contract from stdin", func(t *testing.T) {
		code, stdout, stderr := runAgentDiscovery(t,
			`{"operation":"bookstack.pages.get","version":1}`, "describe", "--config", cfg)

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
		}
		var envelope struct {
			Data struct {
				Operation   capability.Descriptor `json:"operation"`
				Connections []string              `json:"connections"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("stdout is not JSON: %v", err)
		}
		if envelope.Data.Operation.ID != "bookstack.pages.get" || envelope.Data.Operation.Version != 1 {
			t.Errorf("operation = %+v", envelope.Data.Operation)
		}
		if len(envelope.Data.Operation.InputSchema) == 0 || len(envelope.Data.Operation.OutputSchema) == 0 {
			t.Errorf("schemas missing: %+v", envelope.Data.Operation)
		}
		if !reflect.DeepEqual(envelope.Data.Connections, []string{"wiki"}) {
			t.Errorf("connections = %v", envelope.Data.Connections)
		}
	})

	t.Run("unknown operation is stable", func(t *testing.T) {
		code, stdout, stderr := runAgentDiscovery(t,
			`{"operation":"absent.capability"}`, "describe", "--config", cfg)

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.HasPrefix(stderr, "callbell: unknown-operation:") {
			t.Errorf("stderr = %q", stderr)
		}
	})

	t.Run("unknown connection is a different error", func(t *testing.T) {
		_, _, stderr := runAgentDiscovery(t,
			`{"operation":"bookstack.pages.get","connection":"absent"}`, "describe", "--config", cfg)

		if !strings.Contains(stderr, `unknown connection "absent"`) {
			t.Errorf("stderr = %q, want the unknown-connection error", stderr)
		}
	})

	t.Run("empty stdin is a usage error", func(t *testing.T) {
		code, stdout, stderr := runAgentDiscovery(t, "", "describe", "--config", cfg)

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.HasPrefix(stderr, "callbell: invalid-request:") {
			t.Errorf("stderr = %q", stderr)
		}
	})
}

func TestSearchAndInvokeJSONCommands(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, validConfig)

	t.Run("search reads one request and writes one envelope", func(t *testing.T) {
		code, stdout, stderr := runAgentDiscovery(t,
			`{"query":"pages","effect":"read"}`, "search", "--config", cfg)
		if code != exitOK || stderr != "" {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		var envelope struct {
			Data struct {
				Operations []map[string]any `json:"operations"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || len(envelope.Data.Operations) != 2 {
			t.Fatalf("stdout = %q (%v)", stdout, err)
		}
	})

	t.Run("invoke dispatches a known schema directly", func(t *testing.T) {
		request := `{"operation":"bookstack.pages.get","arguments":{"id":"7"}}`
		code, stdout, stderr := runAgentDiscovery(t, request, "invoke", "--config", cfg)
		if code != exitOK || stderr != "" {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		var envelope struct {
			Data struct {
				Operation  string         `json:"operation"`
				Connection string         `json:"connection"`
				Result     map[string]any `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("stdout is not JSON: %v", err)
		}
		if envelope.Data.Operation != "bookstack.pages.get" || envelope.Data.Connection != "wiki" {
			t.Errorf("data = %+v", envelope.Data)
		}
	})

	t.Run("a second JSON request is rejected without stdout", func(t *testing.T) {
		code, stdout, stderr := runAgentDiscovery(t, "{}\n{}", "search", "--config", cfg)
		if code != exitUsage || stdout != "" || !strings.HasPrefix(stderr, "callbell: invalid-request:") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
}

// The shipped registry must be wirable without error, even while no provider is registered yet.
func TestDefaultRegistry(t *testing.T) {
	reg := defaultRegistry()
	if reg == nil {
		t.Fatal("defaultRegistry() = nil")
	}
	got := reg.Provider("bookstack")
	if len(got) != 2 {
		t.Fatalf("provider capabilities = %v, want the two BookStack capabilities", got)
	}
	if got[0].ID != "bookstack.pages.get" || got[1].ID != "bookstack.pages.list" {
		t.Errorf("capabilities = %v", got)
	}
	telegram, ok := reg.ProviderMetadata("telegram")
	if !ok || telegram.DefaultBaseURL != "https://api.telegram.org" || !telegram.Target.Required ||
		len(telegram.SecretRoles) != 1 || telegram.SecretRoles[0].Name != "bot-token" {
		t.Errorf("Telegram metadata = %+v, %v", telegram, ok)
	}
	if operations := reg.Provider("telegram"); len(operations) != 0 {
		t.Errorf("Telegram operations = %v, want none before the send task", operations)
	}
}
