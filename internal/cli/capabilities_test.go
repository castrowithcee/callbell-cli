package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/capability"
)

// fakeRegistry registers two capabilities for the provider the test configuration uses. No provider
// implementation is involved: discovery answers from the registry alone.
func fakeRegistry(t *testing.T) *capability.Registry {
	t.Helper()
	reg := capability.NewRegistry()
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
			Handler: "list",
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
			Handler: "get",
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

	t.Run("full contract", func(t *testing.T) {
		code, stdout, stderr := runDiscovery(t, "describe", "bookstack.pages.get", "--config", cfg, "--agent")

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
		}
		want := "name=bookstack.pages.get\n" +
			"risk=read\n" +
			"description=Read one page\n" +
			"arguments=id!\n" +
			"fields=html\n"
		if stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("short contract", func(t *testing.T) {
		code, stdout, _ := runDiscovery(t, "describe", "--short", "bookstack.pages.get", "--config", cfg, "--agent")

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d", code, exitOK)
		}
		if want := "summary=read bookstack.pages.get(id)\n"; stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("capability nobody offers is a usage error", func(t *testing.T) {
		code, stdout, stderr := runDiscovery(t, "describe", "absent.capability", "--config", cfg)

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "no configured connection offers") {
			t.Errorf("stderr = %q", stderr)
		}
	})

	t.Run("unknown connection is a different error", func(t *testing.T) {
		_, _, stderr := runDiscovery(t, "describe", "bookstack.pages.get", "--connection", "absent", "--config", cfg)

		if !strings.Contains(stderr, `unknown connection "absent"`) {
			t.Errorf("stderr = %q, want the unknown-connection error", stderr)
		}
	})

	t.Run("missing argument is a usage error", func(t *testing.T) {
		code, stdout, _ := runDiscovery(t, "describe", "--config", cfg)

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
	})
}

// The shipped registry must be wirable without error, even while no provider is registered yet.
func TestDefaultRegistry(t *testing.T) {
	if reg := defaultRegistry(); reg == nil {
		t.Fatal("defaultRegistry() = nil")
	}
	got := defaultRegistry().Provider("bookstack")
	if len(got) != 2 {
		t.Fatalf("provider capabilities = %v, want the two BookStack capabilities", got)
	}
	if got[0].ID != "bookstack.pages.get" || got[1].ID != "bookstack.pages.list" {
		t.Errorf("capabilities = %v", got)
	}
}
