package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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
defaults:
  connections:
    knowledge: wiki
`, baseURL))
}

// runCLI drives the command surface with a credential resolver that touches nothing outside the test: the
// process environment, an empty in-process store, and no plaintext fallback.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	opts := testOptions(t, nil)
	code := run(newRootCommand(opts, defaultRegistry()), opts, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func runBookStackRequest(t *testing.T, request string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	opts := testOptions(t, nil)
	opts.Input = strings.NewReader(request)
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

func TestKnowledgeCommandsUseApplicationHandlers(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, validConfig)

	reg := capability.NewRegistry()
	metadata, _ := defaultRegistry().ProviderMetadata("bookstack")
	if err := reg.RegisterProvider(metadata, nil); err != nil {
		t.Fatalf("RegisterProvider() = %v", err)
	}
	var listCalls, getCalls atomic.Int32
	registered := defaultRegistry().Provider("bookstack")
	for _, descriptor := range registered {
		descriptor := descriptor
		var handler capability.Handler
		switch descriptor.ID {
		case capabilityPagesList:
			handler = func(_ context.Context, resolved *config.Resolved, _ *secret.Resolver,
				_ *redact.Redactor, raw json.RawMessage) (any, error) {
				call := listCalls.Add(1)
				want := `{"limit":50,"offset":3}`
				if call == 2 {
					want = `{"limit":0,"offset":0}`
				}
				if resolved.Name != "wiki" || string(raw) != want {
					t.Errorf("list route = %q, arguments = %s", resolved.Name, raw)
				}
				return output.Collection{Columns: capabilityFieldNames(descriptor), Rows: []output.Row{{
					"id": int64(1), "name": "from-handler", "slug": "from-handler", "book_id": int64(2),
					"chapter_id": int64(0), "created_at": "created", "updated_at": "updated",
				}}}, nil
			}
		case capabilityPagesGet:
			handler = func(_ context.Context, resolved *config.Resolved, _ *secret.Resolver,
				_ *redact.Redactor, raw json.RawMessage) (any, error) {
				getCalls.Add(1)
				if resolved.Name != "wiki" || string(raw) != `{"id":7}` {
					t.Errorf("get route = %q, arguments = %s", resolved.Name, raw)
				}
				return output.Object{Fields: []output.Field{
					{Name: "id", Value: int64(7)}, {Name: "name", Value: "from-handler"},
					{Name: "slug", Value: "from-handler"}, {Name: "book_id", Value: int64(2)},
					{Name: "chapter_id", Value: int64(0)}, {Name: "created_at", Value: "created"},
					{Name: "updated_at", Value: "updated"}, {Name: "html", Value: "<b>untrusted</b>"},
					{Name: "markdown", Value: "# untrusted"},
				}}, nil
			}
		}
		if err := reg.Register("bookstack", capability.Operation{Descriptor: descriptor, Handler: handler}); err != nil {
			t.Fatalf("Register(%s) = %v", descriptor.ID, err)
		}
	}

	runCommand := func(args ...string) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		opts := testOptions(t, nil)
		code := run(newRootCommand(opts, reg), opts, append(args, "--config", cfg, "--output", "json"), &stdout, &stderr)
		return code, stderr.String()
	}
	if code, stderr := runCommand("knowledge", "pages", "list", "--offset", "3"); code != exitOK {
		t.Fatalf("list exit = %d, stderr = %q", code, stderr)
	}
	if code, stderr := runCommand("knowledge", "pages", "get", "7"); code != exitOK {
		t.Fatalf("get exit = %d, stderr = %q", code, stderr)
	}
	if code, stderr := runCommand("knowledge", "pages", "list", "--limit", "-1"); code != exitOK {
		t.Fatalf("unlimited list exit = %d, stderr = %q", code, stderr)
	}
	if listCalls.Load() != 2 || getCalls.Load() != 1 {
		t.Errorf("handler calls = list %d, get %d, want two and one", listCalls.Load(), getCalls.Load())
	}
}

func TestBookStackInvokeRejectsNegativeLimitBeforeProviderCall(t *testing.T) {
	var calls atomic.Int32
	reg := capability.NewRegistry()
	metadata, _ := defaultRegistry().ProviderMetadata("bookstack")
	if err := reg.RegisterProvider(metadata, nil); err != nil {
		t.Fatalf("RegisterProvider() = %v", err)
	}
	descriptor, _, _ := defaultRegistry().Lookup(capabilityPagesList)
	if err := reg.Register("bookstack", capability.Operation{Descriptor: descriptor, Handler: func(
		_ context.Context, _ *config.Resolved, _ *secret.Resolver, _ *redact.Redactor, _ json.RawMessage,
	) (any, error) {
		calls.Add(1)
		return output.Collection{}, nil
	}}); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, validConfig)
	request := `{"operation":"bookstack.pages.list","arguments":{"limit":-1}}`

	var stdout, stderr bytes.Buffer
	opts := testOptions(t, nil)
	opts.Input = strings.NewReader(request)
	code := run(newRootCommand(opts, reg), opts, []string{"invoke", "--config", cfg}, &stdout, &stderr)
	if code != exitUsage || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "callbell: invalid-request:") {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0", calls.Load())
	}
}

func TestBookStackOperationsAreSearchableAndDescribable(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	cfg := writeConfig(t, validConfig)

	code, stdout, stderr := runBookStackRequest(t,
		`{"query":"pages","provider":"bookstack"}`, "search", "--config", cfg)
	if code != exitOK || stderr != "" {
		t.Fatalf("search exit = %d, stderr = %q", code, stderr)
	}
	var search struct {
		Data struct {
			Operations []struct {
				ID string `json:"id"`
			} `json:"operations"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &search); err != nil {
		t.Fatalf("search output = %q: %v", stdout, err)
	}
	got := make([]string, len(search.Data.Operations))
	for i, operation := range search.Data.Operations {
		got[i] = operation.ID
	}
	if want := []string{capabilityPagesGet, capabilityPagesList}; !reflect.DeepEqual(got, want) {
		t.Errorf("search operations = %v, want %v", got, want)
	}

	for _, operation := range got {
		request := fmt.Sprintf(`{"operation":%q,"version":1}`, operation)
		code, stdout, stderr := runBookStackRequest(t, request, "describe", "--config", cfg)
		if code != exitOK || stderr != "" {
			t.Fatalf("describe %s exit = %d, stderr = %q", operation, code, stderr)
		}
		var described struct {
			Data struct {
				Operation capability.Descriptor `json:"operation"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &described); err != nil {
			t.Fatalf("describe output = %q: %v", stdout, err)
		}
		if described.Data.Operation.ID != operation || described.Data.Operation.Provider != "bookstack" ||
			described.Data.Operation.Version != 1 {
			t.Errorf("descriptor = %+v", described.Data.Operation)
		}
	}
}

func TestBookStackOperationsAndCommandsShareRoutesAndResults(t *testing.T) {
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
    knowledge: primary
`, primary.URL, audit.URL))

	for _, tt := range []struct {
		name      string
		operation string
		arguments string
		legacy    []string
	}{
		{"list", capabilityPagesList, `{"limit":50,"offset":0}`, []string{"knowledge", "pages", "list"}},
		{"list with legacy unlimited limit", capabilityPagesList, `{"limit":0,"offset":0}`, []string{"knowledge", "pages", "list", "--limit", "-1"}},
		{"get", capabilityPagesGet, `{"id":7}`, []string{"knowledge", "pages", "get", "7"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			legacyArgs := append(append([]string{}, tt.legacy...), "--connection", "audit", "--config", cfg, "--output", "json")
			code, legacy, stderr := runCLI(t, legacyArgs...)
			if code != exitOK {
				t.Fatalf("legacy exit = %d, stderr = %q", code, stderr)
			}
			request := fmt.Sprintf(`{"operation":%q,"connection":"audit","arguments":%s}`, tt.operation, tt.arguments)
			code, invoked, stderr := runBookStackRequest(t, request, "invoke", "--config", cfg)
			if code != exitOK {
				t.Fatalf("invoke exit = %d, stderr = %q", code, stderr)
			}
			var envelope struct {
				Data struct {
					Operation  string          `json:"operation"`
					Version    int             `json:"version"`
					Connection string          `json:"connection"`
					Result     json.RawMessage `json:"result"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(invoked), &envelope); err != nil {
				t.Fatalf("invoke output = %q: %v", invoked, err)
			}
			if envelope.Data.Operation != tt.operation || envelope.Data.Version != 1 || envelope.Data.Connection != "audit" {
				t.Errorf("invoke route = %+v", envelope.Data)
			}
			if !jsonEqual([]byte(legacy), envelope.Data.Result) {
				t.Errorf("legacy result = %s, invoke result = %s", legacy, envelope.Data.Result)
			}
		})
	}
	if primaryCalls.Load() != 0 || auditCalls.Load() != 6 {
		t.Errorf("provider calls = primary %d, audit %d, want 0 and 6", primaryCalls.Load(), auditCalls.Load())
	}
}

func jsonEqual(a, b []byte) bool {
	var av, bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && reflect.DeepEqual(av, bv)
}

func TestBookStackInvokeRedactsProviderErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"error":{"message":%q}}`, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	cfg := bookstackConfig(t, server.URL)
	request := `{"operation":"bookstack.pages.list","arguments":{"limit":1}}`

	code, stdout, stderr := runBookStackRequest(t, request, "invoke", "--config", cfg)
	if code != exitRuntime || stdout != "" {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	for _, canary := range []string{canaryID, canarySecret} {
		if strings.Contains(stderr, canary) {
			t.Errorf("stderr leaks %q: %s", canary, stderr)
		}
	}
	if !strings.Contains(stderr, redact.Marker) {
		t.Errorf("stderr = %q, want redaction marker", stderr)
	}
}

func TestKnowledgeFieldsAreValidatedBeforeProviderCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 0, "data": []any{}})
	}))
	t.Cleanup(server.Close)
	cfg := bookstackConfig(t, server.URL)

	tests := []struct {
		name      string
		args      []string
		available string
	}{
		{
			name:      "list",
			args:      []string{"knowledge", "pages", "list", "--config", cfg, "--fields", "absent"},
			available: "id, name, slug, book_id, chapter_id, created_at, updated_at",
		},
		{
			name:      "get",
			args:      []string{"knowledge", "pages", "get", "1", "--config", cfg, "--fields", "absent"},
			available: "id, name, slug, book_id, chapter_id, created_at, updated_at, html, markdown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)

			if code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			want := "callbell: usage: unknown field \"absent\", available fields are " + tt.available + "\n"
			if !strings.HasPrefix(stderr, want) {
				t.Errorf("stderr = %q, want prefix %q", stderr, want)
			}
			if got := calls.Load(); got != 0 {
				t.Errorf("provider calls = %d, want 0", got)
			}
		})
	}
}

func TestKnowledgeEmptyResultAcceptsDeclaredField(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 0, "data": []any{}})
	}))
	t.Cleanup(server.Close)
	cfg := bookstackConfig(t, server.URL)

	code, stdout, stderr := runCLI(t,
		"knowledge", "pages", "list", "--config", cfg, "--agent", "--fields", "id")

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if stdout != "id\n" {
		t.Errorf("stdout = %q, want the projected header", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want 1", got)
	}
}

func TestKnowledgeCapabilityFieldsMatchResults(t *testing.T) {
	cfg := bookstackConfig(t, pagesServer(t).URL)
	reg := defaultRegistry()

	tests := []struct {
		name       string
		capability string
		args       []string
		fields     func(string) []string
	}{
		{
			name:       "list",
			capability: capabilityPagesList,
			args:       []string{"knowledge", "pages", "list", "--config", cfg, "--agent"},
			fields: func(stdout string) []string {
				return strings.Split(strings.SplitN(stdout, "\n", 2)[0], "|")
			},
		},
		{
			name:       "get",
			capability: capabilityPagesGet,
			args:       []string{"knowledge", "pages", "get", "1", "--config", cfg, "--agent"},
			fields: func(stdout string) []string {
				lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
				fields := make([]string, len(lines))
				for i, line := range lines {
					fields[i] = strings.SplitN(line, "=", 2)[0]
				}
				return fields
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)
			if code != exitOK {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
			}

			var declared []string
			for _, c := range reg.Provider("bookstack") {
				if c.ID == tt.capability {
					declared = capabilityFieldNames(c)
					break
				}
			}
			if declared == nil {
				t.Fatalf("capability %q is not registered", tt.capability)
			}
			if got := tt.fields(stdout); !reflect.DeepEqual(got, declared) {
				t.Errorf("result fields = %v, declared fields = %v", got, declared)
			}
		})
	}
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

// A successful provider response can carry the credential back as untrusted payload. The canary includes
// characters JSON and compact escape differently, so the run proves redaction happens before encoding.
func TestKnowledgeSuccessfulPayloadsAreRedacted(t *testing.T) {
	const escapedCanary = `canary-"\|=secret-3d72`

	auth := "Token " + canaryID + ":" + escapedCanary
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != auth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/pages/1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "Page", "html": "before " + escapedCanary + " after",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total": 1,
			"data":  []map[string]any{{"id": 1, "name": "before " + escapedCanary + " after"}},
		})
	}))
	defer server.Close()
	cfg := bookstackConfig(t, server.URL)
	t.Setenv("TEST_TOKEN_SECRET", escapedCanary)

	tests := []struct {
		name string
		args []string
	}{
		{"collection/table", []string{"knowledge", "pages", "list", "--output", "table"}},
		{"collection/json", []string{"knowledge", "pages", "list", "--output", "json"}},
		{"collection/compact", []string{"knowledge", "pages", "list", "--output", "compact"}},
		{"object/table", []string{"knowledge", "pages", "get", "1", "--output", "table"}},
		{"object/json", []string{"knowledge", "pages", "get", "1", "--output", "json"}},
		{"object/compact", []string{"knowledge", "pages", "get", "1", "--output", "compact"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, append(tt.args, "--config", cfg)...)

			if code != exitOK {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
			}
			if strings.Contains(stdout, escapedCanary) || strings.Contains(stderr, escapedCanary) {
				t.Errorf("the canary reached the output:\nstdout: %s\nstderr: %s", stdout, stderr)
			}
			if !strings.Contains(stdout, redact.Marker) {
				t.Errorf("stdout = %q, want the redaction marker", stdout)
			}
			if strings.HasSuffix(tt.name, "/json") {
				var id any
				if strings.HasPrefix(tt.name, "object/") {
					var record map[string]any
					if err := json.Unmarshal([]byte(stdout), &record); err != nil {
						t.Fatalf("stdout is not valid object JSON: %v", err)
					}
					id = record["id"]
				} else {
					var records []map[string]any
					if err := json.Unmarshal([]byte(stdout), &records); err != nil {
						t.Fatalf("stdout is not valid collection JSON: %v", err)
					}
					id = records[0]["id"]
				}
				if _, ok := id.(float64); !ok {
					t.Errorf("id = %T, want a JSON number", id)
				}
			}
		})
	}
}

// The BookStack capabilities are discoverable through the real registry.
func TestKnowledgeCapabilitiesAreDiscoverable(t *testing.T) {
	cfg := bookstackConfig(t, pagesServer(t).URL)

	code, stdout, stderr := runCLI(t, "capabilities", "--config", cfg, "--agent", "--fields", "name")

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	want := "name\nbookstack.pages.get\nbookstack.pages.list\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}
