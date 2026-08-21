package seatable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/provider/ratelimit"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// The canaries stand for API tokens, for the base tokens they are exchanged for, for a cell value, and
// for a provider body. No test reaches a productive installation: every request is answered by the
// package's own transport seam.
const (
	salesToken   = "canary-seatable-api-token-sales-6b19"
	auditToken   = "canary-seatable-api-token-audit-2f74"
	supportToken = "canary-seatable-api-token-support-8d35"
	onpremToken  = "canary-seatable-api-token-onprem-4a06"

	salesEnv   = "TEST_SEATABLE_SALES"
	auditEnv   = "TEST_SEATABLE_AUDIT"
	supportEnv = "TEST_SEATABLE_SUPPORT"
	onpremEnv  = "TEST_SEATABLE_ONPREM"

	salesBase   = "5c264e76-0e5a-448a-9f34-580b551364ca"
	supportBase = "9a1f3c22-77b4-4a1e-8f0d-2c5b6e7a9014"
	onpremBase  = "3e5d7c11-22aa-4bb3-9c4d-6f8e0a1b2c3d"

	salesBaseToken   = "canary-seatable-base-token-sales-1a2b3c"
	auditBaseToken   = "canary-seatable-base-token-audit-4d5e6f"
	supportBaseToken = "canary-seatable-base-token-support-7g8h"
	onpremBaseToken  = "canary-seatable-base-token-onprem-9i0j"

	rowID      = "Qtf7xPmoRaiFyQPO1aENTj"
	otherRowID = "Ab12Cd34Ef56Gh78Ij90Kl"

	cellCanary = "cell-value-canary-seatable-7c1e"
	bodyCanary = "provider-body-canary-seatable-3b8a"

	selfHosted = "https://seatable.example.invalid"
)

// issued binds every API token of the fixtures to the base it belongs to and to the base token the
// exchange hands out. Two tokens address the same base, which is what a base-scoped credential means.
var issued = map[string]struct{ base, token string }{
	salesToken:   {salesBase, salesBaseToken},
	auditToken:   {salesBase, auditBaseToken},
	supportToken: {supportBase, supportBaseToken},
	onpremToken:  {onpremBase, onpremBaseToken},
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// serve replaces the package transport for one test. Production keeps Go's default transport.
func serve(t *testing.T, handler func(*http.Request) (*http.Response, error)) {
	t.Helper()
	previous := transport
	transport = roundTripFunc(handler)
	t.Cleanup(func() { transport = previous })
}

// serveBase answers the token exchange of every fixture token and delegates the base routes to next.
func serveBase(t *testing.T, next func(*http.Request) (*http.Response, error)) {
	t.Helper()
	serve(t, func(request *http.Request) (*http.Response, error) {
		if response, handled := exchange(request); handled {
			return response, nil
		}
		return next(request)
	})
}

// exchange answers the account route that turns an API token into a base token.
func exchange(request *http.Request) (*http.Response, bool) {
	if request.URL.Path != baseTokenPath {
		return nil, false
	}
	base, ok := issued[strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		return jsonResponse(http.StatusUnauthorized, `{"detail":"Invalid token."}`), true
	}
	return jsonResponse(http.StatusOK, fmt.Sprintf(
		`{"app_name":"callbell","access_token":%q,"dtable_uuid":%q,"dtable_server":%q,`+
			`"workspace_id":7,"dtable_name":"Sales","use_api_gateway":true}`,
		base.token, base.base, "https://"+request.URL.Host+"/dtable-server/")), true
}

// refuse fails the test when any provider request is attempted.
func refuse(t *testing.T) {
	t.Helper()
	serve(t, func(request *http.Request) (*http.Response, error) {
		t.Errorf("the provider was contacted: %s %s", request.Method, request.URL.Path)
		return nil, errors.New("no request expected")
	})
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func resolvedConnection(name, credential, env, origin, target string) *config.Resolved {
	return &config.Resolved{
		Name: name, Provider: Provider, BaseURL: origin, Service: "seatable", Credential: credential,
		Target: target,
		Secrets: config.Credential{
			Type: config.CredentialTypeEnv, Values: map[string]string{roleAPIToken: env},
		},
	}
}

func resolver(red *redact.Redactor) *secret.Resolver {
	return secret.NewWith(func(name string) string {
		switch name {
		case salesEnv:
			return salesToken
		case auditEnv:
			return auditToken
		case supportEnv:
			return supportToken
		case onpremEnv:
			return onpremToken
		}
		return ""
	}, nil, nil, red)
}

// freeLimiter is a limiter that never waits. The rate limit itself is proven by its own tests.
func freeLimiter() *ratelimit.Limiter {
	return ratelimit.New(0, time.Now, ratelimit.Sleep)
}

// stubLimiter gives one API token a limiter without waiting time, so a test that performs several
// requests does not sleep.
func stubLimiter(t *testing.T, token string) {
	t.Helper()
	t.Cleanup(limiters.Replace(token, freeLimiter()))
}

// client opens the sales connection with the package transport currently installed.
func client(t *testing.T, target string) (*Client, *redact.Redactor) {
	t.Helper()
	red := &redact.Redactor{}
	c, err := open(resolvedConnection("sales", "sales-reader", salesEnv, cloudOrigin, target),
		resolver(red), red, freeLimiter())
	if err != nil {
		t.Fatalf("open() = %v", err)
	}
	return c, red
}

// rowsBody carries the system keys of a row, a cell canary, and a second row, so the stable envelope can
// be proven to keep the metadata it promises and to drop everything else.
const rowsBody = `{"rows":[
 {"_id":"` + rowID + `","_ctime":"2026-03-01T09:00:00+01:00","_mtime":"2026-04-02T11:15:00+01:00",
  "_creator":"automation@seatable.invalid","_last_modifier":"automation@seatable.invalid",
  "Name":"Bike & Ride GmbH","Betrag":1250.5,"Notiz":"` + cellCanary + `","Tags":["a","b"]},
 {"_id":"` + otherRowID + `","_ctime":"2026-03-04T09:00:00+01:00","_mtime":"2026-03-04T09:00:00+01:00",
  "Name":"Test GmbH","Betrag":0}
]}`

const rowBody = `{"_id":"` + rowID + `","_ctime":"2026-03-01T09:00:00+01:00",
 "_mtime":"2026-04-02T11:15:00+01:00","_creator":"automation@seatable.invalid",
 "Name":"Bike & Ride GmbH","Notiz":"` + cellCanary + `"}`

// metadataBody is the metadata of a base that holds both fixture tables and their views.
const metadataBody = `{"metadata":{"tables":[
 {"_id":"0000","name":"Kunden","columns":[{"key":"0000","name":"Name"}],
  "views":[{"_id":"0000","name":"Standard"},{"_id":"7Yk3","name":"Aktive"}]},
 {"_id":"0001","name":"Tickets","columns":[],"views":[{"_id":"0000","name":"Standard"}]}
],"version":42,"format_version":9}}`

func rowsRoute(base string) string { return gatewayPath + base + rowsPath }
func metaRoute(base string) string { return gatewayPath + base + metadataPath }
func rowRoute(base, id string) string {
	return gatewayPath + base + rowsPath + id + "/"
}

// Register publishes the configuration metadata the TUI needs and exactly two read-only operations. The
// credential role points at an API token with the read permission, and the target is required.
func TestRegisterPublishesMetadataAndTwoReadOnlyOperations(t *testing.T) {
	reg := capability.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	metadata, ok := reg.ProviderMetadata(Provider)
	if !ok || metadata.Name != "SeaTable" || metadata.DefaultBaseURL != cloudOrigin ||
		len(metadata.SecretRoles) != 1 || metadata.SecretRoles[0].Name != roleAPIToken {
		t.Fatalf("metadata = %+v, %v", metadata, ok)
	}
	// The TUI has to name the least privilege this slice needs, and the fixed table it binds.
	if !strings.Contains(metadata.SecretRoles[0].Description, "permission r") {
		t.Errorf("role description = %q, want the read-only permission named", metadata.SecretRoles[0].Description)
	}
	if !metadata.Target.Required || metadata.Target.Label != "table" ||
		!strings.Contains(metadata.Target.Description, "TABLE/VIEW") {
		t.Errorf("target metadata = %+v, want a required table with an optional view", metadata.Target)
	}

	operations := reg.Provider(Provider)
	if len(operations) != 2 {
		t.Fatalf("operations = %d, want the list and the get operation", len(operations))
	}
	for _, descriptor := range operations {
		if descriptor.Version != 1 || descriptor.Provider != Provider ||
			!descriptor.RequiresExplicitConnection ||
			descriptor.Risk.Effect != capability.EffectRead ||
			descriptor.Risk.Idempotency != capability.IdempotencySafe ||
			descriptor.Risk.Confirmation != capability.ConfirmationNone ||
			!descriptor.Risk.OpenWorld || descriptor.Risk.DataSensitivity != dataSensitivity {
			t.Errorf("descriptor %s = %+v, want a safe read requiring an explicit connection",
				descriptor.ID, descriptor)
		}
		// Neither the base, the table, the view, nor a URL is an argument of the contract.
		for _, forbidden := range []string{"base", "table", "view", "url", "token", "sql"} {
			if strings.Contains(string(descriptor.InputSchema), forbidden) {
				t.Errorf("the input schema of %s offers %q: %s", descriptor.ID, forbidden, descriptor.InputSchema)
			}
		}
	}
	if operations[0].ID != "seatable.rows.get" || operations[1].ID != "seatable.rows.list" {
		t.Errorf("operation IDs = %s, %s", operations[0].ID, operations[1].ID)
	}
}

// Open binds the instance, the fixed table, and the API token of one connection, and refuses everything
// that is not a usable instance or target before any request happens.
func TestOpenBindsTheInstanceTargetAndToken(t *testing.T) {
	refuse(t)

	t.Run("the API token is registered for redaction", func(t *testing.T) {
		c, red := client(t, "Kunden")
		if c.origin != cloudOrigin || c.target.tableParam != "table_name" || c.target.table != "Kunden" {
			t.Fatalf("client = %+v", c)
		}
		if red.Apply("token "+salesToken) != "token "+redact.Marker {
			t.Error("the API token was not registered for redaction")
		}
	})

	t.Run("an unusable instance is refused", func(t *testing.T) {
		red := &redact.Redactor{}
		for _, origin := range []string{
			"http://cloud.seatable.io", "https://cloud.seatable.io/api", "https://user:pw@cloud.seatable.io",
			"https://cloud.seatable.io?x=1", "", "not a url",
		} {
			resolved := resolvedConnection("sales", "sales-reader", salesEnv, origin, "Kunden")
			if _, err := open(resolved, resolver(red), red, freeLimiter()); err == nil {
				t.Errorf("open() accepted the origin %q", origin)
			}
		}
	})

	t.Run("an unusable target is refused", func(t *testing.T) {
		red := &redact.Redactor{}
		for _, target := range []string{
			"", "   ", "/Aktive", "Kunden/", "id:", "id:" + strings.Repeat("a", 33),
			"id:0000/id:../..", "Kunden\x00", strings.Repeat("a", maxTargetLength+1),
		} {
			resolved := resolvedConnection("sales", "sales-reader", salesEnv, cloudOrigin, target)
			if _, err := open(resolved, resolver(red), red, freeLimiter()); err == nil {
				t.Errorf("open() accepted the target %q", target)
			}
		}
	})

	t.Run("a credential without a secret is refused", func(t *testing.T) {
		red := &redact.Redactor{}
		resolved := resolvedConnection("sales", "sales-reader", "TEST_SEATABLE_ABSENT", cloudOrigin, "Kunden")
		if _, err := open(resolved, resolver(red), red, freeLimiter()); err == nil {
			t.Error("open() accepted a credential without a secret")
		}
	})
}

// Every configured target form becomes exactly one pair of fixed query parameters, and nothing else.
func TestTargetFormsBecomeTheFixedQueryParameters(t *testing.T) {
	tests := []struct {
		target string
		want   map[string]string
	}{
		{"Kunden", map[string]string{"table_name": "Kunden"}},
		{" Kunden / Aktive ", map[string]string{"table_name": "Kunden", "view_name": "Aktive"}},
		{"id:0000", map[string]string{"table_id": "0000"}},
		{"id:0000/id:7Yk3", map[string]string{"table_id": "0000", "view_id": "7Yk3"}},
		{"Kunden/id:7Yk3", map[string]string{"table_name": "Kunden", "view_id": "7Yk3"}},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			var got url.Values
			serveBase(t, func(request *http.Request) (*http.Response, error) {
				got = request.URL.Query()
				return jsonResponse(http.StatusOK, `{"rows":[]}`), nil
			})
			c, _ := client(t, tt.target)
			if _, err := c.ListRows(context.Background(), ListOptions{}); err != nil {
				t.Fatalf("ListRows() = %v", err)
			}
			want := url.Values{"start": {"0"}, "limit": {"25"}, "convert_keys": {"true"}}
			for key, value := range tt.want {
				want.Set(key, value)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("query = %v, want %v", got, want)
			}
		})
	}
}

// The list request goes to the API gateway of the configured instance, carries the base token, and sends
// only the fixed target and the bounded paging parameters.
func TestListRowsSendsOnlyBoundedPagingAndTheFixedTarget(t *testing.T) {
	var requests []string
	var authorizations []string
	serveBase(t, func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.Method+" "+request.URL.Path+"?"+request.URL.RawQuery)
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		if request.URL.Scheme != "https" || request.URL.Host != "cloud.seatable.io" {
			t.Errorf("request left the configured instance: %s", request.URL.Redacted())
		}
		return jsonResponse(http.StatusOK, rowsBody), nil
	})

	c, _ := client(t, "Kunden/Aktive")
	if _, err := c.ListRows(context.Background(), ListOptions{Start: 50, Limit: 100}); err != nil {
		t.Fatalf("ListRows() = %v", err)
	}

	want := []string{
		"GET " + rowsRoute(salesBase) + "?convert_keys=true&limit=100&start=50&table_name=Kunden&view_name=Aktive",
	}
	if !reflect.DeepEqual(requests, want) {
		t.Errorf("requests = %v, want %v", requests, want)
	}
	if len(authorizations) != 1 || authorizations[0] != "Bearer "+salesBaseToken {
		t.Errorf("authorizations = %v, want the exchanged base token", authorizations)
	}

	// The bounds hold for a direct caller as well, and a rejected page never becomes a request.
	for _, options := range []ListOptions{{Limit: 101}, {Limit: -1}, {Start: -1}, {Start: maxStart + 1}} {
		if _, err := c.ListRows(context.Background(), options); err == nil {
			t.Errorf("ListRows(%+v) was accepted", options)
		}
	}
	if len(requests) != 1 {
		t.Errorf("requests = %v, want the rejected pages to stay local", requests)
	}
}

// A row becomes the stable envelope: identifier, time metadata, and the column values of the base. Every
// other system key is dropped, and the page reports how a caller reaches the next one.
func TestListRowsNormalizesTheRowEnvelope(t *testing.T) {
	serveBase(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, rowsBody), nil
	})
	c, _ := client(t, "Kunden")

	page, err := c.ListRows(context.Background(), ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ListRows() = %v", err)
	}
	if len(page.Rows) != 2 || page.Start != 0 || page.Limit != 2 || !page.HasMore || page.NextStart != 2 {
		t.Fatalf("page = %+v", page)
	}
	first := page.Rows[0]
	if first.ID != rowID || first.CreatedAt != "2026-03-01T09:00:00+01:00" ||
		first.UpdatedAt != "2026-04-02T11:15:00+01:00" {
		t.Errorf("envelope = %+v", first)
	}
	if len(first.Values) != 4 || string(first.Values["Betrag"]) != "1250.5" ||
		string(first.Values["Tags"]) != `["a","b"]` {
		t.Errorf("values = %v, want exactly the four columns of the row", first.Values)
	}
	for _, dropped := range []string{"_id", "_ctime", "_mtime", "_creator", "_last_modifier"} {
		if _, ok := first.Values[dropped]; ok {
			t.Errorf("the value map carries the system key %q", dropped)
		}
	}

	// A short page is the last one.
	page, err = c.ListRows(context.Background(), ListOptions{Limit: 25})
	if err != nil {
		t.Fatalf("ListRows() = %v", err)
	}
	if page.HasMore || page.NextStart != 0 {
		t.Errorf("page = %+v, want the last page", page)
	}
}

// An answer that ignores the requested page size, or a row without an identifier, is a provider problem
// rather than a result.
func TestListRowsRejectsAnUnusableAnswer(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"more rows than requested", `{"rows":[{"_id":"` + rowID + `"},{"_id":"` + otherRowID + `"}]}`},
		{"a row without an identifier", `{"rows":[{"Name":"Bike"}]}`},
		{"a row with an unusable identifier", `{"rows":[{"_id":"` + strings.Repeat("x", 64) + `"}]}`},
		{"not an object", `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveBase(t, func(*http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, tt.body), nil
			})
			c, _ := client(t, "Kunden")
			_, err := c.ListRows(context.Background(), ListOptions{Limit: 1})
			if class := classOf(err); class != provider.ClassInvalidResponse {
				t.Fatalf("class = %q, want %q (%v)", class, provider.ClassInvalidResponse, err)
			}
		})
	}
}

// The get operation reads exactly the row of a validated identifier from the fixed table of the same
// connection, and refuses an answer that is a different row.
func TestGetRowReadsExactlyTheValidatedRow(t *testing.T) {
	t.Run("the request names the row and the fixed table", func(t *testing.T) {
		var requests []string
		serveBase(t, func(request *http.Request) (*http.Response, error) {
			requests = append(requests, request.URL.Path+"?"+request.URL.RawQuery)
			return jsonResponse(http.StatusOK, rowBody), nil
		})
		c, _ := client(t, "Kunden/Aktive")

		row, err := c.GetRow(context.Background(), rowID)
		if err != nil {
			t.Fatalf("GetRow() = %v", err)
		}
		want := []string{rowRoute(salesBase, rowID) + "?convert_keys=true&table_name=Kunden"}
		if !reflect.DeepEqual(requests, want) {
			t.Errorf("requests = %v, want %v", requests, want)
		}
		if row.ID != rowID || len(row.Values) != 2 || row.UpdatedAt != "2026-04-02T11:15:00+01:00" {
			t.Errorf("row = %+v", row)
		}
	})

	t.Run("an identifier outside the documented form never becomes a request", func(t *testing.T) {
		refuse(t)
		c, _ := client(t, "Kunden")
		for _, id := range []string{"", "1", rowID + "x", "../" + rowID, strings.Repeat("/", rowIDLength)} {
			if _, err := c.GetRow(context.Background(), id); err == nil {
				t.Errorf("GetRow(%q) was accepted", id)
			}
		}
	})

	t.Run("another row than the requested one is refused", func(t *testing.T) {
		serveBase(t, func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"_id":"`+otherRowID+`","Name":"Test"}`), nil
		})
		c, _ := client(t, "Kunden")
		_, err := c.GetRow(context.Background(), rowID)
		if class := classOf(err); class != provider.ClassInvalidResponse {
			t.Fatalf("class = %q, want %q (%v)", class, provider.ClassInvalidResponse, err)
		}
	})
}

// The API token is exchanged once per client, the base it names is the only one a request reaches, and an
// exchange that points somewhere else is refused.
func TestBaseTokenIsExchangedOnceAndBoundToTheConfiguredServer(t *testing.T) {
	t.Run("one exchange serves every request of a client", func(t *testing.T) {
		var paths []string
		serveBase(t, func(request *http.Request) (*http.Response, error) {
			paths = append(paths, request.URL.Path)
			return jsonResponse(http.StatusOK, `{"rows":[]}`), nil
		})
		serve(t, func(request *http.Request) (*http.Response, error) {
			paths = append(paths, request.URL.Path)
			if response, handled := exchange(request); handled {
				return response, nil
			}
			return jsonResponse(http.StatusOK, `{"rows":[]}`), nil
		})

		c, red := client(t, "Kunden")
		for i := 0; i < 3; i++ {
			if _, err := c.ListRows(context.Background(), ListOptions{}); err != nil {
				t.Fatalf("ListRows() = %v", err)
			}
		}
		want := []string{baseTokenPath, rowsRoute(salesBase), rowsRoute(salesBase), rowsRoute(salesBase)}
		if !reflect.DeepEqual(paths, want) {
			t.Errorf("paths = %v, want one exchange and three reads", paths)
		}
		if red.Apply("token "+salesBaseToken) != "token "+redact.Marker {
			t.Error("the base token was not registered for redaction")
		}
	})

	t.Run("the exchange asks for a short-lived token", func(t *testing.T) {
		var query url.Values
		serve(t, func(request *http.Request) (*http.Response, error) {
			query = request.URL.Query()
			response, _ := exchange(request)
			return response, nil
		})
		c, _ := client(t, "Kunden")
		if _, err := c.access(context.Background(), "test"); err != nil {
			t.Fatalf("access() = %v", err)
		}
		if query.Get("exp") != baseTokenLifetime {
			t.Errorf("exp = %q, want %q", query.Get("exp"), baseTokenLifetime)
		}
	})

	t.Run("an exchange that names another server or base is refused", func(t *testing.T) {
		bodies := map[string]string{
			"a foreign server": `{"access_token":"` + salesBaseToken + `","dtable_uuid":"` + salesBase +
				`","dtable_server":"https://attacker.example.invalid/dtable-server/"}`,
			"a plain http server": `{"access_token":"` + salesBaseToken + `","dtable_uuid":"` + salesBase +
				`","dtable_server":"http://cloud.seatable.io/dtable-server/"}`,
			"no base":  `{"access_token":"` + salesBaseToken + `","dtable_uuid":"../../etc"}`,
			"no token": `{"access_token":"","dtable_uuid":"` + salesBase + `"}`,
		}
		for name, body := range bodies {
			t.Run(name, func(t *testing.T) {
				serve(t, func(request *http.Request) (*http.Response, error) {
					if request.URL.Path != baseTokenPath {
						t.Errorf("a base route was reached after a refused exchange: %s", request.URL.Path)
					}
					return jsonResponse(http.StatusOK, body), nil
				})
				c, _ := client(t, "Kunden")
				_, err := c.ListRows(context.Background(), ListOptions{})
				if class := classOf(err); class != provider.ClassInvalidResponse {
					t.Fatalf("class = %q, want %q (%v)", class, provider.ClassInvalidResponse, err)
				}
			})
		}
	})
}

// The connection test proves access and the fixed target against the base metadata, and reads only.
func TestTestConnectionValidatesTheTargetWithoutWriting(t *testing.T) {
	metadata := func(t *testing.T) *[]string {
		t.Helper()
		requests := new([]string)
		serve(t, func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodGet {
				t.Errorf("the connection test used %s, want a read", request.Method)
			}
			*requests = append(*requests, request.Method+" "+request.URL.Path)
			if response, handled := exchange(request); handled {
				return response, nil
			}
			return jsonResponse(http.StatusOK, metadataBody), nil
		})
		return requests
	}

	t.Run("a table and its view exist", func(t *testing.T) {
		requests := metadata(t)
		stubLimiter(t, salesToken)
		class, err := TestConnection(context.Background(),
			resolvedConnection("sales", "sales-reader", salesEnv, cloudOrigin, "Kunden/Aktive"),
			resolver(nil), nil)
		if err != nil || class != provider.ClassOK {
			t.Fatalf("TestConnection() = %q, %v", class, err)
		}
		want := []string{"GET " + baseTokenPath, "GET " + metaRoute(salesBase)}
		if !reflect.DeepEqual(*requests, want) {
			t.Errorf("requests = %v, want %v", *requests, want)
		}
	})

	t.Run("identifiers address the same target", func(t *testing.T) {
		metadata(t)
		stubLimiter(t, salesToken)
		class, err := TestConnection(context.Background(),
			resolvedConnection("sales", "sales-reader", salesEnv, cloudOrigin, "id:0000/id:7Yk3"),
			resolver(nil), nil)
		if err != nil || class != provider.ClassOK {
			t.Fatalf("TestConnection() = %q, %v", class, err)
		}
	})

	for _, tt := range []struct{ name, target, want string }{
		{"an absent table", "Absent", "table"},
		{"an absent view", "Kunden/Absent", "view"},
		{"an absent table identifier", "id:9999", "table"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metadata(t)
			stubLimiter(t, salesToken)
			class, err := TestConnection(context.Background(),
				resolvedConnection("sales", "sales-reader", salesEnv, cloudOrigin, tt.target),
				resolver(nil), nil)
			if err == nil || class != "" || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("TestConnection() = %q, %v, want the missing %s named", class, err, tt.want)
			}
		})
	}

	t.Run("an unusable API token is an auth failure", func(t *testing.T) {
		serve(t, func(request *http.Request) (*http.Response, error) {
			if strings.HasPrefix(request.URL.Path, gatewayPath) {
				t.Error("a base route was reached although the exchange failed")
			}
			return jsonResponse(http.StatusUnauthorized, `{"detail":"`+bodyCanary+`"}`), nil
		})
		stubLimiter(t, salesToken)
		class, err := TestConnection(context.Background(),
			resolvedConnection("sales", "sales-reader", salesEnv, cloudOrigin, "Kunden"), resolver(nil), nil)
		if err != nil || class != provider.ClassAuth {
			t.Fatalf("TestConnection() = %q, %v, want an auth failure", class, err)
		}
	})

	t.Run("an unusable target fails before any request", func(t *testing.T) {
		refuse(t)
		class, err := TestConnection(context.Background(),
			resolvedConnection("sales", "sales-reader", salesEnv, cloudOrigin, ""), resolver(nil), nil)
		if err != nil || class != provider.ClassProviderError {
			t.Fatalf("TestConnection() = %q, %v", class, err)
		}
	})
}

// Every provider status becomes a stable class, and no provider body reaches the message.
func TestProviderStatusesAreClassified(t *testing.T) {
	tests := []struct {
		status int
		want   provider.Class
	}{
		{http.StatusUnauthorized, provider.ClassAuth},
		{http.StatusForbidden, provider.ClassPermission},
		{http.StatusNotFound, provider.ClassProviderError},
		{http.StatusBadRequest, provider.ClassProviderError},
		{http.StatusTooManyRequests, provider.ClassRateLimited},
		{http.StatusGatewayTimeout, provider.ClassTimeout},
		{http.StatusInternalServerError, provider.ClassProviderError},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			serveBase(t, func(*http.Request) (*http.Response, error) {
				return jsonResponse(tt.status, `{"error_message":"`+bodyCanary+`"}`), nil
			})
			c, _ := client(t, "Kunden")
			_, err := c.ListRows(context.Background(), ListOptions{})
			if class := classOf(err); class != tt.want {
				t.Fatalf("class = %q, want %q (%v)", class, tt.want, err)
			}
			if strings.Contains(err.Error(), bodyCanary) {
				t.Errorf("the error carries the provider body: %v", err)
			}
		})
	}

	t.Run("an oversized answer is refused", func(t *testing.T) {
		serveBase(t, func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"rows":[`+strings.Repeat(`{"_id":"x"},`, 200000)+`{}]}`), nil
		})
		c, _ := client(t, "Kunden")
		_, err := c.ListRows(context.Background(), ListOptions{})
		if class := classOf(err); class != provider.ClassInvalidResponse {
			t.Fatalf("class = %q, want %q (%v)", class, provider.ClassInvalidResponse, err)
		}
	})
}

// Requests that share an API token are spaced by the lower of the documented base limits.
func TestRateLimitSpacesRequestsOfOneToken(t *testing.T) {
	serveBase(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"rows":[]}`), nil
	})

	var (
		mu     sync.Mutex
		now    = time.Unix(0, 0)
		waited []time.Duration
	)
	limited := ratelimit.New(minInterval,
		func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		func(_ context.Context, d time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			waited = append(waited, d)
			now = now.Add(d)
			return nil
		})

	red := &redact.Redactor{}
	c, err := open(resolvedConnection("sales", "sales-reader", salesEnv, cloudOrigin, "Kunden"),
		resolver(red), red, limited)
	if err != nil {
		t.Fatalf("open() = %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.ListRows(context.Background(), ListOptions{}); err != nil {
			t.Fatalf("ListRows() = %v", err)
		}
	}

	// One exchange and two reads are three requests, so two of them wait.
	if !reflect.DeepEqual(waited, []time.Duration{minInterval, minInterval}) {
		t.Errorf("waits = %v, want two waits of %v", waited, minInterval)
	}
	if minInterval*200 < time.Minute {
		t.Errorf("interval %v allows more than 200 base requests per minute", minInterval)
	}
}

// A spent budget reported by the API gateway delays the next request instead of running into a refusal.
func TestRateLimitHeadersHoldTheNextRequest(t *testing.T) {
	reset := time.Now().Add(5 * time.Second).Unix()
	serve(t, func(request *http.Request) (*http.Response, error) {
		response, handled := exchange(request)
		if !handled {
			response = jsonResponse(http.StatusOK, `{"rows":[]}`)
		}
		response.Header.Set(headerRemaining, "0")
		response.Header.Set(headerReset, strconv.FormatInt(reset, 10))
		return response, nil
	})

	var (
		mu     sync.Mutex
		now    = time.Unix(0, 0)
		waited []time.Duration
	)
	limited := ratelimit.New(0,
		func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		func(_ context.Context, d time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			waited = append(waited, d)
			now = now.Add(d)
			return nil
		})

	red := &redact.Redactor{}
	c, err := open(resolvedConnection("sales", "sales-reader", salesEnv, cloudOrigin, "Kunden"),
		resolver(red), red, limited)
	if err != nil {
		t.Fatalf("open() = %v", err)
	}
	if _, err := c.ListRows(context.Background(), ListOptions{}); err != nil {
		t.Fatalf("ListRows() = %v", err)
	}
	if len(waited) == 0 || waited[0] < 4*time.Second || waited[0] > 5*time.Second {
		t.Fatalf("waits = %v, want the reported reset to be honoured", waited)
	}

	// A reset far in the future is bounded, and a header without a spent budget changes nothing.
	c.limiter.HoldFor(2 * maxHold)
	c.observeRateLimit(http.Header{headerRemaining: {"17"}, headerReset: {"0"}})
	if _, err := c.ListRows(context.Background(), ListOptions{}); err != nil {
		t.Fatalf("ListRows() = %v", err)
	}
	if last := waited[len(waited)-1]; last > 2*maxHold {
		t.Errorf("wait = %v, want a bounded hold", last)
	}
}

// Two instances, two bases of one instance, and two tokens of one base stay separated: every request uses
// the origin, the token, and the table of its own connection, and every token keeps its own budget.
func TestInstancesBasesAndTokensStaySeparated(t *testing.T) {
	type call struct{ host, path, authorization, table string }
	var calls []call
	serveBase(t, func(request *http.Request) (*http.Response, error) {
		calls = append(calls, call{
			request.URL.Host, request.URL.Path, request.Header.Get("Authorization"),
			request.URL.Query().Get("table_name") + request.URL.Query().Get("table_id"),
		})
		return jsonResponse(http.StatusOK, `{"rows":[]}`), nil
	})

	red := &redact.Redactor{}
	secrets := resolver(red)
	for _, connection := range []*config.Resolved{
		resolvedConnection("sales", "sales-reader", salesEnv, cloudOrigin, "Kunden"),
		resolvedConnection("sales-audit", "sales-auditor", auditEnv, cloudOrigin, "Kunden/Aktive"),
		resolvedConnection("support", "support-reader", supportEnv, cloudOrigin, "id:0001"),
		resolvedConnection("onprem", "onprem-reader", onpremEnv, selfHosted, "Tickets"),
	} {
		c, err := open(connection, secrets, red, freeLimiter())
		if err != nil {
			t.Fatalf("open(%s) = %v", connection.Name, err)
		}
		if _, err := c.ListRows(context.Background(), ListOptions{}); err != nil {
			t.Fatalf("ListRows(%s) = %v", connection.Name, err)
		}
	}

	want := []call{
		{"cloud.seatable.io", rowsRoute(salesBase), "Bearer " + salesBaseToken, "Kunden"},
		{"cloud.seatable.io", rowsRoute(salesBase), "Bearer " + auditBaseToken, "Kunden"},
		{"cloud.seatable.io", rowsRoute(supportBase), "Bearer " + supportBaseToken, "0001"},
		{"seatable.example.invalid", rowsRoute(onpremBase), "Bearer " + onpremBaseToken, "Tickets"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %+v, want %+v", calls, want)
	}
	for _, pair := range [][2]string{{salesToken, auditToken}, {salesToken, supportToken}, {salesToken, onpremToken}} {
		if limiters.For(pair[0]) == limiters.For(pair[1]) {
			t.Errorf("the tokens %q and %q share one rate-limit budget", pair[0], pair[1])
		}
	}
	if limiters.For(salesToken) != limiters.For(salesToken) {
		t.Error("one API token does not keep one rate-limit budget")
	}
}

// Both operations satisfy their registered contract end to end through the application core: the core
// validates the arguments, selects the explicit connection, resolves the token of that base, and
// validates the normalised result against the descriptor.
func TestOperationsSatisfyTheirContractThroughTheApplicationCore(t *testing.T) {
	serveBase(t, func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, rowID+"/") {
			return jsonResponse(http.StatusOK, rowBody), nil
		}
		return jsonResponse(http.StatusOK, rowsBody), nil
	})
	for token := range issued {
		stubLimiter(t, token)
	}

	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)

	list, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "seatable.rows.list", Connection: "sales", Arguments: json.RawMessage(`{"start":0,"limit":2}`),
	})
	if err != nil {
		t.Fatalf("invoke list = %v", err)
	}
	var listed struct {
		Rows []struct {
			ID        string         `json:"id"`
			CreatedAt string         `json:"created_at"`
			Values    map[string]any `json:"values"`
		} `json:"rows"`
		Start     int  `json:"start"`
		Limit     int  `json:"limit"`
		NextStart int  `json:"next_start"`
		HasMore   bool `json:"has_more"`
	}
	if err := json.Unmarshal(list.Result, &listed); err != nil {
		t.Fatalf("list result = %s: %v", list.Result, err)
	}
	if len(listed.Rows) != 2 || listed.Rows[0].ID != rowID || !listed.HasMore || listed.NextStart != 2 ||
		listed.Rows[0].Values["Name"] != "Bike & Ride GmbH" {
		t.Errorf("list result = %s", list.Result)
	}
	if strings.Contains(string(list.Result), "_creator") {
		t.Errorf("the core answered with a system key: %s", list.Result)
	}

	got, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "seatable.rows.get", Connection: "sales-audit",
		Arguments: json.RawMessage(`{"row_id":"` + rowID + `"}`),
	})
	if err != nil {
		t.Fatalf("invoke get = %v", err)
	}
	if !strings.Contains(string(got.Result), `"id":"`+rowID+`"`) ||
		!strings.Contains(string(got.Result), `"updated_at":"2026-04-02T11:15:00+01:00"`) {
		t.Errorf("get result = %s", got.Result)
	}
}

// The core refuses what the contract does not allow before a provider is contacted, and it never guesses
// a base when several connections could serve the request.
func TestTheCoreRefusesUnsupportedRequestsBeforeProviderIO(t *testing.T) {
	refuse(t)
	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)

	tests := []struct {
		name    string
		request application.InvokeRequest
	}{
		{"several bases without an explicit connection", application.InvokeRequest{
			Operation: "seatable.rows.list", Arguments: json.RawMessage(`{}`)}},
		{"a table as an argument", application.InvokeRequest{
			Operation: "seatable.rows.list", Connection: "sales",
			Arguments: json.RawMessage(`{"table_name":"Gehaelter"}`)}},
		{"a base as an argument", application.InvokeRequest{
			Operation: "seatable.rows.list", Connection: "sales",
			Arguments: json.RawMessage(`{"base_uuid":"` + supportBase + `"}`)}},
		{"a view as an argument", application.InvokeRequest{
			Operation: "seatable.rows.list", Connection: "sales",
			Arguments: json.RawMessage(`{"view_name":"Alle"}`)}},
		{"a base token as an argument", application.InvokeRequest{
			Operation: "seatable.rows.list", Connection: "sales",
			Arguments: json.RawMessage(`{"access_token":"` + salesBaseToken + `"}`)}},
		{"a page size above the bound", application.InvokeRequest{
			Operation: "seatable.rows.list", Connection: "sales",
			Arguments: json.RawMessage(`{"limit":1000}`)}},
		{"an offset above the bound", application.InvokeRequest{
			Operation: "seatable.rows.list", Connection: "sales",
			Arguments: json.RawMessage(`{"start":10001}`)}},
		{"a negative offset", application.InvokeRequest{
			Operation: "seatable.rows.list", Connection: "sales",
			Arguments: json.RawMessage(`{"start":-1}`)}},
		{"an identifier outside the documented form", application.InvokeRequest{
			Operation: "seatable.rows.get", Connection: "sales",
			Arguments: json.RawMessage(`{"row_id":"../../metadata/"}`)}},
		{"a missing identifier", application.InvokeRequest{
			Operation: "seatable.rows.get", Connection: "sales", Arguments: json.RawMessage(`{}`)}},
		{"a connection of another provider", application.InvokeRequest{
			Operation: "seatable.rows.list", Connection: "wiki", Arguments: json.RawMessage(`{}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := core.Invoke(context.Background(), tt.request); err == nil {
				t.Fatal("the core accepted the request")
			}
		})
	}
}

// No token and no provider content reaches a result, a diagnostic, or an error, whatever the base answers.
func TestTokensAndCellValuesNeverReachTheOutput(t *testing.T) {
	serveBase(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"rows":[{"_id":"`+rowID+`",`+
			`"Name":"`+salesToken+`","Notiz":"Bearer `+salesBaseToken+`"}]}`), nil
	})
	stubLimiter(t, salesToken)

	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)
	response, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "seatable.rows.list", Connection: "sales", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("invoke = %v", err)
	}
	for _, canary := range []string{salesToken, salesBaseToken} {
		if strings.Contains(string(response.Result), canary) {
			t.Errorf("the result carries a token: %s", response.Result)
		}
	}
	if !strings.Contains(string(response.Result), redact.Marker) {
		t.Errorf("the result was not redacted: %s", response.Result)
	}
	var value any
	if err := json.Unmarshal(response.Result, &value); err != nil {
		t.Errorf("the redacted result is not valid JSON: %v", err)
	}

	// A failing read reports a class, never the cell content or the body the base sent.
	serveBase(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest,
			`{"error_message":"`+cellCanary+` `+bodyCanary+`"}`), nil
	})
	_, err = core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "seatable.rows.list", Connection: "sales", Arguments: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("the failing read was reported as a result")
	}
	for _, canary := range []string{cellCanary, bodyCanary, salesToken, salesBaseToken} {
		if strings.Contains(red.Error(err), canary) {
			t.Errorf("the error carries a canary: %v", err)
		}
	}
}

// registry returns a registry with SeaTable and one foreign provider, so a wrong route is provable.
func registry(t *testing.T) *capability.Registry {
	t.Helper()
	reg := capability.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	if err := reg.RegisterProvider(config.ProviderMetadata{ID: "wiki", Name: "Wiki"}, nil); err != nil {
		t.Fatalf("RegisterProvider() = %v", err)
	}
	return reg
}

// coreConfig configures two SeaTable instances, two bases of the cloud instance, and two API tokens for
// the same base, plus one foreign connection.
func coreConfig() *config.Config {
	credential := func(env string) config.Credential {
		return config.Credential{
			Type: config.CredentialTypeEnv, Values: map[string]string{roleAPIToken: env},
		}
	}
	return &config.Config{
		Version: 1,
		Services: map[string]config.Service{
			"sea-cloud":  {Provider: Provider, BaseURL: cloudOrigin},
			"sea-onprem": {Provider: Provider, BaseURL: selfHosted},
			"wiki":       {Provider: "wiki", BaseURL: "https://wiki.example.invalid"},
		},
		Credentials: map[string]config.Credential{
			"sales-reader":   credential(salesEnv),
			"sales-auditor":  credential(auditEnv),
			"support-reader": credential(supportEnv),
			"onprem-reader":  credential(onpremEnv),
			"wiki-reader":    {Type: config.CredentialTypeEnv, Values: map[string]string{"token-id": "WIKI_ID"}},
		},
		Connections: map[string]config.Connection{
			"sales":       {Service: "sea-cloud", Credential: "sales-reader", Target: "Kunden"},
			"sales-audit": {Service: "sea-cloud", Credential: "sales-auditor", Target: "Kunden/Aktive"},
			"support":     {Service: "sea-cloud", Credential: "support-reader", Target: "id:0001"},
			"onprem":      {Service: "sea-onprem", Credential: "onprem-reader", Target: "Tickets"},
			"wiki":        {Service: "wiki", Credential: "wiki-reader"},
		},
	}
}

func classOf(err error) provider.Class {
	var providerErr *provider.Error
	if errors.As(err, &providerErr) {
		return providerErr.Class
	}
	return ""
}
