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
)

// Canary values stand in for real tokens. No part of the acceptance run needs a real secret or a real
// BookStack instance.
const (
	canaryPrimaryID     = "canary-primary-id-3f7a91"
	canaryPrimarySecret = "canary-primary-secret-c4d208"
	canaryArchiveID     = "canary-archive-id-8b2e55"
	canaryArchiveSecret = "canary-archive-secret-1a9f6d"
)

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
			name string
			args []string
			code int
			in   string
		}{
			{"unknown flag", []string{"--nope"}, 2, "usage"},
			{"unknown command", []string{"frobnicate"}, 2, "usage"},
			{"unknown connection", []string{"knowledge", "pages", "list", "--connection", "absent"}, 2, "unknown-connection"},
			{"empty configuration file", []string{"capabilities", "--config", "/dev/null"}, 2, "config-invalid"},
			{"unknown capability", []string{"describe", "absent.capability"}, 2, "unsupported-capability"},
			{"unknown field", []string{"knowledge", "pages", "list", "--fields", "absent"}, 2, "usage"},
			{"missing page", []string{"knowledge", "pages", "get", "99"}, 1, "provider-error"},
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
		bare.env = []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "CALLBELL_CONFIG=" + configPath}

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
