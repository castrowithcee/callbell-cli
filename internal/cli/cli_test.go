package cli

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
				"--config", "--connection", "--agent", "--output", "--fields", "--limit", "--version",
			},
		},
		{
			name:       "version output is deterministic",
			args:       []string{"--version"},
			wantCode:   exitOK,
			wantStdout: "callbell dev\n",
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

// Options receive the parsed global flags so the application core can consume them as a value.
func TestOptionsAreParsed(t *testing.T) {
	opts := &Options{}
	cmd := newRootCommand(opts, defaultRegistry())
	cmd.SetArgs([]string{"--config", "/tmp/c.yaml", "--connection", "wiki", "--agent", "--output", "compact", "--fields", "id,name", "--limit", "5"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	if opts.Config != "/tmp/c.yaml" || opts.Connection != "wiki" || !opts.Agent || opts.Output != "compact" {
		t.Errorf("options = %+v", *opts)
	}
	if !reflect.DeepEqual(opts.Fields, []string{"id", "name"}) {
		t.Errorf("fields = %v, want [id name]", opts.Fields)
	}
	if opts.Limit != 5 {
		t.Errorf("limit = %d, want 5", opts.Limit)
	}
}
