package twentycrm

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// The canaries stand for an API key, for a workspace-specific custom field, and for provider content. No
// test reaches a productive workspace: every request is answered by the package's own transport seam.
const (
	cloudKey     = "canary-twenty-cloud-key-7be31d"
	internalKey  = "canary-twenty-internal-key-4c0a92"
	cloudEnv     = "TEST_TWENTY_CLOUD_KEY"
	internalEnv  = "TEST_TWENTY_INTERNAL_KEY"
	companyID    = "8f14e45f-ceea-467a-9e60-3b4b5e0f0e21"
	otherID      = "2b6f0cc9-04bb-4bd8-8e19-6ff4c7a2f0d3"
	bodyCanary   = "provider-body-canary-twenty-9d4f"
	customCanary = "custom-field-canary-twenty-6a71"
	selfHosted   = "https://crm.example.invalid"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// serve replaces the package transport for one test. Production keeps Go's default transport.
func serve(t *testing.T, handler func(*http.Request) (*http.Response, error)) {
	t.Helper()
	previous := transport
	transport = roundTripFunc(handler)
	t.Cleanup(func() { transport = previous })
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

func resolvedConnection(name, credential, env, origin string) *config.Resolved {
	return &config.Resolved{
		Name: name, Provider: Provider, BaseURL: origin, Service: "twenty", Credential: credential,
		Secrets: config.Credential{
			Type: config.CredentialTypeEnv, Values: map[string]string{roleAPIKey: env},
		},
	}
}

func resolver(red *redact.Redactor) *secret.Resolver {
	return secret.NewWith(func(name string) string {
		switch name {
		case cloudEnv:
			return cloudKey
		case internalEnv:
			return internalKey
		}
		return ""
	}, nil, nil, red)
}

// freeLimiter is a limiter that never waits. The rate limit itself is proven by its own test.
func freeLimiter() *limiter {
	return newLimiter(0, time.Now, sleepFor)
}

// client opens the cloud connection with the package transport currently installed.
func client(t *testing.T) (*Client, *redact.Redactor) {
	t.Helper()
	red := &redact.Redactor{}
	c, err := open(resolvedConnection("crm", "crm-cloud-reader", cloudEnv, cloudOrigin), resolver(red), red,
		freeLimiter())
	if err != nil {
		t.Fatalf("open() = %v", err)
	}
	return c, red
}

// listBody carries a custom field and a company whose link label is empty, so the stable projection can be
// proven to drop the one and to fall back for the other.
const listBody = `{
  "data": {"companies": [
    {"id":"8f14e45f-ceea-467a-9e60-3b4b5e0f0e21","name":"Bike & Ride GmbH",
     "domainName":{"primaryLinkLabel":"bike-ride.example","primaryLinkUrl":"https://bike-ride.example",
      "secondaryLinks":[{"label":"shop","url":"https://shop.bike-ride.example"}]},
     "createdAt":"2026-05-14T16:52:21.000Z","updatedAt":"2026-06-02T08:11:03.000Z",
     "deletedAt":null,"position":1,"annualRevenue":{"amountMicros":1000,"currencyCode":"EUR"},
     "riskScore":"` + customCanary + `"},
    {"id":"2b6f0cc9-04bb-4bd8-8e19-6ff4c7a2f0d3","name":"Test GmbH",
     "domainName":{"primaryLinkLabel":"","primaryLinkUrl":"https://test.example","secondaryLinks":null},
     "createdAt":"2026-01-09T10:00:00.000Z","updatedAt":"2026-01-09T10:00:00.000Z"}
  ]},
  "totalCount": 57,
  "pageInfo": {"hasNextPage": true, "hasPreviousPage": false, "startCursor": "c0", "endCursor": "cursor-page-2"}
}`

const companyBody = `{
  "data": {"company": {
    "id":"8f14e45f-ceea-467a-9e60-3b4b5e0f0e21","name":"Bike & Ride GmbH",
    "domainName":{"primaryLinkLabel":"bike-ride.example","primaryLinkUrl":"https://bike-ride.example",
     "secondaryLinks":[]},
    "createdAt":"2026-05-14T16:52:21.000Z","updatedAt":"2026-06-02T08:11:03.000Z","deletedAt":null,
    "riskScore":"` + customCanary + `"
  }}
}`

// schemaBody is the workspace document of a compatible workspace: both company routes and, across the
// generated company schemas, every field the stable projection reads.
const schemaBody = `{
  "openapi":"3.1.0",
  "paths":{"/companies":{"get":{}},"/companies/{id}":{"get":{}},"/people":{"get":{}}},
  "components":{"schemas":{
    "Company":{"type":"object","properties":{"name":{},"domainName":{},"riskScore":{}}},
    "CompanyForUpdate":{"type":"object","properties":{"name":{},"domainName":{}}},
    "CompanyForResponse":{"type":"object","properties":{
      "id":{},"name":{},"domainName":{},"createdAt":{},"updatedAt":{},"deletedAt":{},"riskScore":{}}},
    "Person":{"type":"object","properties":{"id":{},"name":{}}}
  }}
}`

// Register publishes the configuration metadata the TUI needs and exactly two read-only operations.
func TestRegisterPublishesMetadataAndTwoReadOnlyOperations(t *testing.T) {
	reg := capability.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	metadata, ok := reg.ProviderMetadata(Provider)
	if !ok || metadata.Name != "Twenty CRM" || metadata.DefaultBaseURL != cloudOrigin ||
		metadata.Target.Required || len(metadata.SecretRoles) != 1 ||
		metadata.SecretRoles[0].Name != roleAPIKey || metadata.SecretRoles[0].Description == "" {
		t.Fatalf("metadata = %+v, %v", metadata, ok)
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
		if len(descriptor.Examples) == 0 || strings.Contains(string(descriptor.Examples[0].Arguments), "key") {
			t.Errorf("descriptor %s examples = %s", descriptor.ID, descriptor.Examples)
		}
	}
	if operations[0].ID != "twentycrm.companies.get" || operations[1].ID != "twentycrm.companies.list" {
		t.Errorf("operation IDs = %s, %s", operations[0].ID, operations[1].ID)
	}
}

// The list request carries exactly the allowed search, sort, and paging parameters and nothing else. The
// origin comes from the service, never from an argument.
func TestListCompaniesSendsOnlyTheAllowedParameters(t *testing.T) {
	var got url.Values
	calls := 0
	serve(t, func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.Scheme != "https" ||
			request.URL.Host != "api.twenty.com" || request.URL.Path != "/rest/companies" {
			t.Errorf("request = %s %s", request.Method, request.URL.Redacted())
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer "+cloudKey {
			t.Errorf("Authorization header = %q, want the bearer API key", authorization)
		}
		got = request.URL.Query()
		return jsonResponse(http.StatusOK, listBody), nil
	})

	c, _ := client(t)
	if _, err := c.ListCompanies(context.Background(), ListOptions{
		NameContains: "Bike", DomainContains: "bike-ride.example", Limit: 50,
		Sort: "name", Direction: "asc", Cursor: "cursor-page-2",
	}); err != nil {
		t.Fatalf("ListCompanies() = %v", err)
	}

	if calls != 1 {
		t.Fatalf("requests = %d, want exactly one", calls)
	}
	want := url.Values{
		"limit": {"50"}, "depth": {"0"}, "order_by": {"name[AscNullsFirst]"},
		"filter":         {`name[ilike]:"%Bike%",domainName.primaryLinkUrl[ilike]:"%bike-ride.example%"`},
		"starting_after": {"cursor-page-2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("query = %v, want %v", got, want)
	}
}

// Omitted arguments produce the deterministic default page, without a filter and without a cursor.
func TestListCompaniesUsesDeterministicDefaults(t *testing.T) {
	var got url.Values
	serve(t, func(request *http.Request) (*http.Response, error) {
		got = request.URL.Query()
		return jsonResponse(http.StatusOK, listBody), nil
	})

	c, _ := client(t)
	if _, err := c.ListCompanies(context.Background(), ListOptions{}); err != nil {
		t.Fatalf("ListCompanies() = %v", err)
	}
	want := url.Values{"limit": {"25"}, "depth": {"0"}, "order_by": {"createdAt[DescNullsLast]"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("query = %v, want %v", got, want)
	}
}

// The result is the bounded stable projection: identifier, name, and primary domain. A workspace-specific
// custom field is not adopted, and the record timestamps stay out of the list contract.
func TestListCompaniesReportsOnlyTheStableProjection(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, listBody), nil
	})

	c, _ := client(t)
	result, err := c.ListCompanies(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("ListCompanies() = %v", err)
	}

	want := &ListResult{
		Companies: []Company{
			{ID: companyID, Name: "Bike & Ride GmbH", Domain: "bike-ride.example"},
			// An empty link label falls back to the link URL rather than reporting no domain.
			{ID: otherID, Name: "Test GmbH", Domain: "https://test.example"},
		},
		NextCursor: "cursor-page-2", HasMore: true,
	}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("result = %#v, want %#v", result, want)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	for _, forbidden := range []string{customCanary, "riskScore", "created_at", "annualRevenue", "secondaryLinks"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the list projection carries %q: %s", forbidden, encoded)
		}
	}
}

// Cursor pagination stays deterministic: the cursor of a page reaches the provider, and the last page
// reports no follow-up cursor.
func TestListCompaniesPaginatesByCursor(t *testing.T) {
	pages := map[string]string{
		"": `{"data":{"companies":[]},"totalCount":2,` +
			`"pageInfo":{"hasNextPage":true,"endCursor":"cursor-page-2"}}`,
		"cursor-page-2": `{"data":{"companies":[]},"totalCount":2,` +
			`"pageInfo":{"hasNextPage":false,"endCursor":"cursor-page-3"}}`,
	}
	requested := make([]string, 0, 2)
	serve(t, func(request *http.Request) (*http.Response, error) {
		cursor := request.URL.Query().Get("starting_after")
		requested = append(requested, cursor)
		return jsonResponse(http.StatusOK, pages[cursor]), nil
	})

	c, _ := client(t)
	first, err := c.ListCompanies(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("ListCompanies() = %v", err)
	}
	if !first.HasMore || first.NextCursor != "cursor-page-2" {
		t.Fatalf("first page = %#v", first)
	}

	second, err := c.ListCompanies(context.Background(), ListOptions{Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("ListCompanies(cursor) = %v", err)
	}
	// The last page hands out no cursor, so a caller cannot loop past the end of the result.
	if second.HasMore || second.NextCursor != "" {
		t.Errorf("second page = %#v", second)
	}
	if !reflect.DeepEqual(requested, []string{"", "cursor-page-2"}) {
		t.Errorf("requested cursors = %v", requested)
	}
}

// Bounds, formats, and anything that could leave a filter value are refused before any provider I/O.
func TestListCompaniesRejectsUnusableOptionsBeforeIO(t *testing.T) {
	refuse(t)
	c, _ := client(t)

	tests := map[string]ListOptions{
		"negative page size":           {Limit: -1},
		"page size above the bound":    {Limit: maxPageSize + 1},
		"unknown sort property":        {Sort: "annual_revenue"},
		"unknown sort direction":       {Direction: "sideways"},
		"oversized name filter":        {NameContains: strings.Repeat("a", searchNameMax+1)},
		"quote in the name filter":     {NameContains: `Acme" or name[ilike]:"%`},
		"comma in the name filter":     {NameContains: "Acme,Other"},
		"bracket in the name filter":   {NameContains: "Acme[ilike]"},
		"domain filter with a slash":   {DomainContains: "acme.example/path"},
		"domain filter with a quote":   {DomainContains: `acme"`},
		"oversized domain filter":      {DomainContains: strings.Repeat("a", searchDomainMax+1)},
		"cursor with a separator":      {Cursor: "cursor&limit=200"},
		"cursor beyond the length cap": {Cursor: strings.Repeat("c", maxCursorLength+1)},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := c.ListCompanies(context.Background(), options); err == nil {
				t.Fatal("ListCompanies() accepted the options")
			}
		})
	}
}

// A list record without a usable identifier is a contract violation, not a result.
func TestListCompaniesRejectsAnUnusableIdentifier(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"companies":[{"id":"not-a-uuid","name":"Acme"}]}}`), nil
	})

	c, _ := client(t)
	_, err := c.ListCompanies(context.Background(), ListOptions{})
	if class := classOf(err); class != provider.ClassInvalidResponse {
		t.Fatalf("class = %q, want %q (%v)", class, provider.ClassInvalidResponse, err)
	}
}

// The get operation reads exactly the requested company and produces no other provider I/O.
func TestGetCompanyReadsExactlyTheRequestedCompany(t *testing.T) {
	requests := make([]string, 0, 1)
	serve(t, func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.Method+" "+request.URL.Path+"?"+request.URL.RawQuery)
		return jsonResponse(http.StatusOK, companyBody), nil
	})

	c, _ := client(t)
	company, err := c.GetCompany(context.Background(), companyID)
	if err != nil {
		t.Fatalf("GetCompany() = %v", err)
	}
	if !reflect.DeepEqual(requests, []string{"GET /rest/companies/" + companyID + "?depth=0"}) {
		t.Fatalf("requests = %v, want exactly one read of the company", requests)
	}

	want := &Company{
		ID: companyID, Name: "Bike & Ride GmbH", Domain: "bike-ride.example",
		CreatedAt: "2026-05-14T16:52:21.000Z", UpdatedAt: "2026-06-02T08:11:03.000Z",
	}
	if !reflect.DeepEqual(company, want) {
		t.Errorf("company = %#v, want %#v", company, want)
	}
	encoded, _ := json.Marshal(company)
	if strings.Contains(string(encoded), customCanary) {
		t.Errorf("the company projection carries a custom field: %s", encoded)
	}
}

// Only a validated UUID reaches the provider, and an answer about another company is refused.
func TestGetCompanyGuardsTheIdentifier(t *testing.T) {
	t.Run("a malformed identifier stops before the provider", func(t *testing.T) {
		refuse(t)
		c, _ := client(t)
		for _, id := range []string{"", "42", "../people", companyID + "0", "zz" + companyID[2:]} {
			if _, err := c.GetCompany(context.Background(), id); err == nil {
				t.Errorf("GetCompany(%q) was accepted", id)
			}
		}
	})

	t.Run("another company is an invalid response", func(t *testing.T) {
		serve(t, func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"data":{"company":{"id":"`+otherID+`","name":"Test GmbH"}}}`), nil
		})
		c, _ := client(t)
		_, err := c.GetCompany(context.Background(), companyID)
		if class := classOf(err); class != provider.ClassInvalidResponse {
			t.Fatalf("class = %q, want %q (%v)", class, provider.ClassInvalidResponse, err)
		}
	})
}

// Every provider status becomes a stable class, and no provider body reaches the message.
func TestProviderStatusesAreNormalized(t *testing.T) {
	tests := []struct {
		status int
		want   provider.Class
	}{
		{http.StatusUnauthorized, provider.ClassAuth},
		{http.StatusForbidden, provider.ClassPermission},
		{http.StatusNotFound, provider.ClassProviderError},
		{http.StatusBadRequest, provider.ClassProviderError},
		{http.StatusUnprocessableEntity, provider.ClassProviderError},
		{http.StatusTooManyRequests, provider.ClassRateLimited},
		{http.StatusGatewayTimeout, provider.ClassTimeout},
		{http.StatusInternalServerError, provider.ClassProviderError},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			serve(t, func(*http.Request) (*http.Response, error) {
				return jsonResponse(tt.status, `{"statusCode":`+strconv.Itoa(tt.status)+
					`,"error":"`+bodyCanary+`","messages":["`+bodyCanary+`"]}`), nil
			})
			c, _ := client(t)
			_, err := c.ListCompanies(context.Background(), ListOptions{})
			if class := classOf(err); class != tt.want {
				t.Errorf("class = %q, want %q (%v)", class, tt.want, err)
			}
			if strings.Contains(err.Error(), bodyCanary) {
				t.Errorf("error carries the provider body: %v", err)
			}
		})
	}
}

// A failure before a status code exists is classified without copying the transport message.
func TestTransportFailuresAreNormalized(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want provider.Class
	}{
		{"timeout", &net.DNSError{IsTimeout: true, Err: bodyCanary}, provider.ClassTimeout},
		{"tls", &tls.CertificateVerificationError{Err: errors.New(bodyCanary)}, provider.ClassTLS},
		{"unreachable", errors.New(bodyCanary), provider.ClassUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serve(t, func(*http.Request) (*http.Response, error) { return nil, tt.err })
			c, _ := client(t)
			_, err := c.ListCompanies(context.Background(), ListOptions{})
			if class := classOf(err); class != tt.want {
				t.Errorf("class = %q, want %q (%v)", class, tt.want, err)
			}
			if strings.Contains(err.Error(), bodyCanary) {
				t.Errorf("error carries the transport message: %v", err)
			}
		})
	}
}

// An answer beyond the size limit is refused instead of being read into memory.
func TestOversizedResponsesAreRefused(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, strings.Repeat("x", maxResponseBytes+1)), nil
	})
	c, _ := client(t)
	_, err := c.ListCompanies(context.Background(), ListOptions{})
	if class := classOf(err); class != provider.ClassInvalidResponse {
		t.Fatalf("class = %q, want %q (%v)", class, provider.ClassInvalidResponse, err)
	}
}

// Opening a connection registers the API key and the derived bearer value with the redactor, accepts the
// cloud origin and a self-hosted https origin, and refuses everything that is not a bare https origin.
func TestOpenRedactsTheKeyAndBindsTheConfiguredOrigin(t *testing.T) {
	red := &redact.Redactor{}
	if _, err := open(resolvedConnection("crm", "crm-cloud-reader", cloudEnv, cloudOrigin),
		resolver(red), red, freeLimiter()); err != nil {
		t.Fatalf("open() = %v", err)
	}
	for _, value := range []string{cloudKey, "Bearer " + cloudKey} {
		if got := red.Apply("diagnostic " + value); strings.Contains(got, cloudKey) {
			t.Errorf("redactor keeps %q: %q", value, got)
		}
	}

	for _, origin := range []string{cloudOrigin, cloudOrigin + "/", selfHosted, selfHosted + "/"} {
		if _, err := open(resolvedConnection("crm", "crm-cloud-reader", cloudEnv, origin),
			resolver(red), red, freeLimiter()); err != nil {
			t.Errorf("open() refused the configured origin %q: %v", origin, err)
		}
	}

	for _, origin := range []string{
		"http://api.twenty.com",                  // plain text would carry the key in the clear
		"https://user:pass@api.twenty.com",       // userinfo is a second credential
		"https://api.twenty.com/rest",            // a path would silently rewrite every route
		"https://api.twenty.com?token=x",         // a query would ride along on every request
		"https://api.twenty.com#fragment",        //
		"https:api.twenty.com",                   // opaque, without a host
		"//api.twenty.com", "api.twenty.com", "", //
	} {
		connection := resolvedConnection("crm", "crm-cloud-reader", cloudEnv, origin)
		if _, err := open(connection, resolver(red), red, freeLimiter()); err == nil {
			t.Errorf("open() accepted the origin %q", origin)
		}
	}

	missing := resolvedConnection("crm", "crm-cloud-reader", "TEST_TWENTY_ABSENT", cloudOrigin)
	if _, err := open(missing, resolver(red), red, freeLimiter()); err == nil {
		t.Error("open() accepted a credential without a secret")
	}
}

// Two workspaces are two connections with two keys and two origins. Each request uses the key and the
// origin of its own connection, and the keys get separate rate-limit budgets.
func TestWorkspacesKeepTheirOwnOriginKeyAndRateBudget(t *testing.T) {
	type call struct{ host, authorization string }
	calls := make([]call, 0, 2)
	serve(t, func(request *http.Request) (*http.Response, error) {
		calls = append(calls, call{request.URL.Host, request.Header.Get("Authorization")})
		return jsonResponse(http.StatusOK, `{"data":{"companies":[]},"pageInfo":{"hasNextPage":false}}`), nil
	})

	red := &redact.Redactor{}
	secrets := resolver(red)
	for _, connection := range []*config.Resolved{
		resolvedConnection("crm", "crm-cloud-reader", cloudEnv, cloudOrigin),
		resolvedConnection("crm-internal", "crm-selfhosted-reader", internalEnv, selfHosted),
	} {
		c, err := open(connection, secrets, red, freeLimiter())
		if err != nil {
			t.Fatalf("open(%s) = %v", connection.Name, err)
		}
		if _, err := c.ListCompanies(context.Background(), ListOptions{}); err != nil {
			t.Fatalf("ListCompanies(%s) = %v", connection.Name, err)
		}
	}

	want := []call{
		{"api.twenty.com", "Bearer " + cloudKey},
		{"crm.example.invalid", "Bearer " + internalKey},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want each connection to use its own origin and key", calls)
	}
	if limiterFor(cloudKey) == limiterFor(internalKey) {
		t.Error("two API keys share one rate-limit budget")
	}
	if limiterFor(cloudKey) != limiterFor(cloudKey) {
		t.Error("one API key does not keep one rate-limit budget")
	}
}

// Requests that share a key are spaced by the documented limit of 100 requests per minute.
func TestRateLimitSpacesRequestsOfOneKey(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"companies":[]},"pageInfo":{"hasNextPage":false}}`), nil
	})

	var (
		mu     sync.Mutex
		now    = time.Unix(0, 0)
		waited []time.Duration
	)
	limited := newLimiter(minInterval,
		func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		func(_ context.Context, d time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			waited = append(waited, d)
			now = now.Add(d)
			return nil
		})

	red := &redact.Redactor{}
	c, err := open(resolvedConnection("crm", "crm-cloud-reader", cloudEnv, cloudOrigin), resolver(red), red, limited)
	if err != nil {
		t.Fatalf("open() = %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.ListCompanies(context.Background(), ListOptions{}); err != nil {
			t.Fatalf("ListCompanies() = %v", err)
		}
	}

	if !reflect.DeepEqual(waited, []time.Duration{minInterval, minInterval}) {
		t.Errorf("waits = %v, want two waits of %v", waited, minInterval)
	}
	if minInterval*100 < time.Minute {
		t.Errorf("interval %v allows more than 100 requests per minute", minInterval)
	}
}

// A cancelled request never becomes a provider call while it waits for the rate limit.
func TestRateLimitRespectsCancellation(t *testing.T) {
	refuse(t)
	limited := newLimiter(minInterval, time.Now, sleepFor)
	limited.next = time.Now().Add(time.Hour)

	red := &redact.Redactor{}
	c, err := open(resolvedConnection("crm", "crm-cloud-reader", cloudEnv, cloudOrigin), resolver(red), red, limited)
	if err != nil {
		t.Fatalf("open() = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.ListCompanies(ctx, ListOptions{})
	if class := classOf(err); class != provider.ClassTimeout {
		t.Fatalf("class = %q, want %q (%v)", class, provider.ClassTimeout, err)
	}
}

// The connection test reads one company and then verifies the workspace schema, so an incompatible
// workspace fails here instead of answering a business call with an incomplete record.
func TestTestConnectionChecksAccessAndWorkspaceSchema(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		requests := make([]string, 0, 2)
		serve(t, func(request *http.Request) (*http.Response, error) {
			requests = append(requests, request.URL.Path+"?"+request.URL.RawQuery)
			if request.URL.Path == schemaPath {
				return jsonResponse(http.StatusOK, schemaBody), nil
			}
			return jsonResponse(http.StatusOK, `{"data":{"companies":[]},"pageInfo":{"hasNextPage":false}}`), nil
		})
		stubLimiter(t, cloudKey)

		class, err := TestConnection(context.Background(),
			resolvedConnection("crm", "crm-cloud-reader", cloudEnv, cloudOrigin), resolver(nil), nil)
		if err != nil || class != provider.ClassOK {
			t.Fatalf("TestConnection() = %q, %v", class, err)
		}
		want := []string{"/rest/companies?depth=0&limit=1", "/open-api/core?"}
		if !reflect.DeepEqual(requests, want) {
			t.Errorf("requests = %v, want %v", requests, want)
		}
	})

	t.Run("an unusable key is an auth failure, not a schema problem", func(t *testing.T) {
		serve(t, func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == schemaPath {
				t.Error("the schema was read although the key was rejected")
			}
			return jsonResponse(http.StatusUnauthorized, `{"messages":["`+bodyCanary+`"]}`), nil
		})
		stubLimiter(t, cloudKey)
		class, err := TestConnection(context.Background(),
			resolvedConnection("crm", "crm-cloud-reader", cloudEnv, cloudOrigin), resolver(nil), nil)
		if err != nil || class != provider.ClassAuth {
			t.Fatalf("TestConnection() = %q, %v", class, err)
		}
	})

	t.Run("a key without company permission reports permission", func(t *testing.T) {
		serve(t, func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusForbidden, `{"messages":["`+bodyCanary+`"]}`), nil
		})
		stubLimiter(t, cloudKey)
		class, err := TestConnection(context.Background(),
			resolvedConnection("crm", "crm-cloud-reader", cloudEnv, cloudOrigin), resolver(nil), nil)
		if err != nil || class != provider.ClassPermission {
			t.Fatalf("TestConnection() = %q, %v", class, err)
		}
	})

	incompatible := map[string]string{
		"a workspace without the company routes": `{"paths":{"/people":{}},"components":{"schemas":{` +
			`"CompanyForResponse":{"properties":{"id":{},"name":{},"domainName":{},"createdAt":{},"updatedAt":{}}}}}}`,
		"a workspace without a company object": `{"paths":{"/companies":{},"/companies/{id}":{}},` +
			`"components":{"schemas":{"Person":{"properties":{"id":{}}}}}}`,
		"a workspace missing a company field": `{"paths":{"/companies":{},"/companies/{id}":{}},` +
			`"components":{"schemas":{"CompanyForResponse":{"properties":{"id":{},"name":{},"createdAt":{}}}}}}`,
	}
	for name, document := range incompatible {
		t.Run(name, func(t *testing.T) {
			serve(t, func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == schemaPath {
					return jsonResponse(http.StatusOK, document), nil
				}
				return jsonResponse(http.StatusOK, `{"data":{"companies":[]},"pageInfo":{"hasNextPage":false}}`), nil
			})
			stubLimiter(t, cloudKey)

			class, err := TestConnection(context.Background(),
				resolvedConnection("crm", "crm-cloud-reader", cloudEnv, cloudOrigin), resolver(nil), nil)
			if err == nil {
				t.Fatalf("TestConnection() = %q, want an explained incompatibility", class)
			}
			if class != "" {
				t.Errorf("class = %q, want none beside the explanation", class)
			}
			// The explanation names the workspace problem so a human can act on it.
			if !strings.Contains(err.Error(), "workspace") {
				t.Errorf("error = %v, want it to explain the workspace problem", err)
			}
			if strings.Contains(err.Error(), cloudKey) {
				t.Errorf("error carries the API key: %v", err)
			}
		})
	}

	t.Run("an unusable connection reports its class instead of failing", func(t *testing.T) {
		refuse(t)
		class, err := TestConnection(context.Background(),
			resolvedConnection("crm", "crm-cloud-reader", cloudEnv, "http://api.twenty.com"), resolver(nil), nil)
		if err != nil || class != provider.ClassProviderError {
			t.Fatalf("TestConnection() = %q, %v", class, err)
		}
	})
}

// Both operations satisfy their registered contract end to end through the application core: the core
// validates the arguments, selects the explicit connection, resolves the key of that workspace, and
// validates the normalised result against the descriptor.
func TestOperationsSatisfyTheirContractThroughTheApplicationCore(t *testing.T) {
	serve(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == companiesPath {
			return jsonResponse(http.StatusOK, listBody), nil
		}
		return jsonResponse(http.StatusOK, companyBody), nil
	})
	stubLimiter(t, cloudKey)
	stubLimiter(t, internalKey)

	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)

	list, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "twentycrm.companies.list", Connection: "crm",
		Arguments: json.RawMessage(`{"name_contains":"Bike","limit":25,"sort":"created_at","direction":"desc"}`),
	})
	if err != nil {
		t.Fatalf("invoke list = %v", err)
	}
	var listed struct {
		Companies []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Domain string `json:"domain"`
		} `json:"companies"`
		NextCursor string `json:"next_cursor"`
		HasMore    bool   `json:"has_more"`
	}
	if err := json.Unmarshal(list.Result, &listed); err != nil {
		t.Fatalf("list result = %s: %v", list.Result, err)
	}
	if len(listed.Companies) != 2 || listed.Companies[0].ID != companyID ||
		listed.Companies[0].Domain != "bike-ride.example" || listed.NextCursor != "cursor-page-2" ||
		!listed.HasMore {
		t.Errorf("list result = %s", list.Result)
	}
	if strings.Contains(string(list.Result), customCanary) {
		t.Errorf("the core answered with a workspace-specific field: %s", list.Result)
	}

	got, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "twentycrm.companies.get", Connection: "crm-internal",
		Arguments: json.RawMessage(`{"id":"` + companyID + `"}`),
	})
	if err != nil {
		t.Fatalf("invoke get = %v", err)
	}
	if !strings.Contains(string(got.Result), `"domain":"bike-ride.example"`) ||
		!strings.Contains(string(got.Result), `"updated_at":"2026-06-02T08:11:03.000Z"`) {
		t.Errorf("get result = %s", got.Result)
	}
}

// The core refuses what the contract does not allow before a provider is contacted, and it never guesses
// a workspace when two connections could serve the request.
func TestTheCoreRefusesUnsupportedRequestsBeforeProviderIO(t *testing.T) {
	refuse(t)
	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)

	tests := []struct {
		name    string
		request application.InvokeRequest
	}{
		{"two workspaces without an explicit connection", application.InvokeRequest{
			Operation: "twentycrm.companies.list", Arguments: json.RawMessage(`{}`)}},
		{"unknown argument", application.InvokeRequest{
			Operation: "twentycrm.companies.list", Connection: "crm",
			Arguments: json.RawMessage(`{"depth":1}`)}},
		{"a base url as an argument", application.InvokeRequest{
			Operation: "twentycrm.companies.list", Connection: "crm",
			Arguments: json.RawMessage(`{"base_url":"https://attacker.example.invalid"}`)}},
		{"page size above the bound", application.InvokeRequest{
			Operation: "twentycrm.companies.list", Connection: "crm",
			Arguments: json.RawMessage(`{"limit":200}`)}},
		{"a filter separator in the search term", application.InvokeRequest{
			Operation: "twentycrm.companies.list", Connection: "crm",
			Arguments: json.RawMessage(`{"name_contains":"Acme\",or(name[ilike]:\"%"}`)}},
		{"unknown sort property", application.InvokeRequest{
			Operation: "twentycrm.companies.list", Connection: "crm",
			Arguments: json.RawMessage(`{"sort":"annual_revenue"}`)}},
		{"a cursor that carries a parameter", application.InvokeRequest{
			Operation: "twentycrm.companies.list", Connection: "crm",
			Arguments: json.RawMessage(`{"cursor":"abc&limit=200"}`)}},
		{"identifier that is not a UUID", application.InvokeRequest{
			Operation: "twentycrm.companies.get", Connection: "crm",
			Arguments: json.RawMessage(`{"id":"../people/1234567890123456789012345"}`)}},
		{"a connection of another provider", application.InvokeRequest{
			Operation: "twentycrm.companies.get", Connection: "wiki",
			Arguments: json.RawMessage(`{"id":"` + companyID + `"}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := core.Invoke(context.Background(), tt.request); err == nil {
				t.Fatal("the core accepted the request")
			}
		})
	}
}

// A resolved API key never reaches a result, a diagnostic, or an error, whatever the workspace answers.
func TestTheAPIKeyNeverReachesTheOutput(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"companies":[{"id":"`+companyID+`",`+
			`"name":"`+cloudKey+`","domainName":{"primaryLinkLabel":"Bearer `+cloudKey+`",`+
			`"primaryLinkUrl":"https://acme.example"}}]},"pageInfo":{"hasNextPage":false}}`), nil
	})
	stubLimiter(t, cloudKey)

	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)
	response, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "twentycrm.companies.list", Connection: "crm", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("invoke = %v", err)
	}
	if strings.Contains(string(response.Result), cloudKey) {
		t.Errorf("the result carries the API key: %s", response.Result)
	}
	if !strings.Contains(string(response.Result), redact.Marker) {
		t.Errorf("the result was not redacted: %s", response.Result)
	}
	var value any
	if err := json.Unmarshal(response.Result, &value); err != nil {
		t.Errorf("the redacted result is not valid JSON: %v", err)
	}
}

// registry returns a registry with Twenty and one foreign provider, so a wrong route is provable.
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

// coreConfig configures two Twenty workspaces with separate origins and credentials, plus one foreign
// connection.
func coreConfig() *config.Config {
	return &config.Config{
		Version: 1,
		Services: map[string]config.Service{
			"crm-cloud":      {Provider: Provider, BaseURL: cloudOrigin},
			"crm-selfhosted": {Provider: Provider, BaseURL: selfHosted},
			"wiki":           {Provider: "wiki", BaseURL: "https://wiki.example.invalid"},
		},
		Credentials: map[string]config.Credential{
			"crm-cloud-reader": {
				Type: config.CredentialTypeEnv, Values: map[string]string{roleAPIKey: cloudEnv},
			},
			"crm-selfhosted-reader": {
				Type: config.CredentialTypeEnv, Values: map[string]string{roleAPIKey: internalEnv},
			},
			"wiki-reader": {Type: config.CredentialTypeEnv, Values: map[string]string{"token-id": "WIKI_ID"}},
		},
		Connections: map[string]config.Connection{
			"crm":          {Service: "crm-cloud", Credential: "crm-cloud-reader"},
			"crm-internal": {Service: "crm-selfhosted", Credential: "crm-selfhosted-reader"},
			"wiki":         {Service: "wiki", Credential: "wiki-reader"},
		},
	}
}

// stubLimiter gives one API key a limiter without waiting time, so a test that performs several requests
// does not sleep. The spacing itself is proven by TestRateLimitSpacesRequestsOfOneKey.
func stubLimiter(t *testing.T, key string) {
	t.Helper()
	digest := sha256.Sum256([]byte(key))
	limitersMu.Lock()
	limiters[digest] = freeLimiter()
	limitersMu.Unlock()
	t.Cleanup(func() {
		limitersMu.Lock()
		delete(limiters, digest)
		limitersMu.Unlock()
	})
}

func classOf(err error) provider.Class {
	var providerErr *provider.Error
	if errors.As(err, &providerErr) {
		return providerErr.Class
	}
	return ""
}
