package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

func TestCodeFor(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want output.Code
	}{
		{"missing configuration", &config.NotFoundError{Path: "/absent.yaml"}, output.CodeConfigMissing},
		{"invalid configuration", &config.InvalidError{Path: "/c.yaml", Err: errors.New("version")}, output.CodeConfigInvalid},
		{"no connection selected", &config.SelectionError{Domain: "knowledge"}, output.CodeConnectionSelection},
		{"named connection missing", &config.SelectionError{Domain: "knowledge", Name: "absent"}, output.CodeUnknownConnection},
		{"unknown connection", &capability.UnknownConnectionError{Name: "absent"}, output.CodeUnknownConnection},
		{"invalid agent request", &application.InvalidRequestError{Message: "schema"}, output.CodeInvalidRequest},
		{"unknown operation", &application.UnknownOperationError{Operation: "x"}, output.CodeUnknownOperation},
		{"ambiguous connection", &application.ConnectionAmbiguousError{Operation: "x"}, output.CodeConnectionAmbiguous},
		{"no matching connection", &application.ConnectionSelectionError{Operation: "x"}, output.CodeConnectionSelection},
		{"confirmation required", &application.ConfirmationRequiredError{Operation: "x"}, output.CodeConfirmationRequired},
		{"policy denied", &application.PolicyDeniedError{Operation: "x"}, output.CodePolicyDenied},
		{"invalid provider result", &application.InvalidProviderResponseError{Operation: "x"}, output.CodeInvalidProviderResult},
		{"provider permission", &provider.Error{Class: provider.ClassPermission}, output.CodePermission},
		{"provider timeout", &provider.Error{Class: provider.ClassTimeout}, output.CodeTimeout},
		{"provider invalid response", &provider.Error{Class: provider.ClassInvalidResponse}, output.CodeInvalidProviderResult},
		{"unsupported capability", &capability.UnsupportedError{Capability: "x"}, output.CodeUnsupportedCapability},
		{"projection", &output.ProjectionError{Field: "x"}, output.CodeUsage},
		{"plain usage error", &UsageError{errors.New("unknown flag")}, output.CodeUsage},
		{"anything else", errors.New("io failure"), output.CodeRuntime},
		{"wrapped specific error", fmt.Errorf("load: %w", &config.NotFoundError{}), output.CodeConfigMissing},
		// A specific error wrapped in a usage error keeps its own code.
		{"usage wrapping a specific error", &UsageError{&capability.UnknownConnectionError{}}, output.CodeUnknownConnection},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codeFor(tt.err); got != tt.want {
				t.Errorf("codeFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Every diagnostic passes the redactor, including an unexpected error nobody anticipated.
func TestErrorsAreRedacted(t *testing.T) {
	const canary = "s3cr3t-canary-9f3a1c"

	var stdout, stderr bytes.Buffer
	opts := &Options{Redactor: &redact.Redactor{}}
	opts.Redactor.Add(canary)

	cmd := newRootCommand(opts, defaultRegistry())
	cmd.RunE = func(*cobra.Command, []string) error {
		return fmt.Errorf("provider rejected Authorization: Token id:%s", canary)
	}

	code := run(cmd, opts, nil, &stdout, &stderr)

	if code != exitRuntime {
		t.Errorf("exit code = %d, want %d", code, exitRuntime)
	}
	if strings.Contains(stderr.String(), canary) {
		t.Errorf("stderr leaks the secret: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), redact.Marker) {
		t.Errorf("stderr = %q, want the redaction marker", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

// An agent-mode failure carries a machine-readable code and no prose.
func TestAgentErrorIsMachineReadable(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")

	var stdout, stderr bytes.Buffer
	opts := &Options{}
	code := run(newRootCommand(opts, defaultRegistry()), opts,
		[]string{"tools", "--agent", "--config", "/nonexistent/callbell.yaml"}, &stdout, &stderr)

	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	first, _, _ := strings.Cut(stderr.String(), "\n")
	if !strings.HasPrefix(first, "callbell: "+string(output.CodeConfigMissing)+": ") {
		t.Errorf("first stderr line = %q, want the code prefix", first)
	}
}
