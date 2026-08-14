package bookstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

// Canary values stand in for real tokens. No test needs a real secret.
const (
	canaryID     = "canary-token-id-4f21"
	canarySecret = "canary-token-secret-9ab3"
)

// recorder captures what a test server received, so a test can prove what was and was not sent.
type recorder struct {
	mu      sync.Mutex
	methods []string
	paths   []string
	queries []string
	auth    []string
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.methods = append(r.methods, req.Method)
	r.paths = append(r.paths, req.URL.Path)
	r.queries = append(r.queries, req.URL.RawQuery)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
}

func (r *recorder) snapshot() ([]string, []string, []string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.methods...), append([]string(nil), r.paths...),
		append([]string(nil), r.queries...), append([]string(nil), r.auth...)
}

func page(id int64, name string) map[string]any {
	return map[string]any{
		"id": id, "book_id": 7, "chapter_id": 0, "name": name,
		"slug": strings.ToLower(name), "created_at": "2026-01-01T00:00:00.000000Z",
		"updated_at": "2026-01-02T00:00:00.000000Z",
	}
}

// newClient wires a client to a test server using the connection model the configuration produces.
func newClient(t *testing.T, baseURL string, red *redact.Redactor) *Client {
	t.Helper()
	t.Setenv("TEST_TOKEN_ID", canaryID)
	t.Setenv("TEST_TOKEN_SECRET", canarySecret)

	client, err := Open(&config.Resolved{
		Name:     "wiki",
		Provider: Provider,
		BaseURL:  baseURL,
		EnvNames: map[string]string{roleTokenID: "TEST_TOKEN_ID", roleTokenSecret: "TEST_TOKEN_SECRET"},
	}, red)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	return client
}

func TestListPages(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{page(1, "Alpha"), page(2, "Beta")},
			"total": 2,
		})
	}))
	defer server.Close()

	got, err := newClient(t, server.URL, nil).ListPages(context.Background(), 10, 0)

	if err != nil {
		t.Fatalf("ListPages() = %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(got.Rows))
	}
	if got.Rows[0]["name"] != "Alpha" || got.Rows[1]["id"] != int64(2) {
		t.Errorf("rows = %v", got.Rows)
	}
	if want := []string{"id", "name", "slug", "book_id", "chapter_id", "created_at", "updated_at"}; !equal(got.Columns, want) {
		t.Errorf("columns = %v, want %v", got.Columns, want)
	}

	methods, paths, queries, auth := rec.snapshot()
	if methods[0] != http.MethodGet {
		t.Errorf("method = %s, want GET", methods[0])
	}
	if paths[0] != "/api/pages" {
		t.Errorf("path = %s", paths[0])
	}
	if !strings.Contains(queries[0], "count=10") || !strings.Contains(queries[0], "offset=0") {
		t.Errorf("query = %s, want the limit and offset passed through", queries[0])
	}
	if want := "Token " + canaryID + ":" + canarySecret; auth[0] != want {
		t.Errorf("authorization header = %q, want the Token form", auth[0])
	}
}

// Limit and offset are pushed down to the provider, and more pages are fetched only while they are needed.
func TestListPagesPagination(t *testing.T) {
	const total = 7
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		count, _ := strconv.Atoi(r.URL.Query().Get("count"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

		data := []map[string]any{}
		// The server never returns more than three records at once, whatever the client asked for.
		for i := offset; i < offset+count && i < total && len(data) < 3; i++ {
			data = append(data, page(int64(i+1), fmt.Sprintf("Page%d", i+1)))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "total": total})
	}))
	defer server.Close()

	client := newClient(t, server.URL, nil)

	t.Run("a limit is honoured across requests", func(t *testing.T) {
		got, err := client.ListPages(context.Background(), 5, 0)
		if err != nil {
			t.Fatalf("ListPages() = %v", err)
		}
		if len(got.Rows) != 5 {
			t.Errorf("rows = %d, want 5", len(got.Rows))
		}
		if got.Rows[0]["id"] != int64(1) || got.Rows[4]["id"] != int64(5) {
			t.Errorf("rows = %v", got.Rows)
		}
	})

	t.Run("no limit fetches everything", func(t *testing.T) {
		got, err := client.ListPages(context.Background(), 0, 0)
		if err != nil {
			t.Fatalf("ListPages() = %v", err)
		}
		if len(got.Rows) != total {
			t.Errorf("rows = %d, want %d", len(got.Rows), total)
		}
	})

	t.Run("offset skips records", func(t *testing.T) {
		got, err := client.ListPages(context.Background(), 2, 4)
		if err != nil {
			t.Fatalf("ListPages() = %v", err)
		}
		if len(got.Rows) != 2 || got.Rows[0]["id"] != int64(5) {
			t.Errorf("rows = %v, want two records starting at id 5", got.Rows)
		}
	})
}

// An instance that ignores the offset must not produce a list that looks complete but repeats records.
func TestListPagesStopsWithoutProgress(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		// The server claims nine pages but always answers with the same three.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total": 9,
			"data":  []map[string]any{page(1, "A"), page(2, "B"), page(3, "C")},
		})
	}))
	defer server.Close()

	got, err := newClient(t, server.URL, nil).ListPages(context.Background(), 0, 0)

	if err != nil {
		t.Fatalf("ListPages() = %v", err)
	}
	if len(got.Rows) != 3 {
		t.Errorf("rows = %d, want the three distinct records", len(got.Rows))
	}
	ids := map[any]int{}
	for _, r := range got.Rows {
		ids[r["id"]]++
	}
	for id, n := range ids {
		if n != 1 {
			t.Errorf("id %v appears %d times", id, n)
		}
	}
	if requests > 2 {
		t.Errorf("sent %d requests, want the loop to stop as soon as nothing new arrives", requests)
	}
}

func TestGetPage(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		p := page(42, "Runbook")
		// Content that would break a naive encoder, plus HTML and Markdown.
		p["html"] = "<p>a|b</p>\n<p>c=d</p>"
		p["markdown"] = "# Title\n\n- a\\b\n- c|d"
		_ = json.NewEncoder(w).Encode(p)
	}))
	defer server.Close()

	got, err := newClient(t, server.URL, nil).GetPage(context.Background(), "42")

	if err != nil {
		t.Fatalf("GetPage() = %v", err)
	}
	_, paths, _, _ := rec.snapshot()
	if paths[0] != "/api/pages/42" {
		t.Errorf("path = %s, want /api/pages/42", paths[0])
	}

	byName := map[string]any{}
	for _, f := range got.Fields {
		byName[f.Name] = f.Value
	}
	if byName["id"] != int64(42) || byName["name"] != "Runbook" {
		t.Errorf("fields = %v", byName)
	}
	// Content is passed through unchanged: nothing is rendered, escaped, or interpreted here.
	if byName["html"] != "<p>a|b</p>\n<p>c=d</p>" {
		t.Errorf("html = %q, want the response verbatim", byName["html"])
	}
	if byName["markdown"] != "# Title\n\n- a\\b\n- c|d" {
		t.Errorf("markdown = %q, want the response verbatim", byName["markdown"])
	}
}

// Every read path must use GET. A mutating request would be a contract violation.
func TestOnlyReadRequests(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{page(1, "A")}, "total": 1})
	}))
	defer server.Close()

	client := newClient(t, server.URL, nil)
	ctx := context.Background()
	if _, err := client.ListPages(ctx, 1, 0); err != nil {
		t.Fatalf("ListPages() = %v", err)
	}
	if _, err := client.GetPage(ctx, "1"); err != nil {
		t.Fatalf("GetPage() = %v", err)
	}
	if got := client.TestConnection(ctx); got != provider.ClassOK {
		t.Fatalf("TestConnection() = %q, want ok", got)
	}

	methods, _, _, _ := rec.snapshot()
	if len(methods) == 0 {
		t.Fatal("no request was recorded")
	}
	for i, m := range methods {
		if m != http.MethodGet {
			t.Errorf("request %d used %s, want GET", i, m)
		}
	}
}

func TestTestConnectionClasses(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    provider.Class
	}{
		{
			name: "ok",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "total": 0})
			},
			want: provider.ClassOK,
		},
		{
			name: "unauthorized is auth",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":401,"message":"No authorization token found on the request"}}`))
			},
			want: provider.ClassAuth,
		},
		{
			name: "forbidden is auth",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"denied"}}`))
			},
			want: provider.ClassAuth,
		},
		{
			name: "too many requests is rate limited",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Too Many Attempts."}}`))
			},
			want: provider.ClassRateLimited,
		},
		{
			name: "server error is a provider error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			want: provider.ClassProviderError,
		},
		{
			name: "unparsable body is a provider error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("not json"))
			},
			want: provider.ClassProviderError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			if got := newClient(t, server.URL, nil).TestConnection(context.Background()); got != tt.want {
				t.Errorf("TestConnection() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("a closed server is unreachable", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()

		if got := newClient(t, url, nil).TestConnection(context.Background()); got != provider.ClassUnreachable {
			t.Errorf("TestConnection() = %q, want unreachable", got)
		}
	})

	t.Run("a name that does not resolve is unreachable", func(t *testing.T) {
		got := newClient(t, "https://callbell-test.invalid", nil).TestConnection(context.Background())

		if got != provider.ClassUnreachable {
			t.Errorf("TestConnection() = %q, want unreachable", got)
		}
	})

	t.Run("an untrusted certificate is a TLS failure", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "total": 0})
		}))
		defer server.Close()

		// The client uses the system roots, so the test server's own certificate is not trusted.
		if got := newClient(t, server.URL, nil).TestConnection(context.Background()); got != provider.ClassTLS {
			t.Errorf("TestConnection() = %q, want tls", got)
		}
	})

	t.Run("a cancelled context is unreachable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(200 * time.Millisecond)
		}))
		defer server.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		if got := newClient(t, server.URL, nil).TestConnection(ctx); got != provider.ClassUnreachable {
			t.Errorf("TestConnection() = %q, want unreachable", got)
		}
	})
}

func TestRedirects(t *testing.T) {
	t.Run("a redirect within the same origin is followed", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/moved", http.StatusFound)
		})
		mux.HandleFunc("/moved", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{page(1, "A")}, "total": 1})
		})
		server := httptest.NewServer(mux)
		defer server.Close()

		got, err := newClient(t, server.URL, nil).ListPages(context.Background(), 1, 0)

		if err != nil {
			t.Fatalf("ListPages() = %v", err)
		}
		if len(got.Rows) != 1 {
			t.Errorf("rows = %d, want 1", len(got.Rows))
		}
	})

	t.Run("a cross-origin redirect is refused before the credential travels", func(t *testing.T) {
		rec := &recorder{}
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record(r)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "total": 0})
		}))
		defer elsewhere.Close()

		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+"/api/pages", http.StatusFound)
		}))
		defer origin.Close()

		_, err := newClient(t, origin.URL, nil).ListPages(context.Background(), 1, 0)

		if err == nil {
			t.Fatal("ListPages() = nil, want a refusal")
		}
		var perr *provider.Error
		if !errors.As(err, &perr) || perr.Class != provider.ClassProviderError {
			t.Fatalf("error = %v, want a provider error", err)
		}
		if !strings.Contains(err.Error(), "different origin") {
			t.Errorf("error = %q", err)
		}
		if methods, _, _, auth := rec.snapshot(); len(methods) != 0 {
			t.Errorf("the other origin received %d requests with authorization %v", len(methods), auth)
		}
	})
}

// Two connections to different servers and two credentials on one server stay separate.
func TestConnectionsStaySeparate(t *testing.T) {
	seen := map[string][]string{}
	var mu sync.Mutex
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = append(seen[name], r.Header.Get("Authorization"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{page(1, name)}, "total": 1,
			})
		}
	}
	first := httptest.NewServer(handler("first"))
	defer first.Close()
	second := httptest.NewServer(handler("second"))
	defer second.Close()

	t.Setenv("READER_ID", "reader-id-0001")
	t.Setenv("READER_SECRET", "reader-secret-0001")
	t.Setenv("AUDITOR_ID", "auditor-id-0002")
	t.Setenv("AUDITOR_SECRET", "auditor-secret-0002")

	open := func(baseURL, idEnv, secretEnv string) *Client {
		t.Helper()
		c, err := Open(&config.Resolved{
			Name: "c", Provider: Provider, BaseURL: baseURL,
			EnvNames: map[string]string{roleTokenID: idEnv, roleTokenSecret: secretEnv},
		}, nil)
		if err != nil {
			t.Fatalf("Open() = %v", err)
		}
		return c
	}

	reader := open(first.URL, "READER_ID", "READER_SECRET")
	auditor := open(first.URL, "AUDITOR_ID", "AUDITOR_SECRET")
	other := open(second.URL, "READER_ID", "READER_SECRET")

	ctx := context.Background()
	for _, c := range []*Client{reader, auditor, other} {
		if _, err := c.ListPages(ctx, 1, 0); err != nil {
			t.Fatalf("ListPages() = %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen["first"]) != 2 || len(seen["second"]) != 1 {
		t.Fatalf("requests = %v", seen)
	}
	if seen["first"][0] == seen["first"][1] {
		t.Errorf("two credentials on one server sent the same authorization")
	}
	if !strings.Contains(seen["first"][0], "reader-id-0001") || !strings.Contains(seen["first"][1], "auditor-id-0002") {
		t.Errorf("authorization headers = %v", seen["first"])
	}
	if seen["second"][0] != seen["first"][0] {
		t.Errorf("the same credential produced different headers on two servers")
	}
}

// Secrets must not reach any message, and the redactor must know them.
func TestNoSecretsInErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"denied"}}`))
	}))
	defer server.Close()

	red := &redact.Redactor{}
	client := newClient(t, server.URL, red)

	_, err := client.ListPages(context.Background(), 1, 0)

	if err == nil {
		t.Fatal("ListPages() = nil, want an error")
	}
	for _, canary := range []string{canaryID, canarySecret} {
		if strings.Contains(err.Error(), canary) {
			t.Errorf("error leaks a secret: %s", err)
		}
	}
	if got := red.Apply(canaryID + ":" + canarySecret); strings.Contains(got, canaryID) {
		t.Errorf("the redactor does not know the token: %q", got)
	}
}

// A credential that yields no secret is reported by its configuration key. The message repeats neither the
// secret nor the text the user put in the field, because that text may itself be a pasted token.
func TestOpenRequiresSecrets(t *testing.T) {
	// pasted is shaped like a real BookStack token: letters and digits only, so it also satisfies every
	// rule a legal environment variable name has to satisfy.
	const pasted = "Pasted7Token9Value2Canary4Kx8Qm1"

	tests := []struct {
		name     string
		envNames map[string]string
		set      map[string]string
	}{
		{"no token id role", map[string]string{roleTokenSecret: "S"}, map[string]string{"S": "x"}},
		{"no token secret role", map[string]string{roleTokenID: "I"}, map[string]string{"I": "x"}},
		{
			"variable not set",
			map[string]string{roleTokenID: "I", roleTokenSecret: "MISSING_ON_PURPOSE"},
			map[string]string{"I": "x"},
		},
		{
			"a token pasted into the field instead of a variable name",
			map[string]string{roleTokenID: pasted, roleTokenSecret: pasted},
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.set {
				t.Setenv(k, v)
			}
			t.Setenv("MISSING_ON_PURPOSE", "")
			t.Setenv(pasted, "")

			_, err := Open(&config.Resolved{
				Name: "c", Provider: Provider, BaseURL: "https://x.invalid",
				Credential: "reader", EnvNames: tt.envNames,
			}, nil)

			var missing *MissingSecretError
			if !errors.As(err, &missing) {
				t.Fatalf("Open() = %v, want a *MissingSecretError", err)
			}
			if !strings.Contains(err.Error(), "credentials.reader.values.") {
				t.Errorf("error = %q, want it to name the configuration key", err)
			}
			for _, forbidden := range []string{pasted, "MISSING_ON_PURPOSE"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Errorf("error = %q, want it to keep %q out", err, forbidden)
				}
			}
		})
	}
}

func TestRegister(t *testing.T) {
	reg := capability.NewRegistry()

	if err := Register(reg); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	got := reg.Provider(Provider)
	if len(got) != 2 {
		t.Fatalf("capabilities = %d, want 2", len(got))
	}
	for _, c := range got {
		if c.Risk != capability.RiskRead {
			t.Errorf("capability %q has risk %q, want read", c.Name, c.Risk)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
