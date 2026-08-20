package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/config"
)

const validConfig = `
version: 1
services:
  wiki:
    provider: bookstack
    base_url: https://wiki.example.invalid
credentials:
  reader:
    type: env
    values:
      token-id: WIKI_TOKEN_ID
      token-secret: WIKI_TOKEN_SECRET
connections:
  wiki:
    service: wiki
    credential: reader
defaults:
  connections:
    knowledge: wiki
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestConfigValidate(t *testing.T) {
	// The command must not fall back to the developer's own configuration.
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")

	valid := writeConfig(t, validConfig)
	broken := writeConfig(t, "version: 1\nservices:\n  wiki:\n    provider: confluence\n    base_url: x\n")
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	tests := []struct {
		name       string
		path       string
		wantCode   int
		wantStderr bool
	}{
		{"valid configuration is silent", valid, exitOK, false},
		{"invalid configuration is a usage error", broken, exitUsage, true},
		{"missing configuration is a usage error", missing, exitUsage, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := Run([]string{"config", "validate", "--config", tt.path}, &stdout, &stderr)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if got := stderr.Len() > 0; got != tt.wantStderr {
				t.Errorf("stderr non-empty = %v, want %v (stderr: %s)", got, tt.wantStderr, stderr.String())
			}
		})
	}
}

func TestConfigValidateUsesTelegramProviderMetadata(t *testing.T) {
	const canary = "canary-telegram-secret-must-not-appear-441"
	t.Setenv("TELEGRAM_BOT_TOKEN", canary)
	valid := `version: 1
services:
  telegram-main:
    provider: telegram
    base_url: https://api.telegram.org
credentials:
  notifier:
    type: env
    values:
      bot-token: TELEGRAM_BOT_TOKEN
connections:
  alerts:
    service: telegram-main
    credential: notifier
    target: "-1001111111111"
  operations:
    service: telegram-main
    credential: notifier
    target: "-1002222222222"
defaults:
  connections:
    telegram: alerts
`
	for _, tt := range []struct {
		name string
		body string
		want int
	}{
		{name: "two distinct targets", body: valid, want: exitOK},
		{name: "target is required", body: strings.Replace(valid, `    target: "-1001111111111"`, `    target: ""`, 1), want: exitUsage},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run([]string{"config", "validate", "--config", writeConfig(t, tt.body)}, &stdout, &stderr)
			if code != tt.want || stdout.Len() != 0 {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), canary) {
				t.Fatal("Telegram secret canary reached validation output")
			}
		})
	}
}

// Validation is deterministic: it reads only the file and reports problems in a stable order.
func TestConfigValidateIsDeterministic(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	path := writeConfig(t, "version: 9\nservices:\n  b:\n    provider: nope\n    base_url: x\n  a:\n    provider: nope\n    base_url: y\n")

	run := func() string {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"config", "validate", "--config", path}, &stdout, &stderr); code != exitUsage {
			t.Fatalf("exit code = %d, want %d", code, exitUsage)
		}
		return stderr.String()
	}

	first := run()
	for i := 0; i < 5; i++ {
		if got := run(); got != first {
			t.Fatalf("run %d differs:\n%s\nwant:\n%s", i+2, got, first)
		}
	}
	if !bytes.Contains([]byte(first), []byte("services.a")) || !bytes.Contains([]byte(first), []byte("services.b")) {
		t.Errorf("both services should be reported:\n%s", first)
	}
}

// Connection selection problems are usage errors. No domain command consumes a connection yet, so the
// classification is proven at its own boundary rather than through a command.
func TestClassifyUserError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"no error", nil, exitOK},
		{"missing file", &config.NotFoundError{Path: "/absent.yaml"}, exitUsage},
		{"invalid file", &config.InvalidError{Path: "/c.yaml", Err: errors.New("version")}, exitUsage},
		{"unknown connection", &config.SelectionError{Domain: "knowledge", Name: "absent"}, exitUsage},
		{"missing default", &config.SelectionError{Domain: "knowledge"}, exitUsage},
		{"wrapped selection error", fmt.Errorf("resolve: %w", &config.SelectionError{Domain: "knowledge"}), exitUsage},
		{"unreadable file", errors.New("cannot read /c.yaml: permission denied"), exitRuntime},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(classifyUserError(tt.err)); got != tt.want {
				t.Errorf("exit code = %d, want %d", got, tt.want)
			}
		})
	}
}

// The terminal editor refuses to start in agent mode, where there is nobody to type.
func TestTUIRefusesAgentMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	opts := &Options{}

	code := run(newRootCommand(opts, defaultRegistry()), opts, []string{"tui", "--agent"}, &stdout, &stderr)

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("cannot run in agent mode")) {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// An unexpected argument to a subcommand is a usage error, not a runtime error.
func TestConfigValidateRejectsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run([]string{"config", "validate", "surplus"}, &stdout, &stderr)

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}
