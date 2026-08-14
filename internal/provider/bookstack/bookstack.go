// Package bookstack implements read-only access to a BookStack instance over its REST API.
//
// Page content arrives as HTML and Markdown. Callbell treats both as untrusted data: it is passed through
// to the output encoders and never rendered, interpreted, or stored.
package bookstack

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// Provider is the provider name used in the configuration.
const Provider = "bookstack"

// The secret roles a BookStack credential must supply.
const (
	roleTokenID     = "token-id"
	roleTokenSecret = "token-secret"
)

// maxCount is the largest page size the BookStack API accepts.
const maxCount = 500

// defaultTimeout bounds every request. Without it a hanging server would block the command forever.
const defaultTimeout = 30 * time.Second

// Capabilities implemented by this provider.
var (
	pagesList = capability.Capability{
		Name:        "knowledge.pages.list",
		Description: "List pages of a knowledge base",
		Risk:        capability.RiskRead,
		Arguments: []capability.Argument{
			{Name: "limit", Description: "Maximum number of pages to return; 0 returns all"},
			{Name: "offset", Description: "Number of pages to skip"},
		},
		Fields: []capability.Field{
			{Name: "id", Description: "Page identifier"},
			{Name: "name", Description: "Page title"},
			{Name: "slug", Description: "URL slug"},
			{Name: "book_id", Description: "Identifier of the containing book"},
			{Name: "chapter_id", Description: "Identifier of the containing chapter, 0 when there is none"},
			{Name: "created_at", Description: "Creation timestamp"},
			{Name: "updated_at", Description: "Last change timestamp"},
		},
	}

	pagesGet = capability.Capability{
		Name:        "knowledge.pages.get",
		Description: "Read one page of a knowledge base",
		Risk:        capability.RiskRead,
		Arguments:   []capability.Argument{{Name: "id", Description: "Page identifier", Required: true}},
		Fields: []capability.Field{
			{Name: "id", Description: "Page identifier"},
			{Name: "name", Description: "Page title"},
			{Name: "slug", Description: "URL slug"},
			{Name: "book_id", Description: "Identifier of the containing book"},
			{Name: "chapter_id", Description: "Identifier of the containing chapter, 0 when there is none"},
			{Name: "created_at", Description: "Creation timestamp"},
			{Name: "updated_at", Description: "Last change timestamp"},
			{Name: "html", Description: "Rendered page content, untrusted data"},
			{Name: "markdown", Description: "Markdown page content when the page uses the Markdown editor, untrusted data"},
		},
	}
)

// Register records this provider's capabilities.
func Register(reg *capability.Registry) error {
	return reg.Register(Provider, pagesList, pagesGet)
}

// Client talks to one BookStack instance with one credential.
type Client struct {
	base *url.URL
	auth string
	http *http.Client
}

// Open builds a client for a resolved connection. The secrets come from the resolver, which owns the
// cascade and the redaction; this provider only asks for the two roles it needs and never returns them.
func Open(resolved *config.Resolved, secrets *secret.Resolver, red *redact.Redactor) (*Client, error) {
	base, err := url.Parse(resolved.BaseURL)
	if err != nil {
		return nil, &provider.Error{
			Class: provider.ClassProviderError, Op: "open",
			Message: fmt.Sprintf("connection %q has an unusable base URL", resolved.Name),
		}
	}

	tokenID, err := role(resolved, secrets, roleTokenID)
	if err != nil {
		return nil, err
	}
	tokenSecret, err := role(resolved, secrets, roleTokenSecret)
	if err != nil {
		return nil, err
	}
	if red != nil {
		red.Add(tokenID, tokenSecret, tokenID+":"+tokenSecret)
	}

	return &Client{
		base: base,
		auth: "Token " + tokenID + ":" + tokenSecret,
		http: &http.Client{
			Timeout: defaultTimeout,
			// A redirect off the configured origin would carry the credential to a host the user never
			// configured, so it is refused rather than followed.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if sameOrigin(via[0].URL, req.URL) {
					return nil
				}
				return &redirectRefusedError{From: via[0].URL.Host}
			},
		},
	}, nil
}

// role resolves one secret role of the connection. Which stage delivers is not this provider's business:
// it needs the value, and the resolver decides where it comes from.
func role(resolved *config.Resolved, secrets *secret.Resolver, name string) (string, error) {
	if secrets == nil {
		return "", &provider.Error{
			Class: provider.ClassProviderError, Op: "open",
			Message: "no credential resolver was configured",
		}
	}
	value, err := secrets.Resolve(resolved.Credential, resolved.Secrets, name)
	if err != nil {
		return "", err
	}
	return value.Secret, nil
}

// redirectRefusedError reports a redirect that would have left the configured origin. The credential is
// never sent to a host the user did not configure.
type redirectRefusedError struct{ From string }

func (e *redirectRefusedError) Error() string {
	return fmt.Sprintf("refused to follow a redirect from %s to a different origin", e.From)
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && a.Host == b.Host
}

// pageJSON mirrors the BookStack API fields this provider reads.
type pageJSON struct {
	ID        int64  `json:"id"`
	BookID    int64  `json:"book_id"`
	ChapterID int64  `json:"chapter_id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	HTML      string `json:"html"`
	Markdown  string `json:"markdown"`
}

type listJSON struct {
	Data  []pageJSON `json:"data"`
	Total int        `json:"total"`
}

type errorJSON struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ListPages returns pages, honouring limit and offset. A limit of zero or less returns every page the
// instance reports, fetched in pages of at most 500 records.
func (c *Client) ListPages(ctx context.Context, limit, offset int) (output.Collection, error) {
	columns := fieldNames(pagesList)
	rows := make([]output.Row, 0, 32)
	// An instance that ignores the offset would otherwise hand back the same records until the reported
	// total is reached, which looks like a complete list but is not one.
	seen := map[int64]bool{}

	for {
		count := maxCount
		if limit > 0 && limit-len(rows) < count {
			count = limit - len(rows)
		}

		query := url.Values{}
		query.Set("count", strconv.Itoa(count))
		query.Set("offset", strconv.Itoa(offset+len(rows)))
		query.Set("sort", "+id")

		var page listJSON
		if err := c.get(ctx, "list pages", "/api/pages", query, &page); err != nil {
			return output.Collection{}, err
		}

		added := 0
		for _, p := range page.Data {
			if seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			rows = append(rows, listRow(p))
			added++
		}

		// No progress means the instance cannot deliver more, whatever its total claims.
		done := added == 0 ||
			(limit > 0 && len(rows) >= limit) ||
			offset+len(rows) >= page.Total
		if done {
			break
		}
	}

	return output.Collection{Columns: columns, Rows: rows}, nil
}

// GetPage returns one page including its untrusted content.
func (c *Client) GetPage(ctx context.Context, id string) (output.Object, error) {
	var page pageJSON
	if err := c.get(ctx, "get page", "/api/pages/"+url.PathEscape(id), nil, &page); err != nil {
		return output.Object{}, err
	}

	return output.Object{Fields: []output.Field{
		{Name: "id", Value: page.ID},
		{Name: "name", Value: page.Name},
		{Name: "slug", Value: page.Slug},
		{Name: "book_id", Value: page.BookID},
		{Name: "chapter_id", Value: page.ChapterID},
		{Name: "created_at", Value: page.CreatedAt},
		{Name: "updated_at", Value: page.UpdatedAt},
		{Name: "html", Value: page.HTML},
		{Name: "markdown", Value: page.Markdown},
	}}, nil
}

// TestConnection performs the smallest authenticated read and reports the stable outcome class.
func (c *Client) TestConnection(ctx context.Context) provider.Class {
	query := url.Values{}
	query.Set("count", "1")

	var page listJSON
	err := c.get(ctx, "test connection", "/api/pages", query, &page)
	if err == nil {
		return provider.ClassOK
	}
	var perr *provider.Error
	if errors.As(err, &perr) {
		return perr.Class
	}
	return provider.ClassProviderError
}

// get performs one read request and decodes the response into out.
func (c *Client) get(ctx context.Context, op, path string, query url.Values, out any) error {
	target := c.base.JoinPath(path)
	target.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return &provider.Error{Class: provider.ClassProviderError, Op: op, Message: "could not build the request"}
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return transportError(op, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return statusError(op, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op,
			Message: "the response was not valid JSON",
		}
	}
	return nil
}

// transportError classifies a failure that happened before a status code existed. The original error text
// is not copied, so a URL carrying credentials can never reach the message.
func transportError(op string, err error) error {
	var (
		certErr  *tls.CertificateVerificationError
		hostErr  x509.HostnameError
		authErr  x509.UnknownAuthorityError
		recErr   tls.RecordHeaderError
		expiring x509.CertificateInvalidError
	)
	switch {
	case errors.As(err, &certErr), errors.As(err, &hostErr), errors.As(err, &authErr),
		errors.As(err, &recErr), errors.As(err, &expiring):
		return &provider.Error{Class: provider.ClassTLS, Op: op, Message: "the TLS connection could not be established"}
	}
	// A refused redirect is a policy decision, not an unreachable server.
	var refused *redirectRefusedError
	if errors.As(err, &refused) {
		return &provider.Error{Class: provider.ClassProviderError, Op: op, Message: refused.Error()}
	}
	return &provider.Error{Class: provider.ClassUnreachable, Op: op, Message: "the server could not be reached"}
}

// statusError maps an HTTP status to a stable class. The provider message is passed through because
// BookStack reports the reason there; secrets are removed centrally before anything is shown.
func statusError(op string, resp *http.Response) error {
	message := ""
	if body, err := io.ReadAll(io.LimitReader(resp.Body, 4096)); err == nil {
		var parsed errorJSON
		if json.Unmarshal(body, &parsed) == nil {
			message = parsed.Error.Message
		}
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return &provider.Error{Class: provider.ClassAuth, Op: op, Message: message}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &provider.Error{Class: provider.ClassRateLimited, Op: op, Message: message}
	default:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op,
			Message: fmt.Sprintf("%s (HTTP %d)", message, resp.StatusCode),
		}
	}
}

func listRow(p pageJSON) output.Row {
	return output.Row{
		"id":         p.ID,
		"name":       p.Name,
		"slug":       p.Slug,
		"book_id":    p.BookID,
		"chapter_id": p.ChapterID,
		"created_at": p.CreatedAt,
		"updated_at": p.UpdatedAt,
	}
}

func fieldNames(c capability.Capability) []string {
	out := make([]string, len(c.Fields))
	for i, f := range c.Fields {
		out[i] = f.Name
	}
	return out
}
