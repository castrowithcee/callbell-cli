package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

const (
	canaryID     = "canary-token-id-4f21"
	canarySecret = "canary-token-secret-9ab3"
)

// bookstackConfig points a connection at a test server. Only environment variable names are stored.
func bookstackConfig(t *testing.T, baseURL string) string {
	t.Helper()
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
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
defaults: {}
`, baseURL))
}

// runInvoke drives the real command tree with a credential resolver that touches nothing outside the test:
// the process environment, an empty in-process store, and no plaintext fallback.
func runInvoke(t *testing.T, arguments string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	opts := testOptions(t, nil)
	opts.Input = strings.NewReader(arguments)
	code := run(newRootCommand(opts, defaultRegistry()), opts, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// invokeResult decodes the invoke envelope and returns the provider result.
func invokeResult(t *testing.T, stdout string) (string, int, string, json.RawMessage) {
	t.Helper()
	var envelope struct {
		Data struct {
			Operation  string          `json:"operation"`
			Version    int             `json:"version"`
			Connection string          `json:"connection"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("invoke output = %q: %v", stdout, err)
	}
	return envelope.Data.Operation, envelope.Data.Version, envelope.Data.Connection, envelope.Data.Result
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

// The BookStack reads the removed knowledge tree offered run through invoke, with the tool ID positional
// and only the schema arguments on stdin.
func TestBookStackInvokeReadsPages(t *testing.T) {
	cfg := bookstackConfig(t, pagesServer(t).URL)

	t.Run("list", func(t *testing.T) {
		code, stdout, stderr := runInvoke(t, `{"limit":50,"offset":0}`,
			"invoke", "bookstack.pages.list", "--config", cfg)
		if code != exitOK || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		operation, version, connection, result := invokeResult(t, stdout)
		if operation != "bookstack.pages.list" || version != 1 || connection != "wiki" {
			t.Errorf("route = %s v%d over %s", operation, version, connection)
		}
		var pages []map[string]any
		if err := json.Unmarshal(result, &pages); err != nil {
			t.Fatalf("result = %s: %v", result, err)
		}
		if len(pages) != 2 || pages[0]["name"] != "Alpha|Beta" || pages[1]["name"] != `back\slash` {
			t.Errorf("pages = %v", pages)
		}
	})

	t.Run("get keeps the untrusted content verbatim", func(t *testing.T) {
		code, stdout, stderr := runInvoke(t, `{"id":1}`, "invoke", "bookstack.pages.get", "--config", cfg)
		if code != exitOK || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		_, _, _, result := invokeResult(t, stdout)
		var page map[string]any
		if err := json.Unmarshal(result, &page); err != nil {
			t.Fatalf("result = %s: %v", result, err)
		}
		if page["html"] != "<p>a|b</p>\n<p>c=d</p>" || page["markdown"] != "# T\n\n- a\\b" {
			t.Errorf("page = %v", page)
		}
		if _, ok := page["id"].(float64); !ok {
			t.Errorf("id = %T, want a JSON number", page["id"])
		}
	})

	t.Run("empty stdin invokes without arguments", func(t *testing.T) {
		code, stdout, stderr := runInvoke(t, "", "invoke", "bookstack.pages.list", "--config", cfg)
		if code != exitOK || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		if _, _, connection, _ := invokeResult(t, stdout); connection != "wiki" {
			t.Errorf("connection = %q", connection)
		}
	})

	t.Run("an explicit connection selects the route", func(t *testing.T) {
		code, stdout, stderr := runInvoke(t, "", "invoke", "bookstack.pages.list",
			"--connection", "wiki", "--config", cfg)
		if code != exitOK || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		if _, _, connection, _ := invokeResult(t, stdout); connection != "wiki" {
			t.Errorf("connection = %q", connection)
		}
	})

	t.Run("an unknown connection is a usage error", func(t *testing.T) {
		code, stdout, stderr := runInvoke(t, "", "invoke", "bookstack.pages.list",
			"--connection", "absent", "--config", cfg)
		if code != exitUsage || stdout != "" || !strings.Contains(stderr, "unknown-connection") {
			t.Errorf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("an unknown tool is a usage error", func(t *testing.T) {
		code, stdout, stderr := runInvoke(t, "", "invoke", "bookstack.pages.absent", "--config", cfg)
		if code != exitUsage || stdout != "" || !strings.HasPrefix(stderr, "callbell: unknown-operation:") {
			t.Errorf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("a missing tool ID is a usage error", func(t *testing.T) {
		code, _, stderr := runInvoke(t, "", "invoke", "--config", cfg)
		if code != exitUsage || !strings.Contains(stderr, "expected exactly one tool ID") {
			t.Errorf("exit=%d stderr=%q", code, stderr)
		}
	})

	t.Run("arguments that are not one JSON object are a usage error", func(t *testing.T) {
		for _, arguments := range []string{"[]", "{}{}", "not json"} {
			code, stdout, stderr := runInvoke(t, arguments, "invoke", "bookstack.pages.list", "--config", cfg)
			if code != exitUsage || stdout != "" || !strings.HasPrefix(stderr, "callbell: invalid-request:") {
				t.Errorf("arguments %q: exit=%d stdout=%q stderr=%q", arguments, code, stdout, stderr)
			}
		}
	})
}

func TestInvokeArgumentsAreBoundedAndValidatedBeforeTheProvider(t *testing.T) {
	prefix, suffix := `{"padding":"`, `"}`
	exact := prefix + strings.Repeat("a", maxAgentRequestBytes-len(prefix)-len(suffix)) + suffix
	if len(exact) != maxAgentRequestBytes {
		t.Fatalf("fixture size = %d, want %d", len(exact), maxAgentRequestBytes)
	}
	arguments, err := readInvokeArguments(strings.NewReader(exact))
	if err != nil || string(arguments) != exact {
		t.Fatalf("readInvokeArguments(at limit) = %q, %v", arguments, err)
	}
	if _, err := readInvokeArguments(strings.NewReader(exact + " ")); err == nil ||
		!strings.Contains(err.Error(), "exceed 1048576 bytes") {
		t.Fatalf("readInvokeArguments(limit + 1) = %v", err)
	}
	if arguments, err := readInvokeArguments(strings.NewReader("  \n")); err != nil ||
		string(arguments) != "{}" {
		t.Fatalf("readInvokeArguments(empty) = %q, %v", arguments, err)
	}

	// An argument the input schema rejects never reaches a provider.
	var calls atomic.Int32
	reg := capability.NewRegistry()
	metadata, _ := defaultRegistry().ProviderMetadata("bookstack")
	if err := reg.RegisterProvider(metadata, nil); err != nil {
		t.Fatalf("RegisterProvider() = %v", err)
	}
	descriptor, _, _ := defaultRegistry().Lookup("bookstack.pages.list")
	if err := reg.Register("bookstack", capability.Operation{Descriptor: descriptor, Handler: func(
		context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor, json.RawMessage,
	) (any, error) {
		calls.Add(1)
		return output.Collection{}, nil
	}}); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, validConfig)

	var stdout, stderr bytes.Buffer
	opts := testOptions(t, nil)
	opts.Input = strings.NewReader(`{"limit":-1}`)
	code := run(newRootCommand(opts, reg), opts,
		[]string{"invoke", "bookstack.pages.list", "--config", cfg}, &stdout, &stderr)
	if code != exitUsage || stdout.Len() != 0 ||
		!strings.HasPrefix(stderr.String(), "callbell: invalid-request:") {
		t.Errorf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", calls.Load())
	}
}

// Two connections of one provider share the tool contract and stay separate on the wire.
func TestBookStackInvokeKeepsConnectionsSeparate(t *testing.T) {
	const (
		primaryID     = "primary-id-31b5"
		primarySecret = "primary-secret-a82d"
		auditID       = "audit-id-84c1"
		auditSecret   = "audit-secret-92ef"
	)
	var primaryCalls, auditCalls atomic.Int32
	server := func(label, auth string, calls *atomic.Int32) *httptest.Server {
		t.Helper()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if got := r.Header.Get("Authorization"); got != auth {
				t.Errorf("%s authorization = %q, want %q", label, got, auth)
			}
			page := map[string]any{
				"id": 7, "book_id": 3, "chapter_id": 0, "name": label, "slug": label,
				"created_at": "2026-08-20T00:00:00Z", "updated_at": "2026-08-20T01:00:00Z",
				"html": "<p>untrusted " + label + "</p>", "markdown": "# untrusted " + label,
			}
			if r.URL.Path == "/api/pages" {
				_ = json.NewEncoder(w).Encode(map[string]any{"total": 1, "data": []any{page}})
				return
			}
			if r.URL.Path == "/api/pages/7" {
				_ = json.NewEncoder(w).Encode(page)
				return
			}
			http.NotFound(w, r)
		}))
	}
	primary := server("primary", "Token "+primaryID+":"+primarySecret, &primaryCalls)
	defer primary.Close()
	audit := server("audit", "Token "+auditID+":"+auditSecret, &auditCalls)
	defer audit.Close()
	t.Setenv("PRIMARY_ID", primaryID)
	t.Setenv("PRIMARY_SECRET", primarySecret)
	t.Setenv("AUDIT_ID", auditID)
	t.Setenv("AUDIT_SECRET", auditSecret)
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, fmt.Sprintf(`
version: 1
services:
  primary:
    provider: bookstack
    base_url: %s
  audit:
    provider: bookstack
    base_url: %s
credentials:
  primary:
    type: env
    values:
      token-id: PRIMARY_ID
      token-secret: PRIMARY_SECRET
  audit:
    type: env
    values:
      token-id: AUDIT_ID
      token-secret: AUDIT_SECRET
connections:
  primary:
    service: primary
    credential: primary
  audit:
    service: audit
    credential: audit
defaults:
  connections:
    bookstack: primary
`, primary.URL, audit.URL))

	for _, tt := range []struct {
		name      string
		tool      string
		arguments string
	}{
		{"list", "bookstack.pages.list", `{"limit":50,"offset":0}`},
		{"get", "bookstack.pages.get", `{"id":7}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runInvoke(t, tt.arguments, "invoke", tt.tool,
				"--connection", "audit", "--config", cfg)
			if code != exitOK || stderr != "" {
				t.Fatalf("exit=%d stderr=%q", code, stderr)
			}
			_, _, connection, result := invokeResult(t, stdout)
			if connection != "audit" || !strings.Contains(string(result), "audit") {
				t.Errorf("connection = %q, result = %s", connection, result)
			}
		})
	}

	// Without --connection the provider default decides, which is the other instance.
	code, stdout, stderr := runInvoke(t, `{"id":7}`, "invoke", "bookstack.pages.get", "--config", cfg)
	if code != exitOK || stderr != "" {
		t.Fatalf("default route exit=%d stderr=%q", code, stderr)
	}
	if _, _, connection, result := invokeResult(t, stdout); connection != "primary" ||
		!strings.Contains(string(result), "primary") {
		t.Errorf("default connection = %q, result = %s", connection, result)
	}
	if primaryCalls.Load() != 1 || auditCalls.Load() != 2 {
		t.Errorf("provider calls = primary %d, audit %d, want 1 and 2", primaryCalls.Load(), auditCalls.Load())
	}
}

// A provider failure keeps its class and never republishes the credential it was handed.
func TestBookStackInvokeProviderFailures(t *testing.T) {
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

			code, stdout, stderr := runInvoke(t, "", "invoke", "bookstack.pages.list", "--config", cfg)
			if code != exitRuntime || stdout != "" {
				t.Errorf("exit=%d stdout=%q", code, stdout)
			}
			if !strings.HasPrefix(stderr, "callbell: "+tt.code+": ") {
				t.Errorf("stderr = %q, want the %s code", stderr, tt.code)
			}
		})
	}

	t.Run("an echoed credential is redacted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"error":{"message":%q}}`, r.Header.Get("Authorization"))
		}))
		defer server.Close()
		cfg := bookstackConfig(t, server.URL)

		code, stdout, stderr := runInvoke(t, "", "invoke", "bookstack.pages.list", "--config", cfg)
		if code != exitRuntime || stdout != "" {
			t.Errorf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		for _, canary := range []string{canaryID, canarySecret} {
			if strings.Contains(stderr, canary) {
				t.Errorf("stderr leaks %q: %s", canary, stderr)
			}
		}
		if !strings.Contains(stderr, redact.Marker) {
			t.Errorf("stderr = %q, want the redaction marker", stderr)
		}
	})
}

// A credential that yields no secret is a usage error naming the configuration key to fix, neither the
// secret nor the text the user wrote into the field.
func TestBookStackInvokeMissingSecret(t *testing.T) {
	cfg := bookstackConfig(t, pagesServer(t).URL)
	t.Setenv("TEST_TOKEN_SECRET", "")

	code, stdout, stderr := runInvoke(t, "", "invoke", "bookstack.pages.list", "--config", cfg)
	if code != exitUsage || stdout != "" {
		t.Errorf("exit=%d stdout=%q", code, stdout)
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

// A successful provider response can carry the credential back as untrusted payload. The canary includes
// characters JSON escapes, so the run proves redaction happens before encoding.
func TestBookStackInvokeRedactsSuccessfulPayloads(t *testing.T) {
	const escapedCanary = `canary-"\|=secret-3d72`

	auth := "Token " + canaryID + ":" + escapedCanary
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != auth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		page := map[string]any{
			"id": 1, "book_id": 7, "chapter_id": 0, "name": "before " + escapedCanary + " after",
			"slug": "page", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z",
			"html": "before " + escapedCanary + " after", "markdown": "# " + escapedCanary,
		}
		if r.URL.Path == "/api/pages" {
			_ = json.NewEncoder(w).Encode(map[string]any{"total": 1, "data": []map[string]any{page}})
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	cfg := bookstackConfig(t, server.URL)
	t.Setenv("TEST_TOKEN_SECRET", escapedCanary)

	for _, tt := range []struct{ tool, arguments string }{
		{"bookstack.pages.list", ""},
		{"bookstack.pages.get", `{"id":1}`},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			code, stdout, stderr := runInvoke(t, tt.arguments, "invoke", tt.tool, "--config", cfg)
			if code != exitOK || stderr != "" {
				t.Fatalf("exit=%d stderr=%q", code, stderr)
			}
			if strings.Contains(stdout, escapedCanary) || strings.Contains(stderr, escapedCanary) {
				t.Errorf("the canary reached the output:\nstdout: %s\nstderr: %s", stdout, stderr)
			}
			if !strings.Contains(stdout, redact.Marker) {
				t.Errorf("stdout = %q, want the redaction marker", stdout)
			}
			if _, _, _, result := invokeResult(t, stdout); !json.Valid(result) {
				t.Errorf("result = %s, want valid JSON", result)
			}
		})
	}
}
