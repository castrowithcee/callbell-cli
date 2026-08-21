package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantCode     int
		wantStdout   string   // exact match when set
		wantInStdout []string // substring match
		wantStderr   bool     // true when stderr must carry a diagnostic
	}{
		{
			name:         "no arguments prints help",
			args:         nil,
			wantCode:     exitOK,
			wantInStdout: []string{"Usage:", "callbell"},
		},
		{
			name:     "help lists all global flags",
			args:     []string{"--help"},
			wantCode: exitOK,
			wantInStdout: []string{
				"--config", "--connection", "--agent", "--output", "--version", "update",
			},
		},
		{
			name:       "version output is deterministic",
			args:       []string{"--version"},
			wantCode:   exitOK,
			wantStdout: "callbell dev\n",
		},
		{
			name:       "dev build cannot self-update",
			args:       []string{"update", "--check"},
			wantCode:   exitUsage,
			wantStdout: "",
			wantStderr: true,
		},
		{
			name:       "unknown flag is a usage error",
			args:       []string{"--nope"},
			wantCode:   exitUsage,
			wantStdout: "",
			wantStderr: true,
		},
		{
			name:       "unknown command is a usage error",
			args:       []string{"frobnicate"},
			wantCode:   exitUsage,
			wantStdout: "",
			wantStderr: true,
		},
		{
			name:         "global flags are accepted",
			args:         []string{"--agent", "--output", "json", "--connection", "wiki", "--config", "/nonexistent.yaml", "--help"},
			wantCode:     exitOK,
			wantInStdout: []string{"Usage:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := Run(tt.args, &stdout, &stderr)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" || tt.wantCode == exitUsage {
				if got := stdout.String(); got != tt.wantStdout {
					t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
				}
			}
			for _, want := range tt.wantInStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout does not contain %q:\n%s", want, stdout.String())
				}
			}
			if got := stderr.Len() > 0; got != tt.wantStderr {
				t.Errorf("stderr non-empty = %v, want %v (stderr: %s)", got, tt.wantStderr, stderr.String())
			}
		})
	}
}

// A global flag must not promise a projection or a page size the commands no longer apply. --fields now
// belongs to config validate, and --limit is gone; both must be rejected everywhere else.
func TestRemovedGlobalFlagsAreGone(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--help"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	for _, flag := range []string{"--fields", "--limit"} {
		if strings.Contains(stdout.String(), flag) {
			t.Errorf("the root help still offers %s:\n%s", flag, stdout.String())
		}
	}

	for _, args := range [][]string{
		{"tools", "--limit", "1"},
		{"tools", "--fields", "id"},
		{"tool", "bookstack.pages.list", "--limit", "1"},
		{"invoke", "bookstack.pages.list", "--limit", "1"},
	} {
		var stdout, stderr bytes.Buffer

		code := Run(args, &stdout, &stderr)

		if code != exitUsage {
			t.Errorf("%v: exit code = %d, want %d (stderr: %s)", args, code, exitUsage, stderr.String())
		}
		if stdout.String() != "" {
			t.Errorf("%v: stdout = %q, want empty", args, stdout.String())
		}
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"no error", nil, exitOK},
		{"internal runtime error", errors.New("provider request failed"), exitRuntime},
		{"wrapped runtime error", errors.Join(errors.New("read config"), errors.New("io failure")), exitRuntime},
		{"usage error", &UsageError{errors.New("unknown flag")}, exitUsage},
		{"wrapped usage error", errors.Join(errors.New("context"), &UsageError{errors.New("bad value")}), exitUsage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.err); got != tt.want {
				t.Errorf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// A runtime error surfacing from the command layer reaches stderr and exit code 1 without any output on
// stdout. There is no public test command, so the failing RunE is injected into a local root command and
// driven through the same run path as the binary.
func TestRunRuntimeError(t *testing.T) {
	var stdout, stderr bytes.Buffer

	opts := &Options{}
	cmd := newRootCommand(opts, defaultRegistry())
	cmd.RunE = func(*cobra.Command, []string) error { return errors.New("synthetic internal failure") }

	code := run(cmd, opts, nil, &stdout, &stderr)

	if code != exitRuntime {
		t.Errorf("exit code = %d, want %d", code, exitRuntime)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); got != "callbell: runtime: synthetic internal failure\n" {
		t.Errorf("stderr = %q, want the diagnostic line only", got)
	}
}

func TestRunWritesUsageBeforeCarriedAudit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	opts := &Options{Redactor: &redact.Redactor{}}
	cmd := newRootCommand(opts, defaultRegistry())
	cmd.RunE = func(*cobra.Command, []string) error {
		return withAudit(&UsageError{errors.New("confirmed usage failure")},
			[]byte(`{"request_id":"audit-id","result":"error"}`+"\n"))
	}

	code := run(cmd, opts, nil, &stdout, &stderr)
	if code != exitUsage || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	value := stderr.String()
	diagnostic := strings.Index(value, "callbell: usage: confirmed usage failure\n")
	usage := strings.Index(value, "Usage:")
	audit := strings.Index(value, `{"request_id":"audit-id","result":"error"}`)
	if diagnostic != 0 || usage < 0 || audit < 0 || !(diagnostic < usage && usage < audit) {
		t.Fatalf("stderr order is diagnostic, usage, audit: %q", value)
	}
}

// Options receive the parsed global flags so the application core can consume them as a value.
func TestOptionsAreParsed(t *testing.T) {
	opts := &Options{}
	cmd := newRootCommand(opts, defaultRegistry())
	cmd.SetArgs([]string{"--config", "/tmp/c.yaml", "--connection", "wiki", "--agent", "--output", "compact"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	if opts.Config != "/tmp/c.yaml" || opts.Connection != "wiki" || !opts.Agent || opts.Output != "compact" {
		t.Errorf("options = %+v", *opts)
	}
}

func TestEmitRedactsBeforeEncoding(t *testing.T) {
	const canary = `canary-"\|=value`

	tests := []struct {
		format output.Format
		want   string
	}{
		{output.FormatTable, "ID  NAME\n7   before [redacted] after\n"},
		{output.FormatJSON, "[{\"id\":7,\"name\":\"before [redacted] after\"}]\n"},
		{output.FormatCompact, "id|name\n7|before [redacted] after\n"},
	}
	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			var stdout bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&stdout)
			redactor := &redact.Redactor{}
			redactor.Add(canary)
			opts := &Options{Format: tt.format, Redactor: redactor}
			result := output.Collection{
				Columns: []string{"id", "name"},
				Rows: []output.Row{
					{"name": "before " + canary + " after", "id": int64(7)},
				},
			}

			if err := emit(cmd, opts, result); err != nil {
				t.Fatalf("emit() = %v", err)
			}
			if got := stdout.String(); got != tt.want {
				t.Errorf("stdout = %q, want %q", got, tt.want)
			}
			if strings.Contains(stdout.String(), canary) {
				t.Errorf("stdout leaks the secret: %q", stdout.String())
			}
			if tt.format == output.FormatJSON {
				var rows []map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
					t.Fatalf("stdout is not valid JSON: %v", err)
				}
				if _, ok := rows[0]["id"].(float64); !ok {
					t.Errorf("id = %T, want a JSON number", rows[0]["id"])
				}
			}
		})
	}
}

// Nothing shortens a result on its way out, and the caller's own value stays untouched.
func TestEmitKeepsEveryRowAndTheInput(t *testing.T) {
	const canary = "complete-canary-8a14"

	var stdout bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	redactor := &redact.Redactor{}
	redactor.Add(canary)
	result := output.Collection{
		Columns: []string{"name", "count"},
		Rows: []output.Row{
			{"name": canary, "count": int64(1)},
			{"name": canary, "count": int64(2)},
		},
	}

	if err := emit(cmd, &Options{Format: output.FormatCompact, Redactor: redactor}, result); err != nil {
		t.Fatalf("emit() = %v", err)
	}
	if got, want := stdout.String(), "name|count\n[redacted]|1\n[redacted]|2\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got := result.Rows[0]["name"]; got != canary {
		t.Errorf("input value = %q, want it unchanged", got)
	}
}
