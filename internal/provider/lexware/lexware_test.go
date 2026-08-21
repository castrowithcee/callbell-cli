package lexware

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

// The canaries stand for an API key and for provider content. No test reaches a productive organization:
// every request is answered by the package's own transport seam.
const (
	primaryKey   = "canary-lexware-primary-key-8f2c41"
	archiveKey   = "canary-lexware-archive-key-3ad907"
	primaryEnv   = "TEST_LEXWARE_PRIMARY_KEY"
	archiveEnv   = "TEST_LEXWARE_ARCHIVE_KEY"
	invoiceID    = "f3d3ae48-30d9-4b56-973a-b3159cbe743c"
	bodyCanary   = "provider-body-canary-lexware-51ab"
	sampleNumber = "RE1012"
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

func resolvedConnection(name, credential, env string) *config.Resolved {
	return &config.Resolved{
		Name: name, Provider: Provider, BaseURL: gateway, Service: "lexware", Credential: credential,
		Secrets: config.Credential{
			Type: config.CredentialTypeEnv, Values: map[string]string{roleAPIKey: env},
		},
	}
}

func resolver(red *redact.Redactor) *secret.Resolver {
	return secret.NewWith(func(name string) string {
		switch name {
		case primaryEnv:
			return primaryKey
		case archiveEnv:
			return archiveKey
		}
		return ""
	}, nil, nil, red)
}

// freeLimiter is a limiter that never waits. The rate limit itself is proven by its own test.
func freeLimiter() *limiter {
	return newLimiter(0, time.Now, sleepFor)
}

// client opens the primary connection with the package transport currently installed.
func client(t *testing.T) (*Client, *redact.Redactor) {
	t.Helper()
	red := &redact.Redactor{}
	c, err := open(resolvedConnection("lexware-primary", "lexware-key", primaryEnv), resolver(red), red, freeLimiter())
	if err != nil {
		t.Fatalf("open() = %v", err)
	}
	return c, red
}

const listBody = `{
  "content": [
    {"id":"f3d3ae48-30d9-4b56-973a-b3159cbe743c","voucherType":"invoice","voucherStatus":"open",
     "voucherNumber":"RE1012","voucherDate":"2026-05-14T00:00:00.000+02:00",
     "createdDate":"2026-05-14T16:52:21.000+02:00","updatedDate":"2026-05-14T16:52:21.000+02:00",
     "dueDate":"2026-05-24T00:00:00.000+02:00","contactId":"777c7793-9fbb-4ec7-9254-0619c199761e",
     "contactName":"Musterfrau, Erika","totalAmount":99.8,"openAmount":74.8,"currency":"EUR","archived":false},
    {"id":"55aa6de8-d32d-47bd-9c3c-d541ab65a8e8","voucherType":"invoice","voucherStatus":"overdue",
     "voucherNumber":"RE1011","voucherDate":"2026-03-02T00:00:00.000+01:00",
     "dueDate":"2026-04-06T00:00:00.000+02:00","contactId":null,"contactName":"Test GmbH",
     "totalAmount":498.8,"openAmount":null,"currency":"EUR","archived":false},
    {"id":"11111111-1111-1111-1111-111111111111","voucherType":"purchaseinvoice","voucherStatus":"open",
     "voucherNumber":"2010096","voucherDate":"2026-06-14T00:00:00.000+02:00","contactName":"Sammellieferant",
     "totalAmount":80.04,"openAmount":80.04,"currency":"EUR","archived":false},
    {"id":"22222222-2222-2222-2222-222222222222","voucherType":"invoice","voucherStatus":"open",
     "voucherNumber":"RE0900","voucherDate":"2025-01-02T00:00:00.000+01:00","contactName":"Archiv GmbH",
     "totalAmount":10,"openAmount":10,"currency":"EUR","archived":true}
  ],
  "first": true, "last": false, "totalPages": 3, "totalElements": 57, "numberOfElements": 4,
  "size": 25, "number": 0
}`

const invoiceBody = `{
  "id":"f3d3ae48-30d9-4b56-973a-b3159cbe743c","organizationId":"aa93e8a8-2aa3-470b-b914-caad8a255dd8",
  "createdDate":"2026-05-14T16:52:21.000+02:00","updatedDate":"2026-05-14T16:52:21.000+02:00",
  "version":2,"language":"de","archived":false,"voucherStatus":"overdue","voucherNumber":"RE1012",
  "voucherDate":"2026-05-14T00:00:00.000+02:00","dueDate":"2026-05-24T00:00:00.000+02:00",
  "address":{"contactId":"777c7793-9fbb-4ec7-9254-0619c199761e","name":"Bike & Ride GmbH & Co. KG",
   "supplement":"Gebäude 10","street":"Musterstraße 42","city":"Freiburg","zip":"79112","countryCode":"DE"},
  "lineItems":[
    {"id":"97b98491-e953-4dc9-97a9-ae437a8052b4","type":"material","name":"Abus Kabelschloss",
     "description":"` + bodyCanary + `","quantity":2,"unitName":"Stück",
     "unitPrice":{"currency":"EUR","netAmount":13.4,"grossAmount":15.95,"taxRatePercentage":19},
     "discountPercentage":50,"lineItemAmount":13.4}],
  "totalPrice":{"currency":"EUR","totalNetAmount":26.72,"totalGrossAmount":29.85,"totalTaxAmount":3.13},
  "taxAmounts":[{"taxRatePercentage":19,"taxAmount":2.55,"netAmount":13.4}],
  "taxConditions":{"taxType":"net"},
  "shippingConditions":{"shippingType":"delivery"},
  "title":"Rechnung","introduction":"Ihre bestellten Positionen","remark":"Vielen Dank"
}`

// Register publishes the configuration metadata the TUI needs and exactly two read-only operations.
func TestRegisterPublishesMetadataAndTwoReadOnlyOperations(t *testing.T) {
	reg := capability.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	metadata, ok := reg.ProviderMetadata(Provider)
	if !ok || metadata.Name != "Lexware Office" || metadata.DefaultBaseURL != gateway ||
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
	if operations[0].ID != "lexware.invoices.get" || operations[1].ID != "lexware.invoices.list" {
		t.Errorf("operation IDs = %s, %s", operations[0].ID, operations[1].ID)
	}
}

// The list request carries exactly the fixed voucher filters plus the controlled ones, and nothing else.
func TestListInvoicesSendsOnlyTheFixedAndControlledFilters(t *testing.T) {
	var got url.Values
	calls := 0
	serve(t, func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.Scheme != "https" ||
			request.URL.Host != "api.lexware.io" || request.URL.Path != "/v1/voucherlist" {
			t.Errorf("request = %s %s", request.Method, request.URL.Redacted())
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer "+primaryKey {
			t.Errorf("Authorization header = %q, want the bearer API key", authorization)
		}
		got = request.URL.Query()
		return jsonResponse(http.StatusOK, listBody), nil
	})

	c, _ := client(t)
	if _, err := c.ListInvoices(context.Background(), ListOptions{
		VoucherNumber: sampleNumber, VoucherDateFrom: "2026-01-01", VoucherDateTo: "2026-12-31",
		Page: 2, Size: 50, Sort: "voucher_number", Direction: "asc",
	}); err != nil {
		t.Fatalf("ListInvoices() = %v", err)
	}

	if calls != 1 {
		t.Fatalf("requests = %d, want exactly one", calls)
	}
	want := url.Values{
		"voucherType": {"invoice"}, "voucherStatus": {"open"}, "archived": {"false"},
		"page": {"2"}, "size": {"50"}, "sort": {"voucherNumber,ASC"},
		"voucherNumber": {sampleNumber}, "voucherDateFrom": {"2026-01-01"}, "voucherDateTo": {"2026-12-31"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("query = %v, want %v", got, want)
	}
}

// Omitted arguments produce the deterministic default page, and nothing widens the fixed filters.
func TestListInvoicesUsesDeterministicDefaults(t *testing.T) {
	var got url.Values
	serve(t, func(request *http.Request) (*http.Response, error) {
		got = request.URL.Query()
		return jsonResponse(http.StatusOK, listBody), nil
	})

	c, _ := client(t)
	if _, err := c.ListInvoices(context.Background(), ListOptions{}); err != nil {
		t.Fatalf("ListInvoices() = %v", err)
	}
	want := url.Values{
		"voucherType": {"invoice"}, "voucherStatus": {"open"}, "archived": {"false"},
		"page": {"0"}, "size": {"25"}, "sort": {"voucherDate,DESC"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("query = %v, want %v", got, want)
	}
}

// Open and overdue invoices are kept, the overdue special case is made explicit, and a record outside the
// confirmed workflow is dropped. The page metadata is normalised alongside them.
func TestListInvoicesKeepsOpenAndOverdueAndDropsForeignRecords(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, listBody), nil
	})

	c, _ := client(t)
	result, err := c.ListInvoices(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("ListInvoices() = %v", err)
	}

	want := &ListResult{
		Invoices: []ListInvoice{
			{
				ID: invoiceID, VoucherNumber: "RE1012", VoucherStatus: "open", Overdue: false,
				VoucherDate: "2026-05-14T00:00:00.000+02:00", DueDate: "2026-05-24T00:00:00.000+02:00",
				ContactName: "Musterfrau, Erika", TotalAmount: "99.8", OpenAmount: "74.8", Currency: "EUR",
			},
			{
				ID: "55aa6de8-d32d-47bd-9c3c-d541ab65a8e8", VoucherNumber: "RE1011",
				VoucherStatus: "overdue", Overdue: true, VoucherDate: "2026-03-02T00:00:00.000+01:00",
				DueDate: "2026-04-06T00:00:00.000+02:00", ContactName: "Test GmbH",
				TotalAmount: "498.8", Currency: "EUR",
			},
		},
		Page: 0, Size: 25, TotalPages: 3, TotalInvoices: 57, LastPage: false,
	}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("result = %#v, want %#v", result, want)
	}
}

// Pagination stays deterministic: the requested page reaches the provider and its answer is reported back
// unchanged, including the last-page marker.
func TestListInvoicesPaginatesDeterministically(t *testing.T) {
	pages := map[string]string{
		"0": `{"content":[],"totalPages":2,"totalElements":30,"size":25,"number":0,"last":false}`,
		"1": `{"content":[],"totalPages":2,"totalElements":30,"size":25,"number":1,"last":true}`,
	}
	requested := make([]string, 0, 2)
	serve(t, func(request *http.Request) (*http.Response, error) {
		page := request.URL.Query().Get("page")
		requested = append(requested, page)
		return jsonResponse(http.StatusOK, pages[page]), nil
	})

	c, _ := client(t)
	for _, page := range []int{0, 1} {
		result, err := c.ListInvoices(context.Background(), ListOptions{Page: page})
		if err != nil {
			t.Fatalf("ListInvoices(page %d) = %v", page, err)
		}
		if result.Page != page || result.TotalPages != 2 || result.TotalInvoices != 30 ||
			result.LastPage != (page == 1) || len(result.Invoices) != 0 {
			t.Errorf("page %d = %#v", page, result)
		}
	}
	if !reflect.DeepEqual(requested, []string{"0", "1"}) {
		t.Errorf("requested pages = %v, want one request per page", requested)
	}
}

// Bounds and formats are refused before any provider I/O happens.
func TestListInvoicesRejectsUnusableOptionsBeforeIO(t *testing.T) {
	refuse(t)
	c, _ := client(t)

	tests := map[string]ListOptions{
		"page size zero is impossible":  {Size: -1},
		"page size above the bound":     {Size: maxPageSize + 1},
		"negative page":                 {Page: -1},
		"page above the bound":          {Page: maxPage + 1},
		"unknown sort property":         {Sort: "contact_name"},
		"unknown sort direction":        {Direction: "sideways"},
		"malformed voucher date filter": {VoucherDateFrom: "14.05.2026"},
		"impossible voucher date":       {VoucherDateTo: "2026-13-45"},
		"oversized voucher number":      {VoucherNumber: strings.Repeat("R", 65)},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := c.ListInvoices(context.Background(), options); err == nil {
				t.Fatal("ListInvoices() accepted the options")
			}
		})
	}
}

// A list record without a usable identifier is a contract violation, not a result.
func TestListInvoicesRejectsAnUnusableIdentifier(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK,
			`{"content":[{"id":"not-a-uuid","voucherType":"invoice","voucherNumber":"RE1"}],"size":25}`), nil
	})

	c, _ := client(t)
	_, err := c.ListInvoices(context.Background(), ListOptions{})
	if class := classOf(err); class != provider.ClassInvalidResponse {
		t.Fatalf("class = %q, want %q (%v)", class, provider.ClassInvalidResponse, err)
	}
}

// The get operation reads exactly the requested invoice and produces no other provider I/O.
func TestGetInvoiceReadsExactlyTheRequestedInvoice(t *testing.T) {
	requests := make([]string, 0, 1)
	serve(t, func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.Method+" "+request.URL.Path+"?"+request.URL.RawQuery)
		return jsonResponse(http.StatusOK, invoiceBody), nil
	})

	c, _ := client(t)
	invoice, err := c.GetInvoice(context.Background(), invoiceID)
	if err != nil {
		t.Fatalf("GetInvoice() = %v", err)
	}
	if !reflect.DeepEqual(requests, []string{"GET /v1/invoices/" + invoiceID + "?"}) {
		t.Fatalf("requests = %v, want exactly one read of the invoice", requests)
	}

	if invoice.ID != invoiceID || invoice.VoucherNumber != "RE1012" ||
		invoice.VoucherStatus != "overdue" || !invoice.Overdue || invoice.Archived ||
		invoice.TaxType != "net" || invoice.Currency != "EUR" ||
		invoice.TotalNetAmount != "26.72" || invoice.TotalGrossAmount != "29.85" ||
		invoice.TotalTaxAmount != "3.13" || invoice.Title != "Rechnung" {
		t.Errorf("invoice = %#v", invoice)
	}
	if invoice.Contact == nil || invoice.Contact.Name != "Bike & Ride GmbH & Co. KG" ||
		invoice.Contact.Zip != "79112" || invoice.Contact.CountryCode != "DE" ||
		invoice.Contact.ID != "777c7793-9fbb-4ec7-9254-0619c199761e" {
		t.Errorf("contact = %#v", invoice.Contact)
	}
	if len(invoice.LineItems) != 1 {
		t.Fatalf("line items = %#v", invoice.LineItems)
	}
	item := invoice.LineItems[0]
	if item.Type != "material" || item.Name != "Abus Kabelschloss" || item.Quantity != "2" ||
		item.UnitName != "Stück" || item.UnitNetAmount != "13.4" || item.UnitGrossAmount != "15.95" ||
		item.TaxRatePercentage != "19" || item.DiscountPercentage != "50" || item.Amount != "13.4" {
		t.Errorf("line item = %#v", item)
	}
}

// Only a validated UUID reaches the provider, and an answer about another invoice is refused.
func TestGetInvoiceGuardsTheIdentifier(t *testing.T) {
	t.Run("a malformed identifier stops before the provider", func(t *testing.T) {
		refuse(t)
		c, _ := client(t)
		for _, id := range []string{"", "42", "../../v1/contacts", invoiceID + "0", strings.ToUpper("zz") +
			invoiceID[2:]} {
			if _, err := c.GetInvoice(context.Background(), id); err == nil {
				t.Errorf("GetInvoice(%q) was accepted", id)
			}
		}
	})

	t.Run("another invoice is an invalid response", func(t *testing.T) {
		serve(t, func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"id":"55aa6de8-d32d-47bd-9c3c-d541ab65a8e8"}`), nil
		})
		c, _ := client(t)
		_, err := c.GetInvoice(context.Background(), invoiceID)
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
		{http.StatusPaymentRequired, provider.ClassPermission},
		{http.StatusForbidden, provider.ClassPermission},
		{http.StatusNotFound, provider.ClassProviderError},
		{http.StatusNotAcceptable, provider.ClassProviderError},
		{http.StatusTooManyRequests, provider.ClassRateLimited},
		{http.StatusGatewayTimeout, provider.ClassTimeout},
		{http.StatusInternalServerError, provider.ClassProviderError},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			serve(t, func(*http.Request) (*http.Response, error) {
				return jsonResponse(tt.status, `{"message":"`+bodyCanary+`"}`), nil
			})
			c, _ := client(t)
			_, err := c.ListInvoices(context.Background(), ListOptions{})
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
			_, err := c.ListInvoices(context.Background(), ListOptions{})
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
	_, err := c.ListInvoices(context.Background(), ListOptions{})
	if class := classOf(err); class != provider.ClassInvalidResponse {
		t.Fatalf("class = %q, want %q (%v)", class, provider.ClassInvalidResponse, err)
	}
}

// Opening a connection registers the API key and the derived bearer value with the redactor, and refuses
// any base URL other than the fixed gateway.
func TestOpenRedactsTheKeyAndBindsTheFixedGateway(t *testing.T) {
	red := &redact.Redactor{}
	if _, err := open(resolvedConnection("lexware-primary", "lexware-key", primaryEnv),
		resolver(red), red, freeLimiter()); err != nil {
		t.Fatalf("open() = %v", err)
	}
	for _, value := range []string{primaryKey, "Bearer " + primaryKey} {
		if got := red.Apply("diagnostic " + value); strings.Contains(got, primaryKey) {
			t.Errorf("redactor keeps %q: %q", value, got)
		}
	}

	for _, base := range []string{"https://api.lexware.example.invalid", "http://api.lexware.io",
		"https://api.lexware.io.attacker.invalid", ""} {
		connection := resolvedConnection("lexware-primary", "lexware-key", primaryEnv)
		connection.BaseURL = base
		if _, err := open(connection, resolver(red), red, freeLimiter()); err == nil {
			t.Errorf("open() accepted base URL %q", base)
		}
	}

	missing := resolvedConnection("lexware-primary", "lexware-key", "TEST_LEXWARE_ABSENT")
	if _, err := open(missing, resolver(red), red, freeLimiter()); err == nil {
		t.Error("open() accepted a credential without a secret")
	}
}

// Two connections carry two API keys. Each request uses the key of its own connection, and the keys get
// separate rate-limit budgets.
func TestConnectionsKeepTheirOwnKeyAndRateBudget(t *testing.T) {
	authorizations := make([]string, 0, 2)
	serve(t, func(request *http.Request) (*http.Response, error) {
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		return jsonResponse(http.StatusOK, `{"content":[],"size":25}`), nil
	})

	red := &redact.Redactor{}
	secrets := resolver(red)
	for _, connection := range []*config.Resolved{
		resolvedConnection("lexware-primary", "primary-key", primaryEnv),
		resolvedConnection("lexware-archive", "archive-key", archiveEnv),
	} {
		c, err := open(connection, secrets, red, freeLimiter())
		if err != nil {
			t.Fatalf("open(%s) = %v", connection.Name, err)
		}
		if _, err := c.ListInvoices(context.Background(), ListOptions{}); err != nil {
			t.Fatalf("ListInvoices(%s) = %v", connection.Name, err)
		}
	}

	want := []string{"Bearer " + primaryKey, "Bearer " + archiveKey}
	if !reflect.DeepEqual(authorizations, want) {
		t.Errorf("authorizations = %v, want each connection to use its own key", authorizations)
	}
	if limiterFor(primaryKey) == limiterFor(archiveKey) {
		t.Error("two API keys share one rate-limit budget")
	}
	if limiterFor(primaryKey) != limiterFor(primaryKey) {
		t.Error("one API key does not keep one rate-limit budget")
	}
}

// Requests that share a key are spaced by the documented limit of two requests per second.
func TestRateLimitSpacesRequestsOfOneKey(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"content":[],"size":25}`), nil
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
	c, err := open(resolvedConnection("lexware-primary", "lexware-key", primaryEnv), resolver(red), red, limited)
	if err != nil {
		t.Fatalf("open() = %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.ListInvoices(context.Background(), ListOptions{}); err != nil {
			t.Fatalf("ListInvoices() = %v", err)
		}
	}

	if !reflect.DeepEqual(waited, []time.Duration{minInterval, minInterval}) {
		t.Errorf("waits = %v, want two waits of %v", waited, minInterval)
	}
}

// A cancelled request never becomes a provider call while it waits for the rate limit.
func TestRateLimitRespectsCancellation(t *testing.T) {
	refuse(t)
	limited := newLimiter(minInterval, time.Now, sleepFor)
	limited.next = time.Now().Add(time.Hour)

	red := &redact.Redactor{}
	c, err := open(resolvedConnection("lexware-primary", "lexware-key", primaryEnv), resolver(red), red, limited)
	if err != nil {
		t.Fatalf("open() = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.ListInvoices(ctx, ListOptions{})
	if class := classOf(err); class != provider.ClassTimeout {
		t.Fatalf("class = %q, want %q (%v)", class, provider.ClassTimeout, err)
	}
}

// The connection test is the smallest authenticated read of the confirmed workflow.
func TestTestConnectionReadsOneInvoiceAndReportsTheClass(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		var got url.Values
		serve(t, func(request *http.Request) (*http.Response, error) {
			got = request.URL.Query()
			return jsonResponse(http.StatusOK, `{"content":[],"size":1}`), nil
		})
		stubLimiter(t, primaryKey)
		class, err := TestConnection(context.Background(),
			resolvedConnection("lexware-primary", "lexware-key", primaryEnv), resolver(nil), nil)
		if err != nil || class != provider.ClassOK {
			t.Fatalf("TestConnection() = %q, %v", class, err)
		}
		want := url.Values{
			"voucherType": {"invoice"}, "voucherStatus": {"open"}, "archived": {"false"},
			"page": {"0"}, "size": {"1"}, "sort": {"voucherDate,DESC"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("query = %v, want %v", got, want)
		}
	})

	t.Run("auth", func(t *testing.T) {
		serve(t, func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusUnauthorized, `{"message":"`+bodyCanary+`"}`), nil
		})
		stubLimiter(t, primaryKey)
		class, err := TestConnection(context.Background(),
			resolvedConnection("lexware-primary", "lexware-key", primaryEnv), resolver(nil), nil)
		if err != nil || class != provider.ClassAuth {
			t.Fatalf("TestConnection() = %q, %v", class, err)
		}
	})

	t.Run("an unusable connection reports its class instead of failing", func(t *testing.T) {
		refuse(t)
		connection := resolvedConnection("lexware-primary", "lexware-key", primaryEnv)
		connection.BaseURL = "https://api.lexware.example.invalid"
		class, err := TestConnection(context.Background(), connection, resolver(nil), nil)
		if err != nil || class != provider.ClassProviderError {
			t.Fatalf("TestConnection() = %q, %v", class, err)
		}
	})
}

// Both operations satisfy their registered contract end to end: the core validates the arguments, selects
// the explicit connection, resolves the key, and validates the normalised result against the descriptor.
func TestOperationsSatisfyTheirContractThroughTheApplicationCore(t *testing.T) {
	serve(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/v1/voucherlist" {
			return jsonResponse(http.StatusOK, listBody), nil
		}
		return jsonResponse(http.StatusOK, invoiceBody), nil
	})
	stubLimiter(t, primaryKey)
	stubLimiter(t, archiveKey)

	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)

	list, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "lexware.invoices.list", Connection: "lexware-primary",
		Arguments: json.RawMessage(`{"size":25,"sort":"voucher_date","direction":"desc"}`),
	})
	if err != nil {
		t.Fatalf("invoke list = %v", err)
	}
	var listed struct {
		Invoices []struct {
			ID      string `json:"id"`
			Overdue bool   `json:"overdue"`
		} `json:"invoices"`
		TotalInvoices int  `json:"total_invoices"`
		LastPage      bool `json:"last_page"`
	}
	if err := json.Unmarshal(list.Result, &listed); err != nil {
		t.Fatalf("list result = %s: %v", list.Result, err)
	}
	if len(listed.Invoices) != 2 || listed.Invoices[0].ID != invoiceID || listed.Invoices[1].Overdue != true ||
		listed.TotalInvoices != 57 || listed.LastPage {
		t.Errorf("list result = %s", list.Result)
	}

	got, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "lexware.invoices.get", Connection: "lexware-archive",
		Arguments: json.RawMessage(`{"id":"` + invoiceID + `"}`),
	})
	if err != nil {
		t.Fatalf("invoke get = %v", err)
	}
	if !strings.Contains(string(got.Result), `"voucher_number":"RE1012"`) ||
		!strings.Contains(string(got.Result), `"overdue":true`) {
		t.Errorf("get result = %s", got.Result)
	}
}

// The core refuses what the contract does not allow before a provider is contacted: a missing explicit
// connection, an argument outside the schema, and an identifier that is not a UUID.
func TestTheCoreRefusesUnsupportedRequestsBeforeProviderIO(t *testing.T) {
	refuse(t)
	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)

	tests := []struct {
		name    string
		request application.InvokeRequest
	}{
		{"no explicit connection", application.InvokeRequest{
			Operation: "lexware.invoices.list", Arguments: json.RawMessage(`{}`)}},
		{"unknown argument", application.InvokeRequest{
			Operation: "lexware.invoices.list", Connection: "lexware-primary",
			Arguments: json.RawMessage(`{"voucher_status":"paid"}`)}},
		{"page size above the bound", application.InvokeRequest{
			Operation: "lexware.invoices.list", Connection: "lexware-primary",
			Arguments: json.RawMessage(`{"size":101}`)}},
		{"malformed date filter", application.InvokeRequest{
			Operation: "lexware.invoices.list", Connection: "lexware-primary",
			Arguments: json.RawMessage(`{"voucher_date_from":"14.05.2026"}`)}},
		{"unknown sort property", application.InvokeRequest{
			Operation: "lexware.invoices.list", Connection: "lexware-primary",
			Arguments: json.RawMessage(`{"sort":"contact_name"}`)}},
		{"identifier that is not a UUID", application.InvokeRequest{
			Operation: "lexware.invoices.get", Connection: "lexware-primary",
			Arguments: json.RawMessage(`{"id":"../../v1/contacts/1234567890123456789012345"}`)}},
		{"a connection of another provider", application.InvokeRequest{
			Operation: "lexware.invoices.get", Connection: "wiki",
			Arguments: json.RawMessage(`{"id":"` + invoiceID + `"}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := core.Invoke(context.Background(), tt.request); err == nil {
				t.Fatal("the core accepted the request")
			}
		})
	}
}

// A resolved API key never reaches a result, a diagnostic, or an error, whatever the provider answers.
func TestTheAPIKeyNeverReachesTheOutput(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"content":[{"id":"`+invoiceID+`","voucherType":"invoice",`+
			`"voucherStatus":"open","voucherNumber":"`+primaryKey+`","voucherDate":"2026-05-14",`+
			`"contactName":"Bearer `+primaryKey+`"}],"size":25,"number":0,"totalPages":1,`+
			`"totalElements":1,"last":true}`), nil
	})
	stubLimiter(t, primaryKey)

	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)
	response, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "lexware.invoices.list", Connection: "lexware-primary", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("invoke = %v", err)
	}
	if strings.Contains(string(response.Result), primaryKey) {
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

// registry returns a registry with Lexware and one foreign provider, so a wrong route is provable.
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

// coreConfig configures two Lexware connections with separate credentials and one foreign connection.
func coreConfig() *config.Config {
	return &config.Config{
		Version: 1,
		Services: map[string]config.Service{
			"lexware": {Provider: Provider, BaseURL: gateway},
			"wiki":    {Provider: "wiki", BaseURL: "https://wiki.example.invalid"},
		},
		Credentials: map[string]config.Credential{
			"primary-key": {Type: config.CredentialTypeEnv, Values: map[string]string{roleAPIKey: primaryEnv}},
			"archive-key": {Type: config.CredentialTypeEnv, Values: map[string]string{roleAPIKey: archiveEnv}},
			"wiki-reader": {Type: config.CredentialTypeEnv, Values: map[string]string{"token-id": "WIKI_ID"}},
		},
		Connections: map[string]config.Connection{
			"lexware-primary": {Service: "lexware", Credential: "primary-key"},
			"lexware-archive": {Service: "lexware", Credential: "archive-key"},
			"wiki":            {Service: "wiki", Credential: "wiki-reader"},
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
