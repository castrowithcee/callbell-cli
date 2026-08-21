// Package seatable implements read-only access to the rows of one fixed SeaTable table.
//
// A SeaTable installation holds many bases, and every base can issue several API tokens with their own
// read or write permission. That cardinality is the configuration: a service is one instance, a credential
// is one API token of one base, and a connection binds them to one fixed table and optionally one fixed
// view. The API token itself never reaches a base route; it is exchanged for a short-lived base token that
// is kept in memory for the current process and never stored.
//
// Column names and cell values arrive from the provider and are treated as untrusted data: they are
// normalised into a stable envelope of identifier, time metadata, and a dynamic value map, passed through
// the output encoders, and never rendered or stored.
package seatable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
const Provider = "seatable"

// cloudOrigin is the managed SeaTable Cloud origin and the default a new service starts from. A dedicated
// or self-hosted installation configures its own HTTPS origin instead; the agent never supplies one.
const cloudOrigin = "https://cloud.seatable.io"

// roleAPIToken is the single secret role a SeaTable credential must supply. It is the API token of exactly
// one base, and for this read-only slice it needs the permission r.
const roleAPIToken = "api-token"

// dataSensitivity classifies results as row content of the configured SeaTable base. It is deliberately
// provider-specific; the architecture defines no global sensitivity taxonomy.
const dataSensitivity = "seatable-base-rows"

// The routes this provider uses. The account route exchanges the API token for a base token; the two base
// routes are served by the API gateway of the same instance.
const (
	baseTokenPath = "/api/v2.1/dtable/app-access-token/"
	gatewayPath   = "/api-gateway/api/v2/dtables/"
	metadataPath  = "/metadata/"
	rowsPath      = "/rows/"
)

// baseTokenLifetime asks for the shortest documented lifetime instead of the three-day default. The token
// lives in this process only, so an hour is more than one command ever needs.
const baseTokenLifetime = "1h"

// convertKeys asks the API gateway for column names rather than internal column keys, so the value map
// carries the names the connection was configured against.
const convertKeys = "true"

// Bounds of one request. The page size stays far below the documented maximum of 1000 rows, and the
// response limits bound one page and the base metadata without inviting an unbounded read.
const (
	defaultPageSize  = 25
	maxPageSize      = 100
	maxStart         = 10000
	maxResponseBytes = 1 << 20
	maxMetadataBytes = 4 << 20
	maxTokenBytes    = 64 << 10
	defaultTimeout   = 30 * time.Second
)

// rowIDLength is the documented length of a SeaTable row identifier.
const rowIDLength = 22

// minInterval spaces requests that share one API token. SeaTable Cloud allows 200 base requests per
// minute, a dedicated or self-hosted server 500; the lower budget is the safe one for both.
const minInterval = 300 * time.Millisecond

// maxHold bounds how long a reported rate-limit reset may delay the next request. A wrong or hostile
// timestamp must not park a command for hours.
const maxHold = time.Minute

// The rate-limit headers the API gateway returns on every base request.
const (
	headerRemaining = "X-Ratelimit-Remaining"
	headerReset     = "X-Ratelimit-Reset"
)

// limiters holds the rate-limit budget of every API token this process has used.
var limiters = ratelimit.NewRegistry(minInterval)

var seatableReadRisk = capability.Risk{
	Effect:          capability.EffectRead,
	Idempotency:     capability.IdempotencySafe,
	Confirmation:    capability.ConfirmationNone,
	OpenWorld:       true,
	DataSensitivity: dataSensitivity,
}

// rowSchema is the stable envelope of one row: the identifier, the time metadata SeaTable maintains, and
// the dynamic column values. The value map is deliberately unconstrained, because every base defines its
// own columns; adopting a base schema as a Callbell contract is not part of this slice.
const rowSchema = `{"type":"object","properties":{` +
	`"id":{"type":"string"},"created_at":{"type":"string"},"updated_at":{"type":"string"},` +
	`"values":{"type":"object"}},"required":["id","values"],"additionalProperties":false}`

var rowsList = capability.Descriptor{
	ID:      Provider + ".rows.list",
	Version: 1,
	Title:   "List SeaTable rows",
	Description: "List one bounded page of rows from the fixed table, or view, of an explicit SeaTable " +
		"connection",
	Tags:                       []string{"seatable", "base", "rows", "list", "table"},
	Risk:                       seatableReadRisk,
	Provider:                   Provider,
	RequiresExplicitConnection: true,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"start":{"type":"integer","minimum":0,"maximum":10000},` +
		`"limit":{"type":"integer","minimum":1,"maximum":100}},` +
		`"additionalProperties":false}`),
	OutputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"rows":{"type":"array","items":` + rowSchema + `},` +
		`"start":{"type":"integer"},"limit":{"type":"integer"},"next_start":{"type":"integer"},` +
		`"has_more":{"type":"boolean"}},` +
		`"required":["rows","start","limit","has_more"],"additionalProperties":false}`),
	Arguments: []capability.Argument{
		{Name: "start", Description: "Zero-based row offset inside the fixed table or view, up to 10000"},
		{Name: "limit", Description: "Rows per page, from 1 through 100; 25 when omitted"},
	},
	Fields: []capability.Field{
		{Name: "rows", Description: "The rows on this page, untrusted data"},
		{Name: "start", Description: "Row offset this page started at"},
		{Name: "limit", Description: "Page size the request applied"},
		{Name: "next_start", Description: "Offset of the following page, absent on the last page"},
		{Name: "has_more", Description: "True when the table or view may hold a following page"},
	},
	Examples: []capability.Example{{
		Description: "Read the first page of the table this connection is bound to",
		Arguments:   json.RawMessage(`{"start":0,"limit":25}`),
	}},
}

var rowsGet = capability.Descriptor{
	ID:      Provider + ".rows.get",
	Version: 1,
	Title:   "Get a SeaTable row",
	Description: "Read one row of the fixed table of an explicit SeaTable connection by its row " +
		"identifier",
	Tags:                       []string{"seatable", "base", "rows", "get", "table"},
	Risk:                       seatableReadRisk,
	Provider:                   Provider,
	RequiresExplicitConnection: true,
	InputSchema: json.RawMessage(`{"type":"object","properties":{"row_id":{"type":"string",` +
		`"minLength":22,"maxLength":22,"pattern":"^[A-Za-z0-9_-]{22}$"}},` +
		`"required":["row_id"],"additionalProperties":false}`),
	OutputSchema: json.RawMessage(rowSchema),
	Arguments: []capability.Argument{
		{Name: "row_id", Description: "Row identifier of 22 characters, as returned by seatable.rows.list", Required: true},
	},
	Fields: []capability.Field{
		{Name: "id", Description: "Row identifier"},
		{Name: "created_at", Description: "Creation time SeaTable keeps for the row"},
		{Name: "updated_at", Description: "Last change time SeaTable keeps for the row"},
		{Name: "values", Description: "Column values of the row by column name, untrusted data"},
	},
	Examples: []capability.Example{{
		Description: "Read one row by the identifier a list result reported",
		Arguments:   json.RawMessage(`{"row_id":"Qtf7xPmoRaiFyQPO1aENTj"}`),
	}},
}

// Register adds SeaTable metadata, its read-only connection test, and the two read operations.
func Register(reg *capability.Registry) error {
	if err := reg.RegisterProvider(config.ProviderMetadata{
		ID: Provider, Name: "SeaTable", DefaultBaseURL: cloudOrigin,
		SecretRoles: []config.SecretRole{{
			Name: roleAPIToken,
			Description: "SeaTable API token of one base: open the base, choose Advanced, API tokens, " +
				"add a token with the permission r for read only; Callbell exchanges it for a " +
				"short-lived base token and never writes",
		}},
		Target: config.TargetMetadata{
			Label:    "table",
			Required: true,
			Description: "fixed table of this base, optionally with a view: TABLE, TABLE/VIEW, or " +
				"id:TABLEID and id:TABLEID/id:VIEWID to address them by identifier",
		},
	}, TestConnection); err != nil {
		return err
	}
	return reg.Register(Provider,
		capability.Operation{Descriptor: rowsList, Handler: capability.Handler(invokeRowsList)},
		capability.Operation{Descriptor: rowsGet, Handler: capability.Handler(invokeRowsGet)},
	)
}

func invokeRowsList(ctx context.Context, resolved *config.Resolved, secrets *secret.Resolver,
	red *redact.Redactor, raw json.RawMessage) (any, error) {
	var options ListOptions
	if err := json.Unmarshal(raw, &options); err != nil {
		return nil, providerError("list rows", "the validated arguments could not be read")
	}
	client, err := Open(resolved, secrets, red)
	if err != nil {
		return nil, err
	}
	return client.ListRows(ctx, options)
}

func invokeRowsGet(ctx context.Context, resolved *config.Resolved, secrets *secret.Resolver,
	red *redact.Redactor, raw json.RawMessage) (any, error) {
	var arguments struct {
		RowID string `json:"row_id"`
	}
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return nil, providerError("get row", "the validated arguments could not be read")
	}
	client, err := Open(resolved, secrets, red)
	if err != nil {
		return nil, err
	}
	return client.GetRow(ctx, arguments.RowID)
}

// target is the fixed table, and optional view, one connection is bound to. Both are configuration: an
// invoke request can name neither, so a connection can never read another table of the base.
type target struct {
	tableParam string
	table      string
	viewParam  string
	view       string
}

// Client binds one API token to the origin of one configured service, to the fixed table of one
// connection, and to the rate limit that token shares.
//
// A client is opened per request by the application core and is never shared between goroutines, so the
// exchanged base token is kept in a plain field.
type Client struct {
	origin   string
	apiToken string
	target   target
	http     *http.Client
	limiter  *ratelimit.Limiter
	redactor *redact.Redactor
	base     *baseAccess
}

// baseAccess is the result of one token exchange: the base the API token belongs to, and the short-lived
// token that authenticates the base routes. Neither value is ever written outside this process.
type baseAccess struct {
	uuid  string
	token string
}

// Open resolves the API token of one selected connection and returns a client for its configured origin.
func Open(resolved *config.Resolved, secrets *secret.Resolver, red *redact.Redactor) (*Client, error) {
	return open(resolved, secrets, red, nil)
}

// open is the internal seam. A caller may supply the rate limiter, and the package's own tests replace
// the transport, so no test ever reaches a productive SeaTable installation.
func open(resolved *config.Resolved, secrets *secret.Resolver, red *redact.Redactor,
	lim *ratelimit.Limiter) (*Client, error) {
	if resolved == nil {
		return nil, providerError("open", "no connection was selected")
	}
	origin, err := originOf(resolved.BaseURL)
	if err != nil {
		return nil, providerError("open", err.Error())
	}
	bound, err := parseTarget(resolved.Target)
	if err != nil {
		return nil, providerError("open", err.Error())
	}
	if secrets == nil {
		return nil, providerError("open", "no credential resolver was configured")
	}
	value, err := secrets.Resolve(resolved.Credential, resolved.Secrets, roleAPIToken)
	if err != nil {
		return nil, err
	}
	if !validToken(value.Secret, 8) {
		return nil, &provider.Error{
			Class: provider.ClassAuth, Op: "open", Message: "the SeaTable API token is unusable",
		}
	}
	if red != nil {
		red.Add(value.Secret, "Bearer "+value.Secret)
	}
	if lim == nil {
		lim = limiters.For(value.Secret)
	}
	return &Client{
		origin: origin, apiToken: value.Secret, target: bound, http: newHTTPClient(),
		limiter: lim, redactor: red,
	}, nil
}

// originOf validates the configured service origin and returns it without a trailing slash. Cloud,
// dedicated, and self-hosted instances are all just origins here: HTTPS, a host, and nothing else.
// Userinfo, a path, a query, or a fragment would either carry a credential or silently rewrite every
// request path.
func originOf(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("a SeaTable service needs a usable https origin")
	}
	if parsed.Scheme != "https" {
		return "", errors.New("a SeaTable service must use https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		strings.Trim(parsed.Path, "/") != "" || parsed.Opaque != "" {
		return "", errors.New("a SeaTable service must be a bare https origin without user, path, query, or fragment")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// The bounds of a configured target. They keep an unusable value out of a request; whether the table and
// the view exist is answered by the connection test against the base metadata.
const (
	maxTargetLength = 512
	maxNameLength   = 255
	maxIDLength     = 32
)

// idPrefix marks the identifier form of a table or a view inside a target.
const idPrefix = "id:"

// parseTarget reads the fixed table, and optional view, of one connection. The form is TABLE, TABLE/VIEW,
// or the identifier form id:TABLEID and id:TABLEID/id:VIEWID.
//
// callbell-dev: a table or view whose name contains a slash has to be addressed by its identifier,
// because the separator is what keeps the target one configuration field.
func parseTarget(raw string) (target, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return target{}, errors.New("a SeaTable connection needs a fixed table as its target")
	}
	if len(trimmed) > maxTargetLength {
		return target{}, errors.New("the configured SeaTable target is too long")
	}
	tablePart, viewPart, hasView := strings.Cut(trimmed, "/")

	tableRef, byID, err := parseReference(tablePart)
	if err != nil {
		return target{}, fmt.Errorf("the configured SeaTable table is unusable: %w", err)
	}
	bound := target{tableParam: "table_name", table: tableRef}
	if byID {
		bound.tableParam = "table_id"
	}
	if !hasView {
		return bound, nil
	}

	viewRef, byID, err := parseReference(viewPart)
	if err != nil {
		return target{}, fmt.Errorf("the configured SeaTable view is unusable: %w", err)
	}
	bound.viewParam, bound.view = "view_name", viewRef
	if byID {
		bound.viewParam = "view_id"
	}
	return bound, nil
}

// parseReference reads one table or view reference and reports whether it is an identifier.
func parseReference(part string) (string, bool, error) {
	trimmed := strings.TrimSpace(part)
	if identifier, ok := strings.CutPrefix(trimmed, idPrefix); ok {
		if !validID(identifier) {
			return "", false, errors.New("an identifier must be 1 to 32 letters, digits, '-' or '_'")
		}
		return identifier, true, nil
	}
	if !validName(trimmed) {
		return "", false, errors.New("a name must be 1 to 255 printable characters")
	}
	return trimmed, false, nil
}

func validID(value string) bool {
	if value == "" || len(value) > maxIDLength {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func validName(value string) bool {
	if value == "" || len([]rune(value)) > maxNameLength {
		return false
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// transport carries every SeaTable request. A nil value is Go's default transport; the package's own
// tests replace it with recorded responses.
var transport http.RoundTripper

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   defaultTimeout,
		Transport: transport,
		// A token travels in the Authorization header, so no redirect is followed: a redirect could only
		// move a credential to an origin the user never configured.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// TestConnection verifies that the connection can serve this provider at all: the API token must be
// exchangeable for a base token of the configured instance, and the base must still hold the fixed table
// and, when configured, the fixed view. The check reads metadata only; it never writes, so a token with a
// wider permission is still only read from here.
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

	if err := c.checkTarget(ctx, op); err != nil {
		var providerErr *provider.Error
		if !errors.As(err, &providerErr) {
			return provider.ClassProviderError, nil
		}
		// A base without the configured table or view is reported with its own explanation, because no
		// stable class can say which part of the target is missing.
		if providerErr.Class == provider.ClassInvalidResponse {
			return "", err
		}
		return providerErr.Class, nil
	}
	return provider.ClassOK, nil
}

// checkTarget reads the metadata of the base the API token belongs to and verifies the fixed target. The
// metadata itself is never reported.
func (c *Client) checkTarget(ctx context.Context, op string) error {
	access, err := c.access(ctx, op)
	if err != nil {
		return err
	}

	var document metadataJSON
	if err := c.get(ctx, op, gatewayPath+url.PathEscape(access.uuid)+metadataPath, nil,
		access.token, maxMetadataBytes, &document); err != nil {
		return err
	}

	for _, table := range document.Metadata.Tables {
		if !c.matchesTable(table) {
			continue
		}
		if c.target.view == "" {
			return nil
		}
		for _, view := range table.Views {
			if c.target.viewParam == "view_id" && view.ID == c.target.view {
				return nil
			}
			if c.target.viewParam == "view_name" && view.Name == c.target.view {
				return nil
			}
		}
		return &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "this SeaTable base does not hold the view this connection is bound to",
		}
	}
	return &provider.Error{
		Class: provider.ClassInvalidResponse, Op: op,
		Message: "this SeaTable base does not hold the table this connection is bound to",
	}
}

func (c *Client) matchesTable(table tableJSON) bool {
	if c.target.tableParam == "table_id" {
		return table.ID == c.target.table
	}
	return table.Name == c.target.table
}

// access returns the base this API token belongs to, exchanging the token on first use. The base token is
// short-lived, request-bound, and never leaves the process.
func (c *Client) access(ctx context.Context, op string) (*baseAccess, error) {
	if c.base != nil {
		return c.base, nil
	}

	query := url.Values{}
	query.Set("exp", baseTokenLifetime)
	var exchanged baseTokenJSON
	if err := c.get(ctx, op, baseTokenPath, query, c.apiToken, maxTokenBytes, &exchanged); err != nil {
		return nil, err
	}
	if !validToken(exchanged.AccessToken, 16) {
		return nil, &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "SeaTable answered the token exchange without a usable base token",
		}
	}
	if !validUUID(exchanged.DTableUUID) {
		return nil, &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "SeaTable answered the token exchange without a usable base identifier",
		}
	}
	// The server URL of the answer is not followed; it only has to agree with the configured instance.
	// A different origin would mean this API token belongs somewhere else.
	if err := c.checkServer(op, exchanged.DTableServer); err != nil {
		return nil, err
	}
	if c.redactor != nil {
		c.redactor.Add(exchanged.AccessToken, "Bearer "+exchanged.AccessToken)
	}
	c.base = &baseAccess{uuid: exchanged.DTableUUID, token: exchanged.AccessToken}
	return c.base, nil
}

func (c *Client) checkServer(op, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.Scheme+"://"+parsed.Host != c.origin {
		return &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "this API token belongs to a different SeaTable server than the configured service",
		}
	}
	return nil
}

// ListOptions are the controlled paging arguments of the list operation. Nothing else reaches the
// provider: the base, the table, the view, and the read-only nature are fixed.
type ListOptions struct {
	Start int `json:"start"`
	Limit int `json:"limit"`
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
	if o.Start < 0 || o.Start > maxStart {
		return fmt.Errorf("the row offset must be between 0 and %d", maxStart)
	}
	return nil
}

// Row is the stable Callbell view of one row: the identifier, the time metadata SeaTable maintains, and
// the column values of the base. Values keeps the raw JSON of every column, so a cell is reported as the
// provider sent it without this package interpreting a base-specific column type.
type Row struct {
	ID        string                     `json:"id"`
	CreatedAt string                     `json:"created_at,omitempty"`
	UpdatedAt string                     `json:"updated_at,omitempty"`
	Values    map[string]json.RawMessage `json:"values"`
}

// ListResult is the normalised page of rows.
type ListResult struct {
	Rows      []Row `json:"rows"`
	Start     int   `json:"start"`
	Limit     int   `json:"limit"`
	NextStart int   `json:"next_start,omitempty"`
	HasMore   bool  `json:"has_more"`
}

// ListRows reads exactly one bounded page of the fixed table or view.
func (c *Client) ListRows(ctx context.Context, options ListOptions) (*ListResult, error) {
	const op = "list rows"
	if err := options.normalize(); err != nil {
		return nil, providerError(op, err.Error())
	}
	access, err := c.access(ctx, op)
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	query.Set(c.target.tableParam, c.target.table)
	if c.target.view != "" {
		query.Set(c.target.viewParam, c.target.view)
	}
	query.Set("start", strconv.Itoa(options.Start))
	query.Set("limit", strconv.Itoa(options.Limit))
	query.Set("convert_keys", convertKeys)

	var page rowsPageJSON
	if err := c.get(ctx, op, gatewayPath+url.PathEscape(access.uuid)+rowsPath, query,
		access.token, maxResponseBytes, &page); err != nil {
		return nil, err
	}
	if len(page.Rows) > options.Limit {
		return nil, &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "SeaTable returned more rows than the requested page allows",
		}
	}

	rows := make([]Row, 0, len(page.Rows))
	for _, raw := range page.Rows {
		row, err := normalizeRow(op, raw)
		if err != nil {
			return nil, err
		}
		rows = append(rows, *row)
	}

	result := &ListResult{
		Rows: rows, Start: options.Start, Limit: options.Limit,
		HasMore: len(rows) == options.Limit,
	}
	if result.HasMore {
		result.NextStart = options.Start + len(rows)
	}
	return result, nil
}

// GetRow reads exactly the row of a validated identifier from the fixed table and performs no other
// provider I/O. A configured view narrows the list operation; the identifier addresses the table itself.
func (c *Client) GetRow(ctx context.Context, rowID string) (*Row, error) {
	const op = "get row"
	if !validRowID(rowID) {
		return nil, providerError(op, "a SeaTable row identifier has 22 letters, digits, '-' or '_'")
	}
	access, err := c.access(ctx, op)
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	query.Set(c.target.tableParam, c.target.table)
	query.Set("convert_keys", convertKeys)

	var raw map[string]json.RawMessage
	path := gatewayPath + url.PathEscape(access.uuid) + rowsPath + url.PathEscape(rowID) + "/"
	if err := c.get(ctx, op, path, query, access.token, maxResponseBytes, &raw); err != nil {
		return nil, err
	}
	row, err := normalizeRow(op, raw)
	if err != nil {
		return nil, err
	}
	if row.ID != rowID {
		return nil, &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "SeaTable answered with a different row than the requested one",
		}
	}
	return row, nil
}

// The system keys SeaTable adds to every row. They become the stable envelope; every other key is a
// column of the base and stays in the value map.
const (
	systemPrefix = "_"
	keyID        = "_id"
	keyCreated   = "_ctime"
	keyUpdated   = "_mtime"
)

// normalizeRow splits one raw row into the stable envelope and the dynamic column values. Every other
// system key is dropped: it describes the base, not the row a caller asked for.
func normalizeRow(op string, raw map[string]json.RawMessage) (*Row, error) {
	row := &Row{Values: map[string]json.RawMessage{}}
	for key, value := range raw {
		switch key {
		case keyID:
			row.ID = decodeString(value)
		case keyCreated:
			row.CreatedAt = decodeString(value)
		case keyUpdated:
			row.UpdatedAt = decodeString(value)
		default:
			if strings.HasPrefix(key, systemPrefix) {
				continue
			}
			row.Values[key] = value
		}
	}
	if row.ID == "" || len(row.ID) > maxIDLength {
		return nil, &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "SeaTable returned a row without a usable identifier",
		}
	}
	return row, nil
}

func decodeString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// rowsPageJSON mirrors the one field the list route answers with.
type rowsPageJSON struct {
	Rows []map[string]json.RawMessage `json:"rows"`
}

// baseTokenJSON mirrors the fields of the token exchange this provider uses. The remaining fields of the
// answer describe the workspace and are not needed for a read.
type baseTokenJSON struct {
	AccessToken  string `json:"access_token"`
	DTableUUID   string `json:"dtable_uuid"`
	DTableServer string `json:"dtable_server"`
}

// metadataJSON mirrors the part of the base metadata the connection test inspects.
type metadataJSON struct {
	Metadata struct {
		Tables []tableJSON `json:"tables"`
	} `json:"metadata"`
}

type tableJSON struct {
	ID    string `json:"_id"`
	Name  string `json:"name"`
	Views []struct {
		ID   string `json:"_id"`
		Name string `json:"name"`
	} `json:"views"`
}

// get performs one bounded read against the configured origin and decodes the response into out. The
// bearer token is chosen by the caller: the API token for the exchange, the base token for a base route.
func (c *Client) get(ctx context.Context, op, path string, query url.Values, token string,
	limit int64, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return &provider.Error{
			Class: provider.ClassTimeout, Op: op,
			Message: "the request ended while it waited for the SeaTable rate limit",
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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	response, err := c.http.Do(req)
	if err != nil {
		return transportError(op, err)
	}
	defer response.Body.Close()
	c.observeRateLimit(response.Header)

	if response.StatusCode != http.StatusOK {
		return statusError(op, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "the SeaTable response could not be read within the size limit",
		}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op, Message: "SeaTable returned an invalid response",
		}
	}
	return nil
}

// observeRateLimit reads the budget headers of the API gateway. When the budget of this token is spent,
// the next request waits for the reported reset instead of running into a refusal.
func (c *Client) observeRateLimit(header http.Header) {
	remaining, err := strconv.Atoi(strings.TrimSpace(header.Get(headerRemaining)))
	if err != nil || remaining > 0 {
		return
	}
	reset, err := strconv.ParseInt(strings.TrimSpace(header.Get(headerReset)), 10, 64)
	if err != nil {
		return
	}
	hold := time.Until(time.Unix(reset, 0))
	if hold > maxHold {
		hold = maxHold
	}
	c.limiter.HoldFor(hold)
}

// statusError maps an HTTP status to a stable class. The provider message is never copied: SeaTable
// echoes request and base detail into it, and the class plus the status is what a caller can act on.
func statusError(op string, status int) error {
	switch status {
	case http.StatusUnauthorized:
		return &provider.Error{Class: provider.ClassAuth, Op: op, Message: "SeaTable rejected the token"}
	case http.StatusForbidden:
		return &provider.Error{
			Class: provider.ClassPermission, Op: op,
			Message: "this SeaTable API token may not perform this read",
		}
	case http.StatusNotFound:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op,
			Message: "this SeaTable base does not hold this table or row",
		}
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op, Message: "SeaTable rejected the request as invalid",
		}
	case http.StatusTooManyRequests:
		return &provider.Error{
			Class: provider.ClassRateLimited, Op: op, Message: "SeaTable rate-limited the operation",
		}
	case http.StatusGatewayTimeout:
		return &provider.Error{Class: provider.ClassTimeout, Op: op, Message: "SeaTable did not answer in time"}
	default:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op,
			Message: fmt.Sprintf("SeaTable rejected the operation (HTTP %d)", status),
		}
	}
}

// transportError classifies a failure that happened before a status code existed. The shared classifier
// owns the rules, so SeaTable publishes the same class and the same transport cause as every other
// provider, and the original error text is never copied.
func transportError(op string, err error) error {
	return provider.Transport(op, "SeaTable", err)
}

func providerError(op, message string) error {
	return &provider.Error{Class: provider.ClassProviderError, Op: op, Message: message}
}

// validToken keeps an obviously unusable value out of a header. The real check is the provider's.
func validToken(value string, min int) bool {
	if len(value) < min || len(value) > 8192 {
		return false
	}
	for _, r := range value {
		// A header value may not carry control characters, and a SeaTable token never does.
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

// validUUID accepts the base identifier in the canonical 8-4-4-4-12 form and in the plain hexadecimal
// form SeaTable also reports, and nothing else.
func validUUID(value string) bool {
	switch len(value) {
	case 32:
		return hexOnly(value)
	case 36:
		for i := 0; i < len(value); i++ {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				if value[i] != '-' {
					return false
				}
				continue
			}
			if !hexOnly(value[i : i+1]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func hexOnly(value string) bool {
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F') {
			continue
		}
		return false
	}
	return true
}

// validRowID mirrors the schema pattern in Go, so a direct caller cannot address a row outside the
// documented identifier form either.
func validRowID(value string) bool {
	if len(value) != rowIDLength {
		return false
	}
	return validID(value)
}
