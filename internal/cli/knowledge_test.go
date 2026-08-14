package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	canaryID     = "canary-token-id-4f21"
	canarySecret = "canary-token-secret-9ab3"
)

// bookstackConfig points a connection at a test server. Only environment variable names are stored.
func bookstackConfig(t *testing.T, baseURL string) string {
	t.Helper()
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("TEST_TOKEN_ID", canaryID)
	t.Setenv("TEST_TOKEN_SECRET", canarySecret)

	return writeConfig(t, fmt.Sprintf(`
version: 1
services:
  wiki:
    provider: bookstack
    base_url: %s
credentials:
  reader:
    type: env
    values:
      token-id: TEST_TOKEN_ID
      token-secret: TEST_TOKEN_SECRET
connections:
  wiki:
    service: wiki
    credential: reader
defaults:
  connections:
    knowledge: wiki
`, baseURL))
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	opts := &Options{}
	code := run(newRootCommand(opts, defaultRegistry()), opts, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// pagesServer answers the two read endpoints with content that stresses the encoders.
func pagesServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total": 2,
			"data": []map[string]any{
				{"id": 1, "book_id": 7, "chapter_id": 0, "name": "Alpha|Beta", "slug": "alpha",
					"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z"},
				{"id": 2, "book_id": 7, "chapter_id": 3, "name": `back\slash`, "slug": "back",
					"created_at": "2026-01-03T00:00:00Z", "updated_at": "2026-01-04T00:00:00Z"},
			},
		})
	})
	mux.HandleFunc("/api/pages/1", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "book_id": 7, "chapter_id": 0, "name": "Alpha|Beta", "slug": "alpha",
			"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z",
			"html":     "<p>a|b</p>\n<p>c=d</p>",
			"markdown": "# T\n\n- a\\b",
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestKnowledgePagesList(t *testing.T) {
	cfg := bookstackConfig(t, pagesServer(t).URL)

	t.Run("compact output escapes the content", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, "knowledge", "pages", "list", "--config", cfg, "--agent")

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
		}
		want := "id|name|slug|book_id|chapter_id|created_at|updated_at\n" +
			"1|Alpha\\|Beta|alpha|7|0|2026-01-01T00:00:00Z|2026-01-02T00:00:00Z\n" +
			"2|back\\\\slash|back|7|3|2026-01-03T00:00:00Z|2026-01-04T00:00:00Z\n"
		if stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})

	t.Run("json keeps the identifiers typed", func(t *testing.T) {
		_, stdout, _ := runCLI(t, "knowledge", "pages", "list", "--config", cfg, "--output", "json")

		var rows []map[string]any
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
			t.Fatalf("output is not JSON: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2", len(rows))
		}
		if _, ok := rows[0]["id"].(float64); !ok {
			t.Errorf("id = %T, want a number", rows[0]["id"])
		}
		if rows[0]["name"] != "Alpha|Beta" {
			t.Errorf("name = %v", rows[0]["name"])
		}
	})

	t.Run("projection selects fields", func(t *testing.T) {
		_, stdout, _ := runCLI(t, "knowledge", "pages", "list", "--config", cfg, "--agent", "--fields", "id,name")

		want := "id|name\n1|Alpha\\|Beta\n2|back\\\\slash\n"
		if stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("an explicit connection works", func(t *testing.T) {
		code, _, stderr := runCLI(t, "knowledge", "pages", "list", "--config", cfg, "--connection", "wiki", "--agent")

		if code != exitOK {
			t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
		}
	})

	t.Run("an unknown connection is a usage error", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, "knowledge", "pages", "list", "--config", cfg, "--connection", "absent")

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "unknown-connection") {
			t.Errorf("stderr = %q", stderr)
		}
	})
}

func TestKnowledgePagesGet(t *testing.T) {
	cfg := bookstackConfig(t, pagesServer(t).URL)

	t.Run("content is passed through escaped, not interpreted", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, "knowledge", "pages", "get", "1", "--config", cfg, "--agent")

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
		}
		want := "id=1\n" +
			"name=Alpha\\|Beta\n" +
			"slug=alpha\n" +
			"book_id=7\n" +
			"chapter_id=0\n" +
			"created_at=2026-01-01T00:00:00Z\n" +
			"updated_at=2026-01-02T00:00:00Z\n" +
			"html=<p>a\\|b</p>\\n<p>c\\=d</p>\n" +
			"markdown=# T\\n\\n- a\\\\b\n"
		if stdout != want {
			t.Errorf("stdout = %q,\nwant %q", stdout, want)
		}
	})

	t.Run("json keeps the real line breaks", func(t *testing.T) {
		_, stdout, _ := runCLI(t, "knowledge", "pages", "get", "1", "--config", cfg, "--output", "json")

		var page map[string]any
		if err := json.Unmarshal([]byte(stdout), &page); err != nil {
			t.Fatalf("output is not JSON: %v", err)
		}
		if page["html"] != "<p>a|b</p>\n<p>c=d</p>" {
			t.Errorf("html = %q", page["html"])
		}
	})

	t.Run("a non-numeric id is a usage error", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, "knowledge", "pages", "get", "abc", "--config", cfg)

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !strings.Contains(stderr, "callbell: usage:") {
			t.Errorf("stderr = %q", stderr)
		}
	})

	t.Run("a missing argument is a usage error", func(t *testing.T) {
		if code, _, _ := runCLI(t, "knowledge", "pages", "get", "--config", cfg); code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}

// A provider failure is a runtime error and carries its own code.
func TestKnowledgeProviderFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"code":401,"message":"denied"}}`, "auth"},
		{"rate limited", http.StatusTooManyRequests, `{"error":{"code":429,"message":"Too Many Attempts."}}`, "rate-limited"},
		{"server error", http.StatusInternalServerError, ``, "provider-error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			cfg := bookstackConfig(t, server.URL)

			code, stdout, stderr := runCLI(t, "knowledge", "pages", "list", "--config", cfg, "--agent")

			if code != exitRuntime {
				t.Errorf("exit code = %d, want %d", code, exitRuntime)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.HasPrefix(stderr, "callbell: "+tt.code+": ") {
				t.Errorf("stderr = %q, want the %s code", stderr, tt.code)
			}
		})
	}
}

// A credential that yields no secret is a usage error that names the configuration key to fix, neither the
// secret nor the text the user wrote into the field.
func TestKnowledgeMissingSecret(t *testing.T) {
	cfg := bookstackConfig(t, pagesServer(t).URL)
	t.Setenv("TEST_TOKEN_SECRET", "")

	code, stdout, stderr := runCLI(t, "knowledge", "pages", "list", "--config", cfg)

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "missing-secret") ||
		!strings.Contains(stderr, "credentials.reader.values.token-secret") {
		t.Errorf("stderr = %q", stderr)
	}
	for _, forbidden := range []string{canaryID, canarySecret, "TEST_TOKEN_SECRET"} {
		if strings.Contains(stderr, forbidden) {
			t.Errorf("stderr = %q, want it to keep %q out", stderr, forbidden)
		}
	}
}

// No canary value may appear in any stream, in the success case or in the failure case.
func TestKnowledgeNoCanaryInOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A hostile provider echoing the credential back must not get it published either.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"failed for ` + r.Header.Get("Authorization") + `"}}`))
	}))
	defer server.Close()
	cfg := bookstackConfig(t, server.URL)

	_, stdout, stderr := runCLI(t, "knowledge", "pages", "list", "--config", cfg, "--agent")

	for _, canary := range []string{canaryID, canarySecret} {
		if strings.Contains(stdout, canary) || strings.Contains(stderr, canary) {
			t.Errorf("a canary reached the output:\nstdout: %s\nstderr: %s", stdout, stderr)
		}
	}
	if !strings.Contains(stderr, "[redacted]") {
		t.Errorf("stderr = %q, want the redaction marker", stderr)
	}
}

// The BookStack capabilities are discoverable through the real registry.
func TestKnowledgeCapabilitiesAreDiscoverable(t *testing.T) {
	cfg := bookstackConfig(t, pagesServer(t).URL)

	code, stdout, stderr := runCLI(t, "capabilities", "--config", cfg, "--agent", "--fields", "name")

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	want := "name\nknowledge.pages.get\nknowledge.pages.list\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}
