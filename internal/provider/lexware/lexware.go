// Package lexware implements read-only access to the open and overdue outgoing invoices of a Lexware
// Office organization.
//
// The provider talks to one fixed production gateway and offers exactly two safe reads: a bounded page of
// invoice metadata and the detail of one invoice selected by its validated identifier. Contact, address,
// and line-item content arrives from the provider and is treated as untrusted data: it is normalised into
// a stable Callbell shape, passed through the output encoders, and never rendered or stored.
package lexware

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

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/provider/ratelimit"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// Provider is the provider name used in the configuration and as the operation namespace.
const Provider = "lexware"

// gateway is the fixed production API gateway. The provider never accepts a configured URL: a Lexware API
// key is bound to this one origin, so a second origin could only be a mistake or an exfiltration route.
const gateway = "https://api.lexware.io"

// roleAPIKey is the single secret role a Lexware credential must supply. It is used as a bearer token.
const roleAPIKey = "api-key"

// dataSensitivity classifies results as accounting data of the configured Lexware organization. It is
// deliberately provider-specific; the architecture defines no global sensitivity taxonomy.
const dataSensitivity = "lexware-invoice-data"

// The fixed voucher filters of the confirmed read-only workflow. An agent cannot widen them: the list
// operation is about open outgoing invoices, not about the whole voucher list.
const (
	fixedVoucherType   = "invoice"
	fixedVoucherStatus = "open"
	fixedArchived      = "false"
)

// Bounds of one request. The page size stays well below the documented maximum of 250, and the response
// limit bounds an invoice with many line items without inviting an unbounded read.
const (
	defaultPageSize  = 25
	maxPageSize      = 100
	maxPage          = 200
	maxResponseBytes = 1 << 20
	defaultTimeout   = 30 * time.Second
)

// minInterval spaces requests that share one API key. Lexware allows two requests per second across all
// of its endpoints, and counts them per key, so the spacing belongs to the key.
const minInterval = 500 * time.Millisecond

// limiters holds the rate-limit budget of every API key this process has used.
var limiters = ratelimit.NewRegistry(minInterval)

var lexwareReadRisk = capability.Risk{
	Effect:          capability.EffectRead,
	Idempotency:     capability.IdempotencySafe,
	Confirmation:    capability.ConfirmationNone,
	OpenWorld:       true,
	DataSensitivity: dataSensitivity,
}

var invoicesList = capability.Descriptor{
	ID:      Provider + ".invoices.list",
	Version: 1,
	Title:   "List open Lexware invoices",
	Description: "List one page of open outgoing invoices of an explicit Lexware Office connection; " +
		"Lexware also reports an invoice whose due date has passed with the transient status overdue",
	Tags:                       []string{"lexware", "invoices", "list", "open", "overdue", "accounting"},
	Risk:                       lexwareReadRisk,
	Provider:                   Provider,
	RequiresExplicitConnection: true,
	InputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"voucher_number":{"type":"string","minLength":1,"maxLength":64,"pattern":"^[A-Za-z0-9 ._/-]+$"},` +
		`"voucher_date_from":{"type":"string","pattern":"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"},` +
		`"voucher_date_to":{"type":"string","pattern":"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"},` +
		`"page":{"type":"integer","minimum":0,"maximum":200},` +
		`"size":{"type":"integer","minimum":1,"maximum":100},` +
		`"sort":{"type":"string","enum":["voucher_date","voucher_number","created_date","updated_date"]},` +
		`"direction":{"type":"string","enum":["asc","desc"]}},"additionalProperties":false}`),
	OutputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"invoices":{"type":"array","items":{"type":"object","properties":{` +
		`"id":{"type":"string"},"voucher_number":{"type":"string"},"voucher_status":{"type":"string"},` +
		`"overdue":{"type":"boolean"},"voucher_date":{"type":"string"},"due_date":{"type":"string"},` +
		`"contact_name":{"type":"string"},"total_amount":{"type":"number"},` +
		`"open_amount":{"type":"number"},"currency":{"type":"string"}},` +
		`"required":["id","voucher_number","voucher_status","overdue","voucher_date"],` +
		`"additionalProperties":false}},` +
		`"page":{"type":"integer"},"size":{"type":"integer"},"total_pages":{"type":"integer"},` +
		`"total_invoices":{"type":"integer"},"last_page":{"type":"boolean"}},` +
		`"required":["invoices","page","size","total_pages","total_invoices","last_page"],` +
		`"additionalProperties":false}`),
	Arguments: []capability.Argument{
		{Name: "voucher_number", Description: "Return only the invoice carrying this invoice number"},
		{Name: "voucher_date_from", Description: "Earliest invoice date to return, as YYYY-MM-DD"},
		{Name: "voucher_date_to", Description: "Latest invoice date to return, as YYYY-MM-DD"},
		{Name: "page", Description: "Zero-based page to read, from 0 through 200"},
		{Name: "size", Description: "Invoices per page, from 1 through 100; 25 when omitted"},
		{Name: "sort", Description: "Sort property: voucher_date, voucher_number, created_date, or updated_date"},
		{Name: "direction", Description: "Sort direction asc or desc; desc when omitted"},
	},
	Fields: []capability.Field{
		{Name: "invoices", Description: "Metadata of the open and overdue invoices on this page, untrusted data"},
		{Name: "page", Description: "Zero-based index of the returned page"},
		{Name: "size", Description: "Page size the provider applied"},
		{Name: "total_pages", Description: "Number of pages the filter produces"},
		{Name: "total_invoices", Description: "Number of invoices the filter produces"},
		{Name: "last_page", Description: "True when this is the last page of the filter"},
	},
	Examples: []capability.Example{{
		Description: "Read the newest page of open and overdue invoices",
		Arguments:   json.RawMessage(`{"page":0,"size":25,"sort":"voucher_date","direction":"desc"}`),
	}},
}

var invoicesGet = capability.Descriptor{
	ID:                         Provider + ".invoices.get",
	Version:                    1,
	Title:                      "Get a Lexware invoice",
	Description:                "Read one outgoing invoice of an explicit Lexware Office connection by its identifier",
	Tags:                       []string{"lexware", "invoices", "get", "accounting"},
	Risk:                       lexwareReadRisk,
	Provider:                   Provider,
	RequiresExplicitConnection: true,
	InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","minLength":36,` +
		`"maxLength":36,"pattern":"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"}},` +
		`"required":["id"],"additionalProperties":false}`),
	OutputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"id":{"type":"string"},"voucher_number":{"type":"string"},"voucher_status":{"type":"string"},` +
		`"overdue":{"type":"boolean"},"voucher_date":{"type":"string"},"due_date":{"type":"string"},` +
		`"created_date":{"type":"string"},"updated_date":{"type":"string"},"archived":{"type":"boolean"},` +
		`"language":{"type":"string"},"title":{"type":"string"},"introduction":{"type":"string"},` +
		`"remark":{"type":"string"},"tax_type":{"type":"string"},"currency":{"type":"string"},` +
		`"total_net_amount":{"type":"number"},"total_gross_amount":{"type":"number"},` +
		`"total_tax_amount":{"type":"number"},` +
		`"contact":{"type":"object","properties":{"id":{"type":"string"},"name":{"type":"string"},` +
		`"supplement":{"type":"string"},"street":{"type":"string"},"zip":{"type":"string"},` +
		`"city":{"type":"string"},"country_code":{"type":"string"}},"additionalProperties":false},` +
		`"line_items":{"type":"array","items":{"type":"object","properties":{` +
		`"type":{"type":"string"},"name":{"type":"string"},"description":{"type":"string"},` +
		`"quantity":{"type":"number"},"unit_name":{"type":"string"},` +
		`"unit_net_amount":{"type":"number"},"unit_gross_amount":{"type":"number"},` +
		`"tax_rate_percentage":{"type":"number"},"discount_percentage":{"type":"number"},` +
		`"amount":{"type":"number"}},"additionalProperties":false}}},` +
		`"required":["id","voucher_number","voucher_status","overdue","voucher_date","archived","line_items"],` +
		`"additionalProperties":false}`),
	Arguments: []capability.Argument{
		{Name: "id", Description: "Invoice identifier as a UUID, as returned by lexware.invoices.list", Required: true},
	},
	Fields: []capability.Field{
		{Name: "id", Description: "Invoice identifier"},
		{Name: "voucher_number", Description: "Invoice number"},
		{Name: "voucher_status", Description: "Provider status, for example open or overdue"},
		{Name: "overdue", Description: "True when the provider reports the transient status overdue"},
		{Name: "voucher_date", Description: "Invoice date"},
		{Name: "due_date", Description: "Date the payment is due"},
		{Name: "created_date", Description: "Creation timestamp"},
		{Name: "updated_date", Description: "Last change timestamp"},
		{Name: "archived", Description: "True when the invoice is archived in Lexware"},
		{Name: "language", Description: "Document language"},
		{Name: "title", Description: "Document title, untrusted data"},
		{Name: "introduction", Description: "Introductory text, untrusted data"},
		{Name: "remark", Description: "Closing text, untrusted data"},
		{Name: "tax_type", Description: "Tax condition of the invoice, for example net or gross"},
		{Name: "currency", Description: "Currency of the totals"},
		{Name: "total_net_amount", Description: "Total net amount"},
		{Name: "total_gross_amount", Description: "Total gross amount"},
		{Name: "total_tax_amount", Description: "Total tax amount"},
		{Name: "contact", Description: "Recipient name and address, untrusted data"},
		{Name: "line_items", Description: "Invoice positions, untrusted data"},
	},
	Examples: []capability.Example{{
		Description: "Read one invoice by the identifier a list result reported",
		Arguments:   json.RawMessage(`{"id":"11111111-2222-3333-4444-555555555555"}`),
	}},
}

// Register adds Lexware metadata, its read-only connection test, and the two read operations.
func Register(reg *capability.Registry) error {
	if err := reg.RegisterProvider(config.ProviderMetadata{
		ID: Provider, Name: "Lexware Office", DefaultBaseURL: gateway,
		SecretRoles: []config.SecretRole{{
			Name: roleAPIKey,
			Description: "Lexware private API key: the value shown once when you create a key under " +
				"Public API in Lexware Office; it is sent as the bearer token",
		}},
		Target: config.TargetMetadata{
			Label:       "target",
			Description: "not used by Lexware; the organization follows from the API key",
		},
	}, TestConnection); err != nil {
		return err
	}
	return reg.Register(Provider,
		capability.Operation{Descriptor: invoicesList, Handler: capability.Handler(invokeInvoicesList)},
		capability.Operation{Descriptor: invoicesGet, Handler: capability.Handler(invokeInvoicesGet)},
	)
}

func invokeInvoicesList(ctx context.Context, resolved *config.Resolved, secrets *secret.Resolver,
	red *redact.Redactor, raw json.RawMessage) (any, error) {
	var options ListOptions
	if err := json.Unmarshal(raw, &options); err != nil {
		return nil, providerError("list invoices", "the validated arguments could not be read")
	}
	client, err := Open(resolved, secrets, red)
	if err != nil {
		return nil, err
	}
	return client.ListInvoices(ctx, options)
}

func invokeInvoicesGet(ctx context.Context, resolved *config.Resolved, secrets *secret.Resolver,
	red *redact.Redactor, raw json.RawMessage) (any, error) {
	var arguments struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return nil, providerError("get invoice", "the validated arguments could not be read")
	}
	client, err := Open(resolved, secrets, red)
	if err != nil {
		return nil, err
	}
	return client.GetInvoice(ctx, arguments.ID)
}

// Client binds one Lexware API key to the fixed gateway and to the rate limit that key shares.
type Client struct {
	auth    string
	http    *http.Client
	limiter *ratelimit.Limiter
}

// Open resolves the API key of one selected connection and returns a client for the fixed gateway.
func Open(resolved *config.Resolved, secrets *secret.Resolver, red *redact.Redactor) (*Client, error) {
	return open(resolved, secrets, red, nil)
}

// open is the internal seam. A caller may supply the rate limiter, and the package's own tests replace
// the transport, so no test ever reaches a productive Lexware organization.
func open(resolved *config.Resolved, secrets *secret.Resolver, red *redact.Redactor,
	lim *ratelimit.Limiter) (*Client, error) {
	if resolved == nil {
		return nil, providerError("open", "no connection was selected")
	}
	if !isGateway(resolved.BaseURL) {
		return nil, providerError("open",
			"a Lexware service must use the fixed API gateway "+gateway)
	}
	if secrets == nil {
		return nil, providerError("open", "no credential resolver was configured")
	}
	value, err := secrets.Resolve(resolved.Credential, resolved.Secrets, roleAPIKey)
	if err != nil {
		return nil, err
	}
	if !validAPIKey(value.Secret) {
		return nil, &provider.Error{Class: provider.ClassAuth, Op: "open", Message: "the Lexware API key is unusable"}
	}
	if red != nil {
		red.Add(value.Secret, "Bearer "+value.Secret)
	}
	if lim == nil {
		lim = limiters.For(value.Secret)
	}
	return &Client{auth: "Bearer " + value.Secret, http: newHTTPClient(), limiter: lim}, nil
}

// isGateway reports whether a configured base URL names the fixed production gateway. A trailing slash is
// the only variation a configuration file may carry.
func isGateway(raw string) bool {
	return strings.TrimRight(strings.TrimSpace(raw), "/") == gateway
}

// transport carries every Lexware request. A nil value is Go's default transport; the package's own tests
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

// TestConnection performs the smallest authenticated read of the confirmed workflow and reports the
// stable outcome class. It reads one invoice metadata record and nothing else.
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
	return client.testConnection(ctx), nil
}

func (c *Client) testConnection(ctx context.Context) provider.Class {
	var page voucherListJSON
	if err := c.get(ctx, "test connection", "/v1/voucherlist", listQuery(ListOptions{Size: 1}), &page); err != nil {
		var providerErr *provider.Error
		if errors.As(err, &providerErr) {
			return providerErr.Class
		}
		return provider.ClassProviderError
	}
	return provider.ClassOK
}

// ListOptions are the controlled filters of the list operation. The voucher type, the voucher status, and
// the archived flag are not part of it: they are fixed by the provider.
type ListOptions struct {
	VoucherNumber   string `json:"voucher_number"`
	VoucherDateFrom string `json:"voucher_date_from"`
	VoucherDateTo   string `json:"voucher_date_to"`
	Page            int    `json:"page"`
	Size            int    `json:"size"`
	Sort            string `json:"sort"`
	Direction       string `json:"direction"`
}

// sortProperties maps the stable Callbell sort names to the provider's property names. An unknown name
// never reaches the provider.
var sortProperties = map[string]string{
	"voucher_date":   "voucherDate",
	"voucher_number": "voucherNumber",
	"created_date":   "createdDate",
	"updated_date":   "updatedDate",
}

// listQuery builds the complete query of one list request. Exactly the fixed filters, the requested page,
// and the controlled filters appear in it.
func listQuery(options ListOptions) url.Values {
	query := url.Values{}
	query.Set("voucherType", fixedVoucherType)
	query.Set("voucherStatus", fixedVoucherStatus)
	query.Set("archived", fixedArchived)
	query.Set("page", strconv.Itoa(options.Page))
	query.Set("size", strconv.Itoa(options.Size))

	property := sortProperties[options.Sort]
	if property == "" {
		property = sortProperties["voucher_date"]
	}
	direction := "DESC"
	if options.Direction == "asc" {
		direction = "ASC"
	}
	query.Set("sort", property+","+direction)

	if options.VoucherNumber != "" {
		query.Set("voucherNumber", options.VoucherNumber)
	}
	if options.VoucherDateFrom != "" {
		query.Set("voucherDateFrom", options.VoucherDateFrom)
	}
	if options.VoucherDateTo != "" {
		query.Set("voucherDateTo", options.VoucherDateTo)
	}
	return query
}

// ListResult is the normalised page of invoice metadata.
type ListResult struct {
	Invoices      []ListInvoice `json:"invoices"`
	Page          int           `json:"page"`
	Size          int           `json:"size"`
	TotalPages    int           `json:"total_pages"`
	TotalInvoices int           `json:"total_invoices"`
	LastPage      bool          `json:"last_page"`
}

// ListInvoice is the stable Callbell view of one voucher list record. An amount Lexware does not report
// stays absent instead of being invented.
type ListInvoice struct {
	ID            string      `json:"id"`
	VoucherNumber string      `json:"voucher_number"`
	VoucherStatus string      `json:"voucher_status"`
	Overdue       bool        `json:"overdue"`
	VoucherDate   string      `json:"voucher_date"`
	DueDate       string      `json:"due_date,omitempty"`
	ContactName   string      `json:"contact_name,omitempty"`
	TotalAmount   json.Number `json:"total_amount,omitempty"`
	OpenAmount    json.Number `json:"open_amount,omitempty"`
	Currency      string      `json:"currency,omitempty"`
}

// ListInvoices reads exactly one page of open outgoing invoices. Lexware answers the fixed status filter
// with open invoices and, for an invoice whose due date has passed, with the transient status overdue;
// both belong to the workflow and are kept, with overdue made explicit.
func (c *Client) ListInvoices(ctx context.Context, options ListOptions) (*ListResult, error) {
	const op = "list invoices"
	if err := options.normalize(); err != nil {
		return nil, providerError(op, err.Error())
	}

	var page voucherListJSON
	if err := c.get(ctx, op, "/v1/voucherlist", listQuery(options), &page); err != nil {
		return nil, err
	}

	invoices := make([]ListInvoice, 0, len(page.Content))
	for _, entry := range page.Content {
		// The filters are the provider's job, but its answer is untrusted: a record outside the
		// confirmed workflow is dropped rather than reported as an open outgoing invoice.
		if entry.VoucherType != fixedVoucherType || entry.Archived {
			continue
		}
		if !validUUID(entry.ID) || entry.VoucherNumber == "" {
			return nil, &provider.Error{
				Class: provider.ClassInvalidResponse, Op: op,
				Message: "Lexware returned an invoice without a usable identifier",
			}
		}
		invoices = append(invoices, ListInvoice{
			ID: entry.ID, VoucherNumber: entry.VoucherNumber, VoucherStatus: entry.VoucherStatus,
			Overdue: entry.VoucherStatus == "overdue", VoucherDate: entry.VoucherDate,
			DueDate: entry.DueDate, ContactName: entry.ContactName, TotalAmount: entry.TotalAmount,
			OpenAmount: entry.OpenAmount, Currency: entry.Currency,
		})
	}

	return &ListResult{
		Invoices: invoices, Page: page.Number, Size: page.Size, TotalPages: page.TotalPages,
		TotalInvoices: page.TotalElements, LastPage: page.Last,
	}, nil
}

// normalize applies the defaults and the bounds of one list request. The application core validates the
// same rules against the input schema first; this keeps a direct caller inside them as well.
func (o *ListOptions) normalize() error {
	if o.Size == 0 {
		o.Size = defaultPageSize
	}
	if o.Size < 1 || o.Size > maxPageSize {
		return fmt.Errorf("the page size must be between 1 and %d", maxPageSize)
	}
	if o.Page < 0 || o.Page > maxPage {
		return fmt.Errorf("the page must be between 0 and %d", maxPage)
	}
	if o.Sort != "" && sortProperties[o.Sort] == "" {
		return errors.New("the sort property is not supported")
	}
	if o.Direction != "" && o.Direction != "asc" && o.Direction != "desc" {
		return errors.New("the sort direction must be asc or desc")
	}
	for _, date := range []string{o.VoucherDateFrom, o.VoucherDateTo} {
		if date == "" {
			continue
		}
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			return errors.New("a voucher date filter must use the format YYYY-MM-DD")
		}
	}
	if len(o.VoucherNumber) > 64 {
		return errors.New("the voucher number filter is too long")
	}
	return nil
}

// Invoice is the stable Callbell view of one invoice. Everything a Lexware user typed is untrusted data.
type Invoice struct {
	ID               string      `json:"id"`
	VoucherNumber    string      `json:"voucher_number"`
	VoucherStatus    string      `json:"voucher_status"`
	Overdue          bool        `json:"overdue"`
	VoucherDate      string      `json:"voucher_date"`
	DueDate          string      `json:"due_date,omitempty"`
	CreatedDate      string      `json:"created_date,omitempty"`
	UpdatedDate      string      `json:"updated_date,omitempty"`
	Archived         bool        `json:"archived"`
	Language         string      `json:"language,omitempty"`
	Title            string      `json:"title,omitempty"`
	Introduction     string      `json:"introduction,omitempty"`
	Remark           string      `json:"remark,omitempty"`
	TaxType          string      `json:"tax_type,omitempty"`
	Currency         string      `json:"currency,omitempty"`
	TotalNetAmount   json.Number `json:"total_net_amount,omitempty"`
	TotalGrossAmount json.Number `json:"total_gross_amount,omitempty"`
	TotalTaxAmount   json.Number `json:"total_tax_amount,omitempty"`
	Contact          *Contact    `json:"contact,omitempty"`
	LineItems        []LineItem  `json:"line_items"`
}

// Contact is the invoice recipient as the invoice carries it.
type Contact struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Supplement  string `json:"supplement,omitempty"`
	Street      string `json:"street,omitempty"`
	Zip         string `json:"zip,omitempty"`
	City        string `json:"city,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
}

// LineItem is one invoice position with its unit price flattened into stable field names.
type LineItem struct {
	Type               string      `json:"type,omitempty"`
	Name               string      `json:"name,omitempty"`
	Description        string      `json:"description,omitempty"`
	Quantity           json.Number `json:"quantity,omitempty"`
	UnitName           string      `json:"unit_name,omitempty"`
	UnitNetAmount      json.Number `json:"unit_net_amount,omitempty"`
	UnitGrossAmount    json.Number `json:"unit_gross_amount,omitempty"`
	TaxRatePercentage  json.Number `json:"tax_rate_percentage,omitempty"`
	DiscountPercentage json.Number `json:"discount_percentage,omitempty"`
	Amount             json.Number `json:"amount,omitempty"`
}

// GetInvoice reads exactly the invoice of a validated identifier and performs no other provider I/O.
func (c *Client) GetInvoice(ctx context.Context, id string) (*Invoice, error) {
	const op = "get invoice"
	if !validUUID(id) {
		return nil, providerError(op, "the invoice identifier must be a UUID")
	}

	var raw invoiceJSON
	if err := c.get(ctx, op, "/v1/invoices/"+url.PathEscape(id), nil, &raw); err != nil {
		return nil, err
	}
	if !strings.EqualFold(raw.ID, id) {
		return nil, &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "Lexware answered with a different invoice than the requested one",
		}
	}

	invoice := &Invoice{
		ID: raw.ID, VoucherNumber: raw.VoucherNumber, VoucherStatus: raw.VoucherStatus,
		Overdue: raw.VoucherStatus == "overdue", VoucherDate: raw.VoucherDate, DueDate: raw.DueDate,
		CreatedDate: raw.CreatedDate, UpdatedDate: raw.UpdatedDate, Archived: raw.Archived,
		Language: raw.Language, Title: raw.Title, Introduction: raw.Introduction, Remark: raw.Remark,
		TaxType: raw.TaxConditions.TaxType, Currency: raw.TotalPrice.Currency,
		TotalNetAmount: raw.TotalPrice.TotalNetAmount, TotalGrossAmount: raw.TotalPrice.TotalGrossAmount,
		TotalTaxAmount: raw.TotalPrice.TotalTaxAmount,
		LineItems:      make([]LineItem, 0, len(raw.LineItems)),
	}
	contact := Contact{
		ID: raw.Address.ContactID, Name: raw.Address.Name, Supplement: raw.Address.Supplement,
		Street: raw.Address.Street, Zip: raw.Address.Zip, City: raw.Address.City,
		CountryCode: raw.Address.CountryCode,
	}
	if contact != (Contact{}) {
		invoice.Contact = &contact
	}
	for _, item := range raw.LineItems {
		invoice.LineItems = append(invoice.LineItems, LineItem{
			Type: item.Type, Name: item.Name, Description: item.Description, Quantity: item.Quantity,
			UnitName: item.UnitName, UnitNetAmount: item.UnitPrice.NetAmount,
			UnitGrossAmount: item.UnitPrice.GrossAmount, TaxRatePercentage: item.UnitPrice.TaxRatePercentage,
			DiscountPercentage: item.DiscountPercentage, Amount: item.LineItemAmount,
		})
	}
	return invoice, nil
}

// voucherListJSON mirrors the provider fields the list operation reads.
type voucherListJSON struct {
	Content []struct {
		ID            string      `json:"id"`
		VoucherType   string      `json:"voucherType"`
		VoucherStatus string      `json:"voucherStatus"`
		VoucherNumber string      `json:"voucherNumber"`
		VoucherDate   string      `json:"voucherDate"`
		DueDate       string      `json:"dueDate"`
		ContactName   string      `json:"contactName"`
		TotalAmount   json.Number `json:"totalAmount"`
		OpenAmount    json.Number `json:"openAmount"`
		Currency      string      `json:"currency"`
		Archived      bool        `json:"archived"`
	} `json:"content"`
	Number        int  `json:"number"`
	Size          int  `json:"size"`
	TotalPages    int  `json:"totalPages"`
	TotalElements int  `json:"totalElements"`
	Last          bool `json:"last"`
}

// invoiceJSON mirrors the provider fields the get operation reads.
type invoiceJSON struct {
	ID            string `json:"id"`
	CreatedDate   string `json:"createdDate"`
	UpdatedDate   string `json:"updatedDate"`
	Language      string `json:"language"`
	Archived      bool   `json:"archived"`
	VoucherStatus string `json:"voucherStatus"`
	VoucherNumber string `json:"voucherNumber"`
	VoucherDate   string `json:"voucherDate"`
	DueDate       string `json:"dueDate"`
	Address       struct {
		ContactID   string `json:"contactId"`
		Name        string `json:"name"`
		Supplement  string `json:"supplement"`
		Street      string `json:"street"`
		City        string `json:"city"`
		Zip         string `json:"zip"`
		CountryCode string `json:"countryCode"`
	} `json:"address"`
	LineItems []struct {
		Type        string      `json:"type"`
		Name        string      `json:"name"`
		Description string      `json:"description"`
		Quantity    json.Number `json:"quantity"`
		UnitName    string      `json:"unitName"`
		UnitPrice   struct {
			Currency          string      `json:"currency"`
			NetAmount         json.Number `json:"netAmount"`
			GrossAmount       json.Number `json:"grossAmount"`
			TaxRatePercentage json.Number `json:"taxRatePercentage"`
		} `json:"unitPrice"`
		DiscountPercentage json.Number `json:"discountPercentage"`
		LineItemAmount     json.Number `json:"lineItemAmount"`
	} `json:"lineItems"`
	TotalPrice struct {
		Currency         string      `json:"currency"`
		TotalNetAmount   json.Number `json:"totalNetAmount"`
		TotalGrossAmount json.Number `json:"totalGrossAmount"`
		TotalTaxAmount   json.Number `json:"totalTaxAmount"`
	} `json:"totalPrice"`
	TaxConditions struct {
		TaxType string `json:"taxType"`
	} `json:"taxConditions"`
	Title        string `json:"title"`
	Introduction string `json:"introduction"`
	Remark       string `json:"remark"`
}

// get performs one bounded read against the fixed gateway and decodes the response into out.
func (c *Client) get(ctx context.Context, op, path string, query url.Values, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return &provider.Error{
			Class: provider.ClassTimeout, Op: op,
			Message: "the request ended while it waited for the Lexware rate limit",
		}
	}

	target := gateway + path
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
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op,
			Message: "the Lexware response could not be read within the size limit",
		}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &provider.Error{
			Class: provider.ClassInvalidResponse, Op: op, Message: "Lexware returned an invalid response",
		}
	}
	return nil
}

// statusError maps an HTTP status to a stable class. The provider message is never copied: Lexware echoes
// request detail into it, and the class plus the status is what a caller can act on.
func statusError(op string, status int) error {
	switch status {
	case http.StatusUnauthorized:
		return &provider.Error{Class: provider.ClassAuth, Op: op, Message: "Lexware rejected the API key"}
	case http.StatusPaymentRequired:
		return &provider.Error{
			Class: provider.ClassPermission, Op: op,
			Message: "the Lexware contract does not include this API access",
		}
	case http.StatusForbidden:
		return &provider.Error{
			Class: provider.ClassPermission, Op: op,
			Message: "the API key is not permitted to perform this operation",
		}
	case http.StatusNotFound:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op, Message: "Lexware does not hold this invoice",
		}
	case http.StatusBadRequest, http.StatusNotAcceptable:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op, Message: "Lexware rejected the request as invalid",
		}
	case http.StatusTooManyRequests:
		return &provider.Error{
			Class: provider.ClassRateLimited, Op: op, Message: "Lexware rate-limited the operation",
		}
	case http.StatusGatewayTimeout:
		return &provider.Error{Class: provider.ClassTimeout, Op: op, Message: "Lexware did not answer in time"}
	default:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op,
			Message: fmt.Sprintf("Lexware rejected the operation (HTTP %d)", status),
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
		return &provider.Error{Class: provider.ClassTimeout, Op: op, Message: "Lexware did not answer in time"}
	case errors.As(err, &certErr), errors.As(err, &hostErr), errors.As(err, &authErr),
		errors.As(err, &recordErr), errors.As(err, &invalidErr):
		return &provider.Error{
			Class: provider.ClassTLS, Op: op, Message: "the TLS connection could not be established",
		}
	default:
		return &provider.Error{Class: provider.ClassUnreachable, Op: op, Message: "Lexware could not be reached"}
	}
}

func providerError(op, message string) error {
	return &provider.Error{Class: provider.ClassProviderError, Op: op, Message: message}
}

// validAPIKey keeps an obviously unusable value out of a request. The real check is the provider's.
func validAPIKey(value string) bool {
	if len(value) < 8 || len(value) > 512 {
		return false
	}
	for _, r := range value {
		// A header value may not carry control characters, and a Lexware key never does.
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
