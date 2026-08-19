package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// Canary values stand in for real tokens. No part of the acceptance run needs a real secret or a real
// BookStack instance.
const (
	canaryPrimaryID     = "canary-primary-id-3f7a91"
	canaryPrimarySecret = "canary-primary-secret-c4d208"
	canaryArchiveID     = "canary-archive-id-8b2e55"
	canaryArchiveSecret = "canary-archive-secret-1a9f6d"
)

// echoPageID is the page whose read makes the mock quote the credential back at the binary.
const echoPageID = "13"

func allCanaries() []string {
	return []string{canaryPrimaryID, canaryPrimarySecret, canaryArchiveID, canaryArchiveSecret}
}

// run executes the built binary and returns its exit code and both streams separately.
type runner struct {
	bin string
	env []string
	// seen collects every byte the binary ever produced, for the canary check at the end.
	seen *strings.Builder
}

func (c *runner) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Env = c.env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}
	c.seen.WriteString(stdout.String())
	c.seen.WriteString(stderr.String())
	return code, stdout.String(), stderr.String()
}

// page returns one BookStack page record.
func page(id int64, name string) map[string]any {
	return map[string]any{
		"id": id, "book_id": 7, "chapter_id": 0, "name": name,
		"slug":       strings.ToLower(strings.ReplaceAll(name, " ", "-")),
		"created_at": "2026-01-01T00:00:00.000000Z", "updated_at": "2026-01-02T00:00:00.000000Z",
	}
}

// mock answers the two read endpoints the way BookStack documents them. It rejects a request without the
// expected token, so a test can prove that connections stay separate.
func mock(t *testing.T, wantAuth string, pages []map[string]any, content string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"No authorization token found on the request"}}`))
			return false
		}
		return true
	}
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": pages, "total": len(pages)})
	})
	mux.HandleFunc("/api/pages/", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/pages/")
		if id == echoPageID {
			// A provider that hands the credential back in its own error message. Without it the canary
			// check would pass on a binary that redacts nothing, because nothing would ever carry a
			// secret towards the output in the first place.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":500,"message":"failed for ` +
				r.Header.Get("Authorization") + `"}}`))
			return
		}
		if id != "1" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Item not found"}}`))
			return
		}
		p := page(1, pages[0]["name"].(string))
		p["html"] = content
		p["markdown"] = "# Title\n\n- a\\b\n- c|d"
		_ = json.NewEncoder(w).Encode(p)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// buildBinary compiles the command under test into dir and returns its path.
func buildBinary(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "callbell")
	// The throwaway binary needs no VCS stamping, and stamping fails in a working copy without a
	// repository of its own.
	build := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building the binary: %v", err)
	}
	return bin
}

// TestEndToEnd drives the public command surface of the built binary against a local mock. It is the
// acceptance run for the MVP: discovery, connection selection, reading, every output format, the stream
// and exit code contract, and a secret leak check.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("the acceptance run builds the binary")
	}

	dir := t.TempDir()
	bin := buildBinary(t, dir)

	primary := mock(t, "Token "+canaryPrimaryID+":"+canaryPrimarySecret,
		[]map[string]any{page(1, "Primary Runbook"), page(2, "Primary Notes")},
		"<p>a|b</p>\n<p>c=d</p>")
	archive := mock(t, "Token "+canaryArchiveID+":"+canaryArchiveSecret,
		[]map[string]any{page(1, "Archived Runbook")}, "<p>archived</p>")

	configPath := filepath.Join(dir, "config.yaml")
	config := fmt.Sprintf(`version: 1
services:
  wiki-primary:
    provider: bookstack
    base_url: %s
  wiki-archive:
    provider: bookstack
    base_url: %s
credentials:
  primary-reader:
    type: env
    values:
      token-id: E2E_PRIMARY_ID
      token-secret: E2E_PRIMARY_SECRET
  archive-reader:
    type: env
    values:
      token-id: E2E_ARCHIVE_ID
      token-secret: E2E_ARCHIVE_SECRET
connections:
  primary:
    service: wiki-primary
    credential: primary-reader
  archive:
    service: wiki-archive
    credential: archive-reader
defaults:
  connections:
    knowledge: primary
`, primary.URL, archive.URL)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}

	var seen strings.Builder
	c := &runner{
		bin: bin,
		env: []string{
			"HOME=" + dir,
			"PATH=" + os.Getenv("PATH"),
			"CALLBELL_CONFIG=" + configPath,
			// The acceptance run must not touch the credential store of the machine it runs on, and
			// must not depend on whether that machine has one.
			secret.StoreSelector + "=none",
			"E2E_PRIMARY_ID=" + canaryPrimaryID,
			"E2E_PRIMARY_SECRET=" + canaryPrimarySecret,
			"E2E_ARCHIVE_ID=" + canaryArchiveID,
			"E2E_ARCHIVE_SECRET=" + canaryArchiveSecret,
		},
		seen: &seen,
	}

	t.Run("the configuration validates", func(t *testing.T) {
		code, stdout, stderr := c.run(t, "config", "validate")

		if code != 0 || stdout != "" || stderr != "" {
			t.Errorf("exit %d, stdout %q, stderr %q; want a silent success", code, stdout, stderr)
		}
	})

	t.Run("capabilities are discovered", func(t *testing.T) {
		code, stdout, stderr := c.run(t, "capabilities", "--agent", "--fields", "name")

		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		want := "name\nknowledge.pages.get\nknowledge.pages.list\n"
		if stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("a capability describes itself", func(t *testing.T) {
		code, stdout, _ := c.run(t, "describe", "knowledge.pages.list", "--agent")

		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		for _, want := range []string{"name=knowledge.pages.list", "risk=read", "fields=id,name,slug"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, want)
			}
		}
	})

	t.Run("the domain default selects a connection", func(t *testing.T) {
		code, stdout, stderr := c.run(t, "knowledge", "pages", "list", "--agent")

		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if !strings.Contains(stdout, "Primary Runbook") || strings.Contains(stdout, "Archived") {
			t.Errorf("stdout = %q, want the primary instance", stdout)
		}
	})

	t.Run("an explicit connection reaches the other instance", func(t *testing.T) {
		code, stdout, stderr := c.run(t, "knowledge", "pages", "list", "--connection", "archive", "--agent")

		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if !strings.Contains(stdout, "Archived Runbook") || strings.Contains(stdout, "Primary") {
			t.Errorf("stdout = %q, want the archive instance", stdout)
		}
	})

	t.Run("one page is read", func(t *testing.T) {
		code, stdout, _ := c.run(t, "knowledge", "pages", "get", "1", "--output", "json")

		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout is not JSON: %v", err)
		}
		if _, ok := got["id"].(float64); !ok {
			t.Errorf("id = %T, want a JSON number", got["id"])
		}
		if got["html"] != "<p>a|b</p>\n<p>c=d</p>" {
			t.Errorf("html = %q, want the content verbatim", got["html"])
		}
	})

	t.Run("a provider echoing the credential does not get it published", func(t *testing.T) {
		code, stdout, stderr := c.run(t, "knowledge", "pages", "get", echoPageID)

		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, redact.Marker) {
			t.Errorf("stderr = %q, want the credential replaced by %s", stderr, redact.Marker)
		}
	})

	t.Run("all three formats render the same result", func(t *testing.T) {
		_, table, _ := c.run(t, "knowledge", "pages", "list", "--output", "table")
		_, jsonOut, _ := c.run(t, "knowledge", "pages", "list", "--output", "json")
		_, compact, _ := c.run(t, "knowledge", "pages", "list", "--agent")

		if !strings.HasPrefix(table, "ID") || !strings.Contains(table, "Primary Runbook") {
			t.Errorf("table = %q, want a header and the data", table)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(jsonOut), &rows); err != nil || len(rows) != 2 {
			t.Errorf("json = %q (%v)", jsonOut, err)
		}
		if !strings.HasPrefix(compact, "id|name|slug|") {
			t.Errorf("compact = %q, want the header line", compact)
		}
		if len(compact) >= len(jsonOut) {
			t.Errorf("compact is %d bytes, json is %d bytes", len(compact), len(jsonOut))
		}
	})

	t.Run("repeated calls are byte identical", func(t *testing.T) {
		_, first, _ := c.run(t, "knowledge", "pages", "list", "--agent")
		for i := 0; i < 3; i++ {
			if _, got, _ := c.run(t, "knowledge", "pages", "list", "--agent"); got != first {
				t.Fatalf("run %d = %q, want %q", i+2, got, first)
			}
		}
	})

	t.Run("stream and exit code contract", func(t *testing.T) {
		tests := []struct {
			name  string
			args  []string
			code  int
			in    string
			usage string
		}{
			{"unknown flag", []string{"--nope"}, 2, "usage", "callbell [flags]"},
			{"unknown command", []string{"frobnicate"}, 2, "usage", "callbell [flags]"},
			{"unknown connection", []string{"knowledge", "pages", "list", "--connection", "absent"}, 2, "unknown-connection", "callbell knowledge pages list [flags]"},
			{"empty configuration file", []string{"capabilities", "--config", "/dev/null"}, 2, "config-invalid", "callbell capabilities [flags]"},
			{"unknown capability", []string{"describe", "absent.capability"}, 2, "unsupported-capability", "callbell describe <capability> [flags]"},
			{"unknown field", []string{"knowledge", "pages", "list", "--fields", "absent"}, 2, "usage", "callbell knowledge pages list [flags]"},
			{"missing page", []string{"knowledge", "pages", "get", "99"}, 1, "provider-error", ""},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				code, stdout, stderr := c.run(t, tt.args...)

				if code != tt.code {
					t.Errorf("exit %d, want %d (stderr %q)", code, tt.code, stderr)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want empty on failure", stdout)
				}
				if !strings.HasPrefix(stderr, "callbell: "+tt.in+": ") {
					t.Errorf("stderr = %q, want the %q code", stderr, tt.in)
				}
				if tt.usage != "" && !strings.Contains(stderr, "\nUsage:\n  "+tt.usage+"\n") {
					t.Errorf("stderr = %q, want usage for %q", stderr, tt.usage)
				}
			})
		}
	})

	t.Run("a wrong credential is an auth failure", func(t *testing.T) {
		wrong := *c
		wrong.env = append([]string{}, c.env...)
		for i, kv := range wrong.env {
			if strings.HasPrefix(kv, "E2E_ARCHIVE_SECRET=") {
				wrong.env[i] = "E2E_ARCHIVE_SECRET=" + canaryArchiveSecret + "-wrong"
			}
		}

		code, stdout, stderr := wrong.run(t, "knowledge", "pages", "list", "--connection", "archive")

		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.HasPrefix(stderr, "callbell: auth: ") {
			t.Errorf("stderr = %q, want the auth code", stderr)
		}
	})

	t.Run("an unset variable is reported by its configuration key", func(t *testing.T) {
		bare := *c
		bare.env = []string{
			"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "CALLBELL_CONFIG=" + configPath,
			secret.StoreSelector + "=none",
		}

		code, stdout, stderr := bare.run(t, "knowledge", "pages", "list")

		if code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "missing-secret") ||
			!strings.Contains(stderr, "credentials.primary-reader.values.token-id") {
			t.Errorf("stderr = %q, want the configuration key", stderr)
		}
		// The configured text is never repeated, because it may be a pasted token rather than a name.
		if strings.Contains(stderr, "E2E_PRIMARY_ID") {
			t.Errorf("stderr = %q, want the configured text kept out", stderr)
		}
	})

	// The last check: nothing the binary ever produced may carry a secret.
	t.Run("no canary reached any stream", func(t *testing.T) {
		for _, canary := range allCanaries() {
			if strings.Contains(seen.String(), canary) {
				t.Errorf("the canary %q reached the output", canary)
			}
		}
		for path, data := range filesUnder(t, dir) {
			for _, canary := range allCanaries() {
				if strings.Contains(data, canary) {
					t.Errorf("the canary %q reached the file %s", canary, path)
				}
			}
		}
	})
}

// filesUnder reads every regular file below dir, keyed by its path relative to dir. The binary itself is
// skipped: it is compiled output, not something a run produced.
func filesUnder(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "callbell" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			rel = path
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return files
}

// TestAPastedSecretNeverReachesTheOutput covers the case the canary check in TestEndToEnd cannot see. There
// the secrets live in the environment, so the redactor knows them. Here the secret is written straight into
// the configuration file, the way a user does who mistakes the credential field for a place to paste a
// token. Nothing that knows a real secret is involved, so every message about that field has to keep the
// configured text to itself.
//
// Both shapes matter. A pasted BookStack token is letters and digits, which is also exactly what a legal
// environment variable name looks like, so it passes validation and only fails later when no such variable
// is set. A token with other characters is rejected by validation instead. Neither path may echo it.
func TestAPastedSecretNeverReachesTheOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("the acceptance run builds the binary")
	}

	tests := []struct {
		name   string
		canary string
	}{
		{"shaped like a variable name", "Pasted3Canary7Token9Kx2Qm4Tz8Bv"},
		{"shaped like nothing else", "pasted/canary+token=6Rd1Nw"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := buildBinary(t, dir)
			server := mock(t, "Token unused:unused", []map[string]any{page(1, "Page")}, "<p>x</p>")

			// The canary is quoted, so the file is well-formed YAML and the failure happens in
			// validation or in resolution, not in the parser.
			configPath := filepath.Join(dir, "config.yaml")
			config := fmt.Sprintf(`version: 1
services:
  wiki:
    provider: bookstack
    base_url: %s
credentials:
  reader:
    type: env
    values:
      token-id: %q
      token-secret: %q
connections:
  primary:
    service: wiki
    credential: reader
defaults:
  connections:
    knowledge: primary
`, server.URL, tt.canary, tt.canary)
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatalf("writing the configuration: %v", err)
			}

			var seen strings.Builder
			c := &runner{
				bin: bin,
				env: []string{
					"HOME=" + dir,
					"PATH=" + os.Getenv("PATH"),
					"CALLBELL_CONFIG=" + configPath,
					secret.StoreSelector + "=none",
				},
				seen: &seen,
			}

			commands := [][]string{
				{"config", "validate"},
				{"capabilities"},
				{"capabilities", "--agent"},
				{"describe", "knowledge.pages.list"},
				{"knowledge", "pages", "list"},
				{"knowledge", "pages", "list", "--agent"},
				{"knowledge", "pages", "list", "--output", "json"},
				{"knowledge", "pages", "list", "--connection", "primary"},
				{"knowledge", "pages", "get", "1"},
				{"knowledge", "pages", "get", "1", "--output", "json"},
			}
			for _, args := range commands {
				_, stdout, stderr := c.run(t, args...)
				if strings.Contains(stdout, tt.canary) {
					t.Errorf("%v: the canary reached stdout: %q", args, stdout)
				}
				if strings.Contains(stderr, tt.canary) {
					t.Errorf("%v: the canary reached stderr: %q", args, stderr)
				}
			}

			for path, data := range filesUnder(t, dir) {
				// The configuration file is the user's own file; the canary is in it by construction.
				if path == "config.yaml" {
					continue
				}
				if strings.Contains(data, tt.canary) {
					t.Errorf("the canary reached the file %s", path)
				}
			}
		})
	}
}

// TestCredentialStoreEndToEnd drives the credential cascade through the built binary: a keyring
// credential that holds nothing in the configuration, the refusal to fall back to a plaintext file
// without being told to, the named way out, and an environment variable overriding what is stored.
//
// The machine's own credential store stays untouched: the runs disable it, which is also what a CI job
// and a container do.
func TestCredentialStoreEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("the acceptance run builds the binary")
	}

	const (
		storedID     = "canary-stored-id-5e21c7"
		storedSecret = "canary-stored-secret-b83f04"
		overrideID   = "canary-override-id-71ad9c"
	)

	dir := t.TempDir()
	bin := buildBinary(t, dir)
	server := mock(t, "Token "+storedID+":"+storedSecret, []map[string]any{page(1, "Vault Runbook")}, "<p>x</p>")

	configPath := filepath.Join(dir, "config.yaml")
	config := fmt.Sprintf(`version: 1
services:
  wiki:
    provider: bookstack
    base_url: %s
credentials:
  vault-reader:
    type: keyring
connections:
  wiki:
    service: wiki
    credential: vault-reader
defaults:
  connections:
    knowledge: wiki
`, server.URL)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}

	var seen strings.Builder
	baseEnv := []string{
		"HOME=" + dir,
		"PATH=" + os.Getenv("PATH"),
		"CALLBELL_CONFIG=" + configPath,
		secret.StoreSelector + "=none",
	}
	c := &runner{bin: bin, env: baseEnv, seen: &seen}
	fallback := filepath.Join(dir, secret.FileName)

	// pipe runs the binary with a secret on standard input, the way the command documents.
	pipe := func(t *testing.T, in string, args ...string) (int, string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = c.env
		cmd.Stdin = strings.NewReader(in)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("running %v: %v", args, err)
		}
		seen.WriteString(stdout.String())
		seen.WriteString(stderr.String())
		return code, stdout.String(), stderr.String()
	}

	t.Run("the configuration validates without holding a secret", func(t *testing.T) {
		code, stdout, stderr := c.run(t, "config", "validate")

		if code != 0 || stdout != "" || stderr != "" {
			t.Errorf("exit %d, stdout %q, stderr %q; want a silent success", code, stdout, stderr)
		}
	})

	t.Run("without a store and without the switch nothing is written", func(t *testing.T) {
		code, stdout, stderr := pipe(t, storedID, "credential", "set", "vault-reader", "token-id")

		if code != 2 {
			t.Errorf("exit %d, want 2 (stderr %q)", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "--plaintext") {
			t.Errorf("stderr = %q, want the named way out", stderr)
		}
		if _, err := os.Stat(fallback); !os.IsNotExist(err) {
			t.Fatalf("the plaintext fallback exists without the switch: %v", err)
		}
	})

	t.Run("the named fallback carries the run", func(t *testing.T) {
		for role, value := range map[string]string{"token-id": storedID, "token-secret": storedSecret} {
			if code, _, stderr := pipe(t, value, "credential", "set", "vault-reader", role, "--plaintext"); code != 0 {
				t.Fatalf("storing %s: exit %d (stderr %q)", role, code, stderr)
			}
		}
		info, err := os.Stat(fallback)
		if err != nil {
			t.Fatalf("the fallback was not written: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode().Perm())
		}

		code, stdout, stderr := c.run(t, "knowledge", "pages", "list", "--agent")

		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if !strings.Contains(stdout, "Vault Runbook") {
			t.Errorf("stdout = %q, want the page", stdout)
		}

		// The same secret, handed back by the provider: the file delivered it, and the redactor still
		// has to keep it out of the message.
		_, _, stderr = c.run(t, "knowledge", "pages", "get", echoPageID)
		if !strings.Contains(stderr, redact.Marker) {
			t.Errorf("stderr = %q, want the credential replaced by %s", stderr, redact.Marker)
		}
	})

	t.Run("a fallback others can read is refused", func(t *testing.T) {
		if err := os.Chmod(fallback, 0o644); err != nil {
			t.Fatalf("Chmod() = %v", err)
		}
		defer func() {
			if err := os.Chmod(fallback, 0o600); err != nil {
				t.Fatalf("Chmod() = %v", err)
			}
		}()

		code, stdout, stderr := c.run(t, "knowledge", "pages", "list")

		if code != 2 {
			t.Errorf("exit %d, want 2 (stderr %q)", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "plaintext file (readable by others)") {
			t.Errorf("stderr = %q, want the widened mode reported", stderr)
		}
		if !strings.Contains(stderr, "chmod 600 "+fallback) {
			t.Errorf("stderr = %q, want the command that fixes it", stderr)
		}
		// One state, one code: reading, writing and deleting all report the file.
		if !strings.HasPrefix(stderr, "callbell: config-invalid: ") {
			t.Errorf("stderr = %q, want the config-invalid code", stderr)
		}
	})

	t.Run("a delete that cannot clear the file says so", func(t *testing.T) {
		if err := os.Chmod(fallback, 0o644); err != nil {
			t.Fatalf("Chmod() = %v", err)
		}
		defer func() {
			if err := os.Chmod(fallback, 0o600); err != nil {
				t.Fatalf("Chmod() = %v", err)
			}
		}()

		code, stdout, stderr := c.run(t, "credential", "delete", "vault-reader", "token-id")

		if code != 2 {
			t.Errorf("exit %d, want 2 (stderr %q)", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.HasPrefix(stderr, "callbell: config-invalid: ") {
			t.Errorf("stderr = %q, want the same code the file gets when it is read", stderr)
		}
		for _, want := range []string{"may still be stored in the plaintext file", "chmod 600 " + fallback} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, want)
			}
		}
		// The store the user switched off is not what this is about, and it is not what is reported.
		if strings.Contains(stderr, "switched off") {
			t.Errorf("stderr = %q, want the real blocker rather than the store", stderr)
		}
	})

	t.Run("the source of every secret is visible", func(t *testing.T) {
		code, stdout, stderr := c.run(t, "config", "validate", "--secrets", "--agent")

		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		want := "connection|credential|role|source|checked\n" +
			"wiki|vault-reader|token-id|plaintext file|environment variable (not set), credential store (switched off)\n" +
			"wiki|vault-reader|token-secret|plaintext file|environment variable (not set), credential store (switched off)\n"
		if stdout != want {
			t.Errorf("stdout = %q,\nwant %q", stdout, want)
		}
	})

	t.Run("an environment variable overrides the stored secret and says so", func(t *testing.T) {
		shadowed := *c
		shadowed.env = append(append([]string{}, baseEnv...), "CALLBELL_VAULT_READER_TOKEN_ID="+overrideID)

		code, stdout, stderr := shadowed.run(t, "config", "validate", "--secrets", "--agent", "--fields", "role,source")

		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		want := "role|source\ntoken-id|environment variable\ntoken-secret|plaintext file\n"
		if stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}

		// The override really reaches the provider: the mock rejects the wrong token.
		code, _, stderr = shadowed.run(t, "knowledge", "pages", "list", "--agent")
		if code != 1 || !strings.HasPrefix(stderr, "callbell: auth: ") {
			t.Errorf("exit %d, stderr %q; want an auth failure", code, stderr)
		}
	})

	t.Run("deleting the last entry removes the fallback", func(t *testing.T) {
		for _, role := range []string{"token-id", "token-secret"} {
			if code, _, stderr := c.run(t, "credential", "delete", "vault-reader", role); code != 0 {
				t.Fatalf("deleting %s: exit %d (stderr %q)", role, code, stderr)
			}
		}
		if _, err := os.Stat(fallback); !os.IsNotExist(err) {
			t.Errorf("the emptied fallback was kept: %v", err)
		}
	})

	t.Run("no canary reached any stream", func(t *testing.T) {
		for _, canary := range []string{storedID, storedSecret, overrideID} {
			if strings.Contains(seen.String(), canary) {
				t.Errorf("the canary %q reached the output", canary)
			}
		}
		// The fallback file is the one place a secret may live, and it is gone by now anyway.
		for path, data := range filesUnder(t, dir) {
			for _, canary := range []string{storedID, storedSecret, overrideID} {
				if strings.Contains(data, canary) {
					t.Errorf("the canary %q reached the file %s", canary, path)
				}
			}
		}
	})
}

// TestEnvCredentialDoesNotFallThrough nails down that a credential of type env is resolved from the
// variable it names and from nothing else. The setup is the attack it prevents: the variables are unset,
// the way a CI run looks that forgot its secret, while a switched-on plaintext fallback beside the
// configuration holds a working token under the same credential name. The run must fail rather than
// authenticate with the identity that happens to lie next to the configuration.
func TestEnvCredentialDoesNotFallThrough(t *testing.T) {
	if testing.Short() {
		t.Skip("the acceptance run builds the binary")
	}

	const (
		fileID     = "canary-file-id-1d97a2"
		fileSecret = "canary-file-secret-6b04ce"
	)

	dir := t.TempDir()
	bin := buildBinary(t, dir)
	server := mock(t, "Token "+fileID+":"+fileSecret, []map[string]any{page(1, "Fallback Runbook")}, "<p>x</p>")

	configPath := filepath.Join(dir, "config.yaml")
	template := `version: 1
services:
  wiki:
    provider: bookstack
    base_url: %s
credentials:
  reader:
    type: %s
connections:
  wiki:
    service: wiki
    credential: reader
defaults:
  connections:
    knowledge: wiki
`
	envConfig := fmt.Sprintf(template, server.URL, "env\n    values:\n      token-id: E2E_UNSET_ID\n      token-secret: E2E_UNSET_SECRET")
	keyringConfig := fmt.Sprintf(template, server.URL, "keyring")
	if err := os.WriteFile(configPath, []byte(envConfig), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}

	// The fallback is the one a developer would have on the same machine, complete and switched on.
	fallback := fmt.Sprintf("version: 1\nallow_plaintext: true\ncredentials:\n  reader:\n    token-id: %s\n    token-secret: %s\n",
		fileID, fileSecret)
	if err := os.WriteFile(filepath.Join(dir, secret.FileName), []byte(fallback), 0o600); err != nil {
		t.Fatalf("writing the fallback: %v", err)
	}

	var seen strings.Builder
	c := &runner{
		bin: bin,
		env: []string{
			"HOME=" + dir,
			"PATH=" + os.Getenv("PATH"),
			"CALLBELL_CONFIG=" + configPath,
			secret.StoreSelector + "=none",
		},
		seen: &seen,
	}

	t.Run("the run fails instead of using the file", func(t *testing.T) {
		code, stdout, stderr := c.run(t, "knowledge", "pages", "list")

		if code != 2 {
			t.Errorf("exit %d, want 2 (stderr %q)", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "missing-secret") ||
			!strings.Contains(stderr, "credentials.reader.values.token-id") {
			t.Errorf("stderr = %q, want the missing secret reported", stderr)
		}
		if strings.Contains(stderr, string(secret.SourcePlaintext)) {
			t.Errorf("stderr = %q, want no stage beyond the variable", stderr)
		}
	})

	t.Run("the report names the environment variable as the only stage", func(t *testing.T) {
		code, stdout, stderr := c.run(t, "config", "validate", "--secrets", "--agent")

		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		want := "connection|credential|role|source|checked\n" +
			"wiki|reader|token-id|missing|environment variable (not set)\n" +
			"wiki|reader|token-secret|missing|environment variable (not set)\n"
		if stdout != want {
			t.Errorf("stdout = %q,\nwant %q", stdout, want)
		}
	})

	t.Run("the same file works for a keyring credential", func(t *testing.T) {
		if err := os.WriteFile(configPath, []byte(keyringConfig), 0o600); err != nil {
			t.Fatalf("writing the configuration: %v", err)
		}

		code, stdout, stderr := c.run(t, "knowledge", "pages", "list", "--agent")

		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if !strings.Contains(stdout, "Fallback Runbook") {
			t.Errorf("stdout = %q, want the page", stdout)
		}
	})

	t.Run("no canary reached any stream", func(t *testing.T) {
		for _, canary := range []string{fileID, fileSecret} {
			if strings.Contains(seen.String(), canary) {
				t.Errorf("the canary %q reached the output", canary)
			}
		}
	})
}
