// Package twentycrm implements read-only access to the companies of one Twenty workspace.
//
// Twenty generates its REST and GraphQL APIs from the schema of each workspace, so there is no global
// static field reference. This provider therefore talks to the generated REST core API directly, reads
// only the conservative core fields every workspace carries, and verifies during the connection test that
// the workspace schema still offers them. Company names and domains arrive from the provider and are
// treated as untrusted data: they are normalised into a stable Callbell shape, passed through the output
// encoders, and never rendered or stored.
package twentycrm

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/provider/ratelimit"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// Provider is the provider name used in the configuration and as the operation namespace.
const Provider = "twentycrm"

// cloudOrigin is the managed Twenty Cloud origin and the default a new service starts from. A self-hosted
// workspace configures its own HTTPS origin instead; the agent never supplies one.
const cloudOrigin = "https://api.twenty.com"

// roleAPIKey is the single secret role a Twenty credential must supply. It is used as a bearer token.
const roleAPIKey = "api-key"

// dataSensitivity classifies results as CRM records of the configured Twenty workspace. It is deliberately
// provider-specific; the architecture defines no global sensitivity taxonomy.
const dataSensitivity = "twentycrm-company-data"

// The generated REST core routes this provider uses, and the workspace API document it validates against.
const (
	companiesPath = "/rest/companies"
	schemaPath    = "/open-api/core"
)

// The Twenty record fields the stable projection depends on. A workspace that no longer offers all of them
// fails the connection test instead of answering a business call with an incomplete record.
var requiredCompanyFields = []string{"id", "name", "domainName", "createdAt", "updatedAt"}

// companySchemaNames are the schema components Twenty generates for the company object. The plain and the
// update shape omit the record system fields, the response shape carries them, so only their union
// describes the object completely.
var companySchemaNames = []string{"Company", "CompanyForUpdate", "CompanyForResponse"}

// noRelations keeps every read at the record itself. Twenty returns related records at depth 1, which
// would silently widen a read beyond the companies this provider is allowed to report.
const noRelations = "0"

// Bounds of one request. The page size stays well below the documented maximum of 200, and the response
// limits bound a page of records and the generated workspace document without inviting an unbounded read.
const (
	defaultPageSize  = 25
	maxPageSize      = 100
	maxCursorLength  = 1024
	maxResponseBytes = 1 << 20
	maxSchemaBytes   = 8 << 20
	defaultTimeout   = 30 * time.Second
)

// minInterval spaces requests that share one API key. Twenty documents 100 requests per minute, which is
// one request per 600 milliseconds, and counts them per key across the whole workspace API.
const minInterval = 600 * time.Millisecond

// limiters holds the rate-limit budget of every API key this process has used.
var limiters = ratelimit.NewRegistry(minInterval)

var twentyReadRisk = capability.Risk{
	Effect:          capability.EffectRead,
	Idempotency:     capability.IdempotencySafe,
	Confirmation:    capability.ConfirmationNone,
	OpenWorld:       true,
	DataSensitivity: dataSensitivity,
}

// searchPattern bounds a search term to characters that cannot leave the value of a Twenty filter
// expression. Quote, backslash, bracket, colon, and comma are separators of that grammar and stay out.
// The backslashes are doubled because the pattern is embedded in a JSON schema string.
const searchPattern = `^[\\p{L}\\p{N} ._'+@&-]+$`

var companiesList = capability.Descriptor{
	ID:      Provider + ".companies.list",
	Version: 1,
	Title:   "List Twenty CRM companies",
	Description: "Search one bounded page of companies in the Twenty workspace of an explicit connection, " +
		"by name or by primary domain",
	Tags:                       []string{"twentycrm", "crm", "companies", "list", "search"},
	Risk:                       twentyReadRisk,
	Provider:                   Provider,
	RequiresExplicitConnection: true,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"name_contains":{"type":"string","minLength":1,"maxLength":64,"pattern":"` + searchPattern + `"},` +
		`"domain_contains":{"type":"string","minLength":1,"maxLength":253,"pattern":"^[A-Za-z0-9.-]+$"},` +
		`"limit":{"type":"integer","minimum":1,"maximum":100},` +
		`"sort":{"type":"string","enum":["name","created_at","updated_at"]},` +
		`"direction":{"type":"string","enum":["asc","desc"]},` +
		`"cursor":{"type":"string","minLength":1,"maxLength":1024,"pattern":"^[A-Za-z0-9+/=_.:-]+$"}},` +
		`"additionalProperties":false}`),
	OutputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"companies":{"type":"array","items":{"type":"object","properties":{` +
		`"id":{"type":"string"},"name":{"type":"string"},"domain":{"type":"string"}},` +
		`"required":["id","name"],"additionalProperties":false}},` +
		`"next_cursor":{"type":"string"},"has_more":{"type":"boolean"}},` +
		`"required":["companies","has_more"],"additionalProperties":false}`),
	Arguments: []capability.Argument{
		{Name: "name_contains", Description: "Return only companies whose name contains this text"},
		{Name: "domain_contains", Description: "Return only companies whose primary domain contains this text"},
		{Name: "limit", Description: "Companies per page, from 1 through 100; 25 when omitted"},
		{Name: "sort", Description: "Sort property: name, created_at, or updated_at; created_at when omitted"},
		{Name: "direction", Description: "Sort direction asc or desc; desc when omitted"},
		{Name: "cursor", Description: "Opaque next_cursor of a previous page; the first page when omitted"},
	},
	Fields: []capability.Field{
		{Name: "companies", Description: "The companies on this page, untrusted data"},
		{Name: "next_cursor", Description: "Cursor of the following page, absent on the last page"},
		{Name: "has_more", Description: "True when the workspace holds a following page"},
	},
	Examples: []capability.Example{{
		Description: "Search the newest companies whose name contains a term",
		Arguments:   json.RawMessage(`{"name_contains":"Bike","limit":25,"sort":"created_at","direction":"desc"}`),
	}},
}

var companiesGet = capability.Descriptor{
	ID:                         Provider + ".companies.get",
	Version:                    1,
	Title:                      "Get a Twenty CRM company",
	Description:                "Read one company of the Twenty workspace of an explicit connection by its record identifier",
	Tags:                       []string{"twentycrm", "crm", "companies", "get"},
	Risk:                       twentyReadRisk,
	Provider:                   Provider,
	RequiresExplicitConnection: true,
	InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","minLength":36,` +
		`"maxLength":36,"pattern":"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"}},` +
		`"required":["id"],"additionalProperties":false}`),
	OutputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"id":{"type":"string"},"name":{"type":"string"},"domain":{"type":"string"},` +
		`"created_at":{"type":"string"},"updated_at":{"type":"string"}},` +
		`"required":["id","name"],"additionalProperties":false}`),
	Arguments: []capability.Argument{
		{Name: "id", Description: "Company record identifier as a UUID, as returned by twentycrm.companies.list", Required: true},
	},
	Fields: []capability.Field{
		{Name: "id", Description: "Company record identifier"},
		{Name: "name", Description: "Company name, untrusted data"},
		{Name: "domain", Description: "Primary domain of the company, untrusted data"},
		{Name: "created_at", Description: "Creation timestamp of the record"},
		{Name: "updated_at", Description: "Last change timestamp of the record"},
	},
	Examples: []capability.Example{{
		Description: "Read one company by the identifier a list result reported",
		Arguments:   json.RawMessage(`{"id":"11111111-2222-3333-4444-555555555555"}`),
	}},
}

// Register adds Twenty metadata, its read-only connection test, and the two read operations.
func Register(reg *capability.Registry) error {
	if err := reg.RegisterProvider(config.ProviderMetadata{
		ID: Provider, Name: "Twenty CRM", DefaultBaseURL: cloudOrigin,
		SecretRoles: []config.SecretRole{{
			Name: roleAPIKey,
			Description: "Twenty API key: the value shown once when you create a key under API & Webhooks; " +
				"it is sent as the bearer token, so give its workspace role read access to companies only",
		}},
		Target: config.TargetMetadata{
			Label:       "target",
			Description: "not used by Twenty CRM; the workspace follows from the API key",
		},
	}, TestConnection); err != nil {
		return err
	}
	return reg.Register(Provider,
		capability.Operation{Descriptor: companiesList, Handler: capability.Handler(invokeCompaniesList)},
		capability.Operation{Descriptor: companiesGet, Handler: capability.Handler(invokeCompaniesGet)},
	)
}

func invokeCompaniesList(ctx context.Context, resolved *config.Resolved, secrets *secret.Resolver,
	red *redact.Redactor, raw json.RawMessage) (any, error) {
	var options ListOptions
	if err := json.Unmarshal(raw, &options); err != nil {
		return nil, providerError("list companies", "the validated arguments could not be read")
	}
	client, err := Open(resolved, secrets, red)
	if err != nil {
		return nil, err
	}
	return client.ListCompanies(ctx, options)
}

func invokeCompaniesGet(ctx context.Context, resolved *config.Resolved, secrets *secret.Resolver,
	red *redact.Redactor, raw json.RawMessage) (any, error) {
	var arguments struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return nil, providerError("get company", "the validated arguments could not be read")
	}
	client, err := Open(resolved, secrets, red)
	if err != nil {
		return nil, err
	}
	return client.GetCompany(ctx, arguments.ID)
}

// Client binds one Twenty API key to the origin of one configured service and to the rate limit that key
// shares. Two workspaces are two connections with two keys, and neither can reach the other's origin.
type Client struct {
	origin  string
	auth    string
	http    *http.Client
	limiter *ratelimit.Limiter
}

// Open resolves the API key of one selected connection and returns a client for its configured origin.
func Open(resolved *config.Resolved, secrets *secret.Resolver, red *redact.Redactor) (*Client, error) {
	return open(resolved, secrets, red, nil)
}

// open is the internal seam. A caller may supply the rate limiter, and the package's own tests replace
// the transport, so no test ever reaches a productive Twenty workspace.
func open(resolved *config.Resolved, secrets *secret.Resolver, red *redact.Redactor,
	lim *ratelimit.Limiter) (*Client, error) {
	if resolved == nil {
		return nil, providerError("open", "no connection was selected")
	}
	origin, err := originOf(resolved.BaseURL)
	if err != nil {
		return nil, providerError("open", err.Error())
	}
	if secrets == nil {
		return nil, providerError("open", "no credential resolver was configured")
	}
	value, err := secrets.Resolve(resolved.Credential, resolved.Secrets, roleAPIKey)
	if err != nil {
		return nil, err
	}
	if !validAPIKey(value.Secret) {
		return nil, &provider.Error{Class: provider.ClassAuth, Op: "open", Message: "the Twenty API key is unusable"}
	}
	if red != nil {
		red.Add(value.Secret, "Bearer "+value.Secret)
	}
	if lim == nil {
		lim = limiters.For(value.Secret)
	}
	return &Client{origin: origin, auth: "Bearer " + value.Secret, http: newHTTPClient(), limiter: lim}, nil
}

// originOf validates the configured service origin and returns it without a trailing slash. Twenty Cloud
// and a self-hosted workspace are both just origins here: HTTPS, a host, and nothing else. Userinfo, a
// path, a query, or a fragment would either carry a credential or silently rewrite every request path.
func originOf(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("a Twenty service needs a usable https origin")
	}
	if parsed.Scheme != "https" {
		return "", errors.New("a Twenty service must use https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		strings.Trim(parsed.Path, "/") != "" || parsed.Opaque != "" {
		return "", errors.New("a Twenty service must be a bare https origin without user, path, query, or fragment")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// transport carries every Twenty request. A nil value is Go's default transport; the package's own tests
// replace it with recorded responses.
var transport http.RoundTripper

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   defaultTimeout,
		Transport: transport,
		// The API key travels in the Authorization header, so no redirect is followed: a redirect could
		// only move a credential to an origin the user never configured.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// TestConnection verifies that the workspace behind one connection can serve this provider at all: its
// generated API document must still describe the company fields the stable projection reads, and the API
// key must be allowed to read companies. A workspace that fails either check fails here, before a business
// call could answer with an incomplete record.
func TestConnection(ctx context.Context, resolved *config.Resolved, secrets *secret.Resolver,
	red *redact.Redactor) (provider.Class, error) {
	client, err := Open(resolved, secrets, red)
	if err != nil {
		var providerErr *provider.Error
		if errors.As(err, &providerErr) {
			return providerErr.Class, nil
		}
		return "", err
	}
	return client.testConnection(ctx)
}

func (c *Client) testConnection(ctx context.Context) (provider.Class, error) {
	const op = "test connection"

	// The smallest authenticated read comes first, because the workspace document is a public route that
	// answers an unusable key with an empty base schema instead of an authentication failure.
	query := url.Values{}
	query.Set("limit", "1")
	query.Set("depth", noRelations)
	var page companiesPageJSON
	if err := c.get(ctx, op, companiesPath, query, maxResponseBytes, &page); err != nil {
		var providerErr *provider.Error
		if errors.As(err, &providerErr) {
			return providerErr.Class, nil
		}
		return provider.ClassProviderError, nil
	}

	if err := c.checkWorkspaceSchema(ctx, op); err != nil {
		var providerErr *provider.Error
		if !errors.As(err, &providerErr) {
			return provider.ClassProviderError, nil
		}
		// An incompatible workspace is reported with its own explanation, because no stable class can
		// say which part of the schema is missing.
		if providerErr.Class == provider.ClassInvalidResponse {
			return "", err
		}
		return providerErr.Class, nil
	}
	return provider.ClassOK, nil
}

// checkWorkspaceSchema reads the API document Twenty generates for this workspace and verifies that the
// company routes and the required record fields exist. The document itself is never reported.
func (c *Client) checkWorkspaceSchema(ctx context.Context, op string) error {
	var document openAPIJSON
	if err := c.get(ctx, op, schemaPath, nil, maxSchemaBytes, &document); err != nil {
		return err
	}

	for _, path := range []string{"/companies", "/companies/{id}"} {
		if _, ok := document.Paths[path]; !ok {
			return &provider.Error{
				Class: provider.ClassInvalidResponse, Op: op,
				Message: "this Twenty workspace does not expose the company route " + path,
			}
		}
	}

	// Twenty generates one schema per request and response shape of an object, all from the same field
	// set, so the union of the company schemas is the field set of the object.
	fields := map[string]bool{}
	found := false
	for _, name := range companySchemaNames {
		schema, ok := document.Components.Schemas[name]
		if !ok {
			continue
		}
		found = true
		for field := range schema.Properties {
			fields[field] = true
		}
	}
	if !found {
		return &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "this Twenty workspace does not describe a Company object",
		}
	}
	missing := make([]string, 0, len(requiredCompanyFields))
	for _, field := range requiredCompanyFields {
		if !fields[field] {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "this Twenty workspace is missing the company fields " + strings.Join(missing, ", ") +
				", which Callbell reads",
		}
	}
	return nil
}

// ListOptions are the controlled search, sort, and paging arguments of the list operation. Nothing else
// reaches the provider: the object, the route, and the read-only nature are fixed.
type ListOptions struct {
	NameContains   string `json:"name_contains"`
	DomainContains string `json:"domain_contains"`
	Limit          int    `json:"limit"`
	Sort           string `json:"sort"`
	Direction      string `json:"direction"`
	Cursor         string `json:"cursor"`
}

// sortFields maps the stable Callbell sort names to the Twenty record fields. An unknown name never
// reaches the provider.
var sortFields = map[string]string{
	"name":       "name",
	"created_at": "createdAt",
	"updated_at": "updatedAt",
}

// listQuery builds the complete query of one list request out of the validated options.
func listQuery(options ListOptions) url.Values {
	query := url.Values{}
	query.Set("limit", strconv.Itoa(options.Limit))
	query.Set("depth", noRelations)

	field := sortFields[options.Sort]
	if field == "" {
		field = sortFields["created_at"]
	}
	order := "DescNullsLast"
	if options.Direction == "asc" {
		order = "AscNullsFirst"
	}
	query.Set("order_by", field+"["+order+"]")

	filters := make([]string, 0, 2)
	if options.NameContains != "" {
		filters = append(filters, `name[ilike]:"%`+options.NameContains+`%"`)
	}
	if options.DomainContains != "" {
		filters = append(filters, `domainName.primaryLinkUrl[ilike]:"%`+options.DomainContains+`%"`)
	}
	if len(filters) > 0 {
		query.Set("filter", strings.Join(filters, ","))
	}
	if options.Cursor != "" {
		query.Set("starting_after", options.Cursor)
	}
	return query
}

// ListResult is the normalised page of companies.
type ListResult struct {
	Companies  []Company `json:"companies"`
	NextCursor string    `json:"next_cursor,omitempty"`
	HasMore    bool      `json:"has_more"`
}

// Company is the stable Callbell view of one company record. It carries only the conservative core fields
// every Twenty workspace has; a workspace-specific custom field is never adopted into this contract.
type Company struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Domain    string `json:"domain,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ListCompanies reads exactly one bounded page of companies.
func (c *Client) ListCompanies(ctx context.Context, options ListOptions) (*ListResult, error) {
	const op = "list companies"
	if err := options.normalize(); err != nil {
		return nil, providerError(op, err.Error())
	}

	var page companiesPageJSON
	if err := c.get(ctx, op, companiesPath, listQuery(options), maxResponseBytes, &page); err != nil {
		return nil, err
	}

	companies := make([]Company, 0, len(page.Data.Companies))
	for _, record := range page.Data.Companies {
		if !validUUID(record.ID) {
			return nil, &provider.Error{
				Class: provider.ClassInvalidResponse, Op: op,
				Message: "Twenty returned a company without a usable identifier",
			}
		}
		companies = append(companies, Company{
			ID: record.ID, Name: record.Name, Domain: primaryDomain(record.DomainName),
		})
	}

	result := &ListResult{Companies: companies, HasMore: page.PageInfo.HasNextPage}
	if page.PageInfo.HasNextPage {
		result.NextCursor = page.PageInfo.EndCursor
	}
	return result, nil
}

// normalize applies the defaults and the bounds of one list request. The application core validates the
// same rules against the input schema first; this keeps a direct caller inside them as well.
func (o *ListOptions) normalize() error {
	if o.Limit == 0 {
		o.Limit = defaultPageSize
	}
	if o.Limit < 1 || o.Limit > maxPageSize {
		return fmt.Errorf("the page size must be between 1 and %d", maxPageSize)
	}
	if o.Sort != "" && sortFields[o.Sort] == "" {
		return errors.New("the sort property is not supported")
	}
	if o.Direction != "" && o.Direction != "asc" && o.Direction != "desc" {
		return errors.New("the sort direction must be asc or desc")
	}
	if len(o.Cursor) > maxCursorLength || !safeCursor(o.Cursor) {
		return errors.New("the cursor is not one this provider handed out")
	}
	if !safeSearchTerm(o.NameContains, searchNameMax) {
		return errors.New("the name filter contains characters a search cannot carry")
	}
	if !safeDomainTerm(o.DomainContains) {
		return errors.New("the domain filter is not a usable domain fragment")
	}
	return nil
}

// GetCompany reads exactly the company of a validated identifier and performs no other provider I/O.
func (c *Client) GetCompany(ctx context.Context, id string) (*Company, error) {
	const op = "get company"
	if !validUUID(id) {
		return nil, providerError(op, "the company identifier must be a UUID")
	}

	query := url.Values{}
	query.Set("depth", noRelations)
	var response companyJSON
	if err := c.get(ctx, op, companiesPath+"/"+url.PathEscape(id), query, maxResponseBytes, &response); err != nil {
		return nil, err
	}
	record := response.Data.Company
	if !strings.EqualFold(record.ID, id) {
		return nil, &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "Twenty answered with a different company than the requested one",
		}
	}
	return &Company{
		ID: record.ID, Name: record.Name, Domain: primaryDomain(record.DomainName),
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

// companyRecordJSON mirrors the Twenty record fields this provider reads. DomainName stays raw because a
// workspace may carry it as a link object or, in an older schema, as plain text.
type companyRecordJSON struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	DomainName json.RawMessage `json:"domainName"`
	CreatedAt  string          `json:"createdAt"`
	UpdatedAt  string          `json:"updatedAt"`
}

type companiesPageJSON struct {
	Data struct {
		Companies []companyRecordJSON `json:"companies"`
	} `json:"data"`
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

type companyJSON struct {
	Data struct {
		Company companyRecordJSON `json:"company"`
	} `json:"data"`
}

// openAPIJSON mirrors the two parts of the generated workspace document the connection test inspects.
type openAPIJSON struct {
	Paths      map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"schemas"`
	} `json:"components"`
}

// primaryDomain reduces the Twenty link field to the one stable string this provider reports. A workspace
// that carries the field as plain text is read as that text.
func primaryDomain(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var link struct {
		PrimaryLinkLabel string `json:"primaryLinkLabel"`
		PrimaryLinkURL   string `json:"primaryLinkUrl"`
	}
	if json.Unmarshal(raw, &link) != nil {
		return ""
	}
	if link.PrimaryLinkLabel != "" {
		return link.PrimaryLinkLabel
	}
	return link.PrimaryLinkURL
}

// get performs one bounded read against the configured origin and decodes the response into out.
func (c *Client) get(ctx context.Context, op, path string, query url.Values, limit int64, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return &provider.Error{
			Class: provider.ClassTimeout, Op: op,
			Message: "the request ended while it waited for the Twenty rate limit",
		}
	}

	target := c.origin + path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return providerError(op, "the request could not be built")
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")

	response, err := c.http.Do(req)
	if err != nil {
		return transportError(op, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return statusError(op, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "the Twenty response could not be read within the size limit",
		}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op, Message: "Twenty returned an invalid response",
		}
	}
	return nil
}

// statusError maps an HTTP status to a stable class. The provider message is never copied: Twenty echoes
// request and record detail into it, and the class plus the status is what a caller can act on.
func statusError(op string, status int) error {
	switch status {
	case http.StatusUnauthorized:
		return &provider.Error{Class: provider.ClassAuth, Op: op, Message: "Twenty rejected the API key"}
	case http.StatusForbidden:
		return &provider.Error{
			Class: provider.ClassPermission, Op: op,
			Message: "the workspace role of this API key may not perform this read",
		}
	case http.StatusNotFound:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op, Message: "this Twenty workspace does not hold this record",
		}
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op, Message: "Twenty rejected the request as invalid",
		}
	case http.StatusTooManyRequests:
		return &provider.Error{
			Class: provider.ClassRateLimited, Op: op, Message: "Twenty rate-limited the operation",
		}
	case http.StatusGatewayTimeout:
		return &provider.Error{Class: provider.ClassTimeout, Op: op, Message: "Twenty did not answer in time"}
	default:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op,
			Message: fmt.Sprintf("Twenty rejected the operation (HTTP %d)", status),
		}
	}
}

// transportError classifies a failure that happened before a status code existed. The original error text
// is not copied, so a URL or a header can never reach the message.
func transportError(op string, err error) error {
	var (
		certErr    *tls.CertificateVerificationError
		hostErr    x509.HostnameError
		authErr    x509.UnknownAuthorityError
		recordErr  tls.RecordHeaderError
		invalidErr x509.CertificateInvalidError
		netErr     net.Error
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return &provider.Error{Class: provider.ClassTimeout, Op: op, Message: "Twenty did not answer in time"}
	case errors.As(err, &certErr), errors.As(err, &hostErr), errors.As(err, &authErr),
		errors.As(err, &recordErr), errors.As(err, &invalidErr):
		return &provider.Error{
			Class: provider.ClassTLS, Op: op, Message: "the TLS connection could not be established",
		}
	default:
		return &provider.Error{Class: provider.ClassUnreachable, Op: op, Message: "Twenty could not be reached"}
	}
}

func providerError(op, message string) error {
	return &provider.Error{Class: provider.ClassProviderError, Op: op, Message: message}
}

// validAPIKey keeps an obviously unusable value out of a request. The real check is the provider's.
func validAPIKey(value string) bool {
	if len(value) < 8 || len(value) > 4096 {
		return false
	}
	for _, r := range value {
		// A header value may not carry control characters, and a Twenty key never does.
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

// validUUID accepts the canonical 8-4-4-4-12 hexadecimal form and nothing else.
func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if b != '-' {
				return false
			}
			continue
		}
		hex := (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
		if !hex {
			return false
		}
	}
	return true
}

// The bounds the search terms share with the input schema.
const (
	searchNameMax   = 64
	searchDomainMax = 253
)

// safeSearchTerm mirrors the schema pattern in Go, so a direct caller cannot smuggle a filter separator
// into the value of a Twenty filter expression either.
func safeSearchTerm(value string, max int) bool {
	if value == "" {
		return true
	}
	if len([]rune(value)) > max {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ', r == '.', r == '_', r == '\'', r == '+', r == '@', r == '&', r == '-':
		case r > 0x7f && (unicode.IsLetter(r) || unicode.IsDigit(r)):
		default:
			return false
		}
	}
	return true
}

func safeDomainTerm(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > searchDomainMax {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// safeCursor accepts the opaque cursor alphabet this provider hands out and nothing else.
func safeCursor(value string) bool {
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '+', r == '/', r == '=', r == '_', r == '.', r == ':', r == '-':
		default:
			return false
		}
	}
	return true
}
