// Package nextcloud implements read-only access to the Files app of one Nextcloud instance over WebDAV.
//
// A Nextcloud installation holds many identities, and every identity owns its own file tree below
// /remote.php/dav/files/{user-id}/. That cardinality is the configuration: a service is one instance with
// an optional installation path, a credential is one user ID together with one revocable app password,
// and a connection binds them to one fixed root folder. An invoke request can name neither the instance,
// nor the identity, nor the root; it may only address a path below the root the connection is bound to.
//
// The adapter produces exactly one HTTP method, PROPFIND, and asks for a fixed property set. It therefore
// never reads file content, never writes, and never reaches another app of the instance. Names, DAV
// properties, and the whole multi-status document arrive from the provider and are treated as untrusted
// data: they are normalised into a stable metadata envelope, passed through the output encoders, and
// never rendered or stored.
package nextcloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// Provider is the provider name used in the configuration and as the operation namespace.
const Provider = "nextcloud"

// The secret roles a Nextcloud credential must supply. Both belong to one identity: the user ID names it,
// and the app password authenticates it without ever exposing the account password or defeating a second
// factor. Nextcloud recommends an app password precisely for WebDAV clients, and it can be revoked alone.
const (
	roleUserID      = "user-id"
	roleAppPassword = "app-password"
)

// dataSensitivity classifies results as file metadata of the configured Nextcloud identity. It is
// deliberately provider-specific; the architecture defines no global sensitivity taxonomy.
const dataSensitivity = "nextcloud-files-metadata"

// filesRoot are the fixed path segments of the authenticated Files WebDAV endpoint. The user ID follows
// them, and the configured root folder follows the user ID.
var filesRoot = []string{"remote.php", "dav", "files"}

// methodPropfind is the only HTTP method this adapter ever produces. A read of metadata needs nothing
// else, and a server can therefore not receive a writing or content-delivering request through it.
const methodPropfind = "PROPFIND"

// The two depths this adapter uses: the node itself, and the node plus its immediate children. A larger
// depth would walk the whole tree and is deliberately not offered.
const (
	depthSelf     = "0"
	depthChildren = "1"
)

// propfindBody asks for exactly the properties the normalised metadata is built from. Requesting a fixed
// set rather than allprop keeps the answer small and keeps an unknown provider property out of the result.
const propfindBody = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop>` +
	`<d:displayname/><d:resourcetype/><d:getcontenttype/><d:getcontentlength/>` +
	`<d:getlastmodified/><d:getetag/><oc:fileid/><oc:size/><oc:permissions/>` +
	`</d:prop></d:propfind>`

// Bounds of one request and one answer. They are deliberately conservative: a folder that exceeds them is
// refused before any of its data is handed on, rather than silently truncated into a list that looks
// complete. A caller who needs a larger folder narrows the connection root instead.
const (
	maxBodyBytes   = 4 << 20
	maxEntries     = 500
	maxXMLDepth    = 20
	maxTextBytes   = 8 << 10
	maxPathLength  = 1024
	maxSegments    = 32
	maxSegmentLen  = 255
	maxUserIDLen   = 64
	maxValueLength = 1024
	defaultTimeout = 30 * time.Second
)

// pathPattern is the schema form of one path relative to the connection root: one or more segments
// separated by a single slash, where no segment is empty, "." or "..", and no character is a backslash or
// a percent sign. It therefore refuses an absolute path, an absolute URL, a traversal, a Windows
// separator, and every percent-encoded separator or double encoding before the core resolves a secret.
//
// callbell-dev: refusing the percent sign outright also refuses a name that genuinely contains one; that
// name stays visible in a listing and needs a narrower connection root to be addressed.
const pathPattern = `^(?:\\.[^./\\\\%][^/\\\\%]*|\\.\\.[^/\\\\%]+|[^./\\\\%][^/\\\\%]*)` +
	`(?:/(?:\\.[^./\\\\%][^/\\\\%]*|\\.\\.[^/\\\\%]+|[^./\\\\%][^/\\\\%]*))*$`

// pathSchema is the optional relative path both operations accept. Omitting it addresses the connection
// root itself.
const pathSchema = `{"type":"string","minLength":1,"maxLength":1024,"pattern":"` + pathPattern + `"}`

// entrySchema is the stable envelope of one file or folder. It is the same shape in both operations, so a
// caller can hand a listed path straight to the stat operation.
const entrySchema = `{"type":"object","properties":{` +
	`"path":{"type":"string"},"name":{"type":"string"},` +
	`"type":{"type":"string","enum":["file","folder"]},` +
	`"content_type":{"type":"string"},"size":{"type":"integer"},"modified_at":{"type":"string"},` +
	`"etag":{"type":"string"},"file_id":{"type":"string"},"readable":{"type":"boolean"}},` +
	`"required":["path","name","type","size","readable"],"additionalProperties":false}`

var nextcloudReadRisk = capability.Risk{
	Effect:          capability.EffectRead,
	Idempotency:     capability.IdempotencySafe,
	Confirmation:    capability.ConfirmationNone,
	OpenWorld:       true,
	DataSensitivity: dataSensitivity,
}

// entryFields is the discovery metadata of the normalised envelope. Both operations report the same
// fields, so they are described once.
var entryFields = []capability.Field{
	{Name: "path", Description: "Path relative to the fixed root folder of this connection, empty for the root itself"},
	{Name: "name", Description: "Display name of the file or folder, untrusted data"},
	{Name: "type", Description: "Either file or folder"},
	{Name: "content_type", Description: "MIME type Nextcloud reports for a file"},
	{Name: "size", Description: "Size in bytes; for a folder the size Nextcloud keeps for its whole subtree"},
	{Name: "modified_at", Description: "Last change time, normalised to RFC 3339 in UTC"},
	{Name: "etag", Description: "Entity tag of the current version"},
	{Name: "file_id", Description: "Stable Nextcloud file identifier"},
	{Name: "readable", Description: "True when the effective permissions of this identity allow reading the node"},
}

var filesList = capability.Descriptor{
	ID:      Provider + ".files.list",
	Version: 1,
	Title:   "List Nextcloud files",
	Description: "List the immediate children of the fixed root folder, or of one folder below it, of an " +
		"explicit Nextcloud connection; the listing is one level deep and never reads file content",
	Tags:                       []string{"nextcloud", "files", "webdav", "list", "folder"},
	Risk:                       nextcloudReadRisk,
	Provider:                   Provider,
	RequiresExplicitConnection: true,
	InputSchema: json.RawMessage(`{"type":"object","properties":{"path":` + pathSchema + `},` +
		`"additionalProperties":false}`),
	OutputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"path":{"type":"string"},"entries":{"type":"array","items":` + entrySchema + `},` +
		`"count":{"type":"integer"}},` +
		`"required":["path","entries","count"],"additionalProperties":false}`),
	Arguments: []capability.Argument{
		{Name: "path", Description: "Folder relative to the fixed root folder of this connection; the root itself when omitted"},
	},
	Fields: []capability.Field{
		{Name: "path", Description: "Folder that was listed, relative to the connection root"},
		{Name: "entries", Description: "Metadata of the immediate children, untrusted data; a child whose metadata the server refused is omitted"},
		{Name: "count", Description: "Number of reported children"},
	},
	Examples: []capability.Example{{
		Description: "List one folder below the fixed root of this connection; omitting the path lists that root itself",
		Arguments:   json.RawMessage(`{"path":"2026"}`),
	}},
}

var filesStat = capability.Descriptor{
	ID:      Provider + ".files.stat",
	Version: 1,
	Title:   "Get Nextcloud file metadata",
	Description: "Read the metadata of exactly one file or folder below the fixed root folder of an " +
		"explicit Nextcloud connection; file content is never read",
	Tags:                       []string{"nextcloud", "files", "webdav", "stat", "metadata"},
	Risk:                       nextcloudReadRisk,
	Provider:                   Provider,
	RequiresExplicitConnection: true,
	InputSchema: json.RawMessage(`{"type":"object","properties":{"path":` + pathSchema + `},` +
		`"additionalProperties":false}`),
	OutputSchema: json.RawMessage(entrySchema),
	Arguments: []capability.Argument{
		{Name: "path", Description: "File or folder relative to the fixed root folder of this connection; the root itself when omitted"},
	},
	Fields: entryFields,
	Examples: []capability.Example{{
		Description: "Read the metadata of one file a listing reported",
		Arguments:   json.RawMessage(`{"path":"Reports/2026/q1.pdf"}`),
	}},
}

// Register adds Nextcloud metadata, its read-only connection test, and the two read operations.
func Register(reg *capability.Registry) error {
	if err := reg.RegisterProvider(config.ProviderMetadata{
		ID: Provider, Name: "Nextcloud",
		SecretRoles: []config.SecretRole{{
			Name: roleUserID,
			Description: "Nextcloud user ID of the identity to read as: the value shown as username in " +
				"Settings, Personal info, not the display name and not an email address unless the " +
				"account uses one as its user ID",
		}, {
			Name: roleAppPassword,
			Description: "Nextcloud app password of the same identity: Settings, Security, Devices and " +
				"sessions, create a new app password; it is revocable on its own and is what Nextcloud " +
				"expects from a WebDAV client, especially with two-factor or external authentication",
		}},
		Target: config.TargetMetadata{
			Label:    "root folder",
			Required: true,
			Description: "fixed folder below the Files of this identity that this connection may read, " +
				"for example Reports or Team/Reports; a single / binds the whole Files root",
		},
	}, TestConnection); err != nil {
		return err
	}
	return reg.Register(Provider,
		capability.Operation{Descriptor: filesList, Handler: capability.Handler(invokeFilesList)},
		capability.Operation{Descriptor: filesStat, Handler: capability.Handler(invokeFilesStat)},
	)
}

// arguments are the only inputs either operation accepts. The instance, the identity, and the root folder
// are configuration, so nothing here can move a request outside the selected connection.
type arguments struct {
	Path string `json:"path"`
}

func invokeFilesList(ctx context.Context, resolved *config.Resolved, secrets *secret.Resolver,
	red *redact.Redactor, raw json.RawMessage) (any, error) {
	var input arguments
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, providerError("list files", "the validated arguments could not be read")
	}
	client, err := Open(resolved, secrets, red)
	if err != nil {
		return nil, err
	}
	return client.ListFiles(ctx, input.Path)
}

func invokeFilesStat(ctx context.Context, resolved *config.Resolved, secrets *secret.Resolver,
	red *redact.Redactor, raw json.RawMessage) (any, error) {
	var input arguments
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, providerError("stat file", "the validated arguments could not be read")
	}
	client, err := Open(resolved, secrets, red)
	if err != nil {
		return nil, err
	}
	return client.StatFile(ctx, input.Path)
}

// Client binds one identity of one configured Nextcloud instance to the fixed root folder of one
// connection. A client is opened per request by the application core and is never shared.
type Client struct {
	origin string
	// prefix are the decoded path segments up to and including the user ID: the optional installation
	// path, the fixed Files WebDAV segments, and the identity.
	prefix []string
	// root are the decoded segments of the fixed root folder below the Files root of that identity.
	root []string
	auth string
	http *http.Client
}

// Open resolves the identity of one selected connection and returns a client bound to its root folder.
func Open(resolved *config.Resolved, secrets *secret.Resolver, red *redact.Redactor) (*Client, error) {
	const op = "open"
	if resolved == nil {
		return nil, providerError(op, "no connection was selected")
	}
	origin, install, err := parseInstance(resolved.BaseURL)
	if err != nil {
		return nil, providerError(op, err.Error())
	}
	root, err := parseRoot(resolved.Target)
	if err != nil {
		return nil, providerError(op, err.Error())
	}
	if secrets == nil {
		return nil, providerError(op, "no credential resolver was configured")
	}

	userID, err := role(resolved, secrets, roleUserID)
	if err != nil {
		return nil, err
	}
	if !validUserID(userID) {
		return nil, &provider.Error{
			Class: provider.ClassAuth, Op: op, Message: "the Nextcloud user ID is unusable",
		}
	}
	password, err := role(resolved, secrets, roleAppPassword)
	if err != nil {
		return nil, err
	}
	if !validPassword(password) {
		return nil, &provider.Error{
			Class: provider.ClassAuth, Op: op, Message: "the Nextcloud app password is unusable",
		}
	}

	// Every value that could carry the app password is registered before it can reach a diagnostic: the
	// password itself, the basic-auth pair, its encoding, and the complete header.
	pair := userID + ":" + password
	encoded := base64.StdEncoding.EncodeToString([]byte(pair))
	header := "Basic " + encoded
	if red != nil {
		red.Add(password, pair, encoded, header)
	}

	prefix := append(append([]string{}, install...), filesRoot...)
	prefix = append(prefix, userID)
	client := &Client{origin: origin, prefix: prefix, root: root, auth: header}
	client.http = newHTTPClient(origin, escapePath(prefix))
	return client, nil
}

// role resolves one secret role of the connection. Which stage of the cascade delivers is not this
// provider's business: it needs the value, and the resolver decides where it comes from.
func role(resolved *config.Resolved, secrets *secret.Resolver, name string) (string, error) {
	value, err := secrets.Resolve(resolved.Credential, resolved.Secrets, name)
	if err != nil {
		return "", err
	}
	return value.Secret, nil
}

// parseInstance validates the configured service and splits it into the origin and the optional
// installation path. A Nextcloud instance may live below a path such as /nextcloud, so the path is kept,
// but userinfo, a query, or a fragment would either carry a credential or rewrite every request.
func parseInstance(raw string) (string, []string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", nil, errors.New("a Nextcloud service needs a usable https URL")
	}
	if parsed.Scheme != "https" {
		return "", nil, errors.New("a Nextcloud service must use https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Opaque != "" {
		return "", nil, errors.New(
			"a Nextcloud service must be an https URL without user, query, or fragment")
	}
	install, err := splitConfigured(parsed.Path)
	if err != nil {
		return "", nil, fmt.Errorf("the installation path of this Nextcloud service is unusable: %w", err)
	}
	return parsed.Scheme + "://" + parsed.Host, install, nil
}

// parseRoot reads the fixed root folder one connection is bound to. A single slash binds the whole Files
// root of the identity; anything else is a folder below it.
func parseRoot(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("a Nextcloud connection needs a fixed root folder as its target")
	}
	segments, err := splitConfigured(trimmed)
	if err != nil {
		return nil, fmt.Errorf("the configured Nextcloud root folder is unusable: %w", err)
	}
	return segments, nil
}

// splitConfigured reads a configured path into decoded segments. A leading and a trailing slash are
// accepted here, because a configuration field is written by a person; everything a request could be
// moved by is not.
func splitConfigured(raw string) ([]string, error) {
	trimmed := strings.Trim(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return nil, nil
	}
	return splitRelative(trimmed)
}

// splitRelative reads one path relative to the connection root into its segments. It mirrors the schema
// pattern in Go, so a direct caller stays inside the same rules as an agent request, and it adds the
// checks a regular expression cannot express safely.
func splitRelative(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxPathLength {
		return nil, errors.New("the path is too long")
	}
	if strings.HasPrefix(raw, "/") {
		return nil, errors.New("a path must be relative to the root folder of this connection")
	}
	if strings.Contains(raw, "://") {
		return nil, errors.New("a path must be relative to the root folder of this connection, not a URL")
	}
	segments := strings.Split(raw, "/")
	if len(segments) > maxSegments {
		return nil, errors.New("the path has too many components")
	}
	for _, segment := range segments {
		if err := checkSegment(segment); err != nil {
			return nil, err
		}
	}
	return segments, nil
}

// checkSegment rejects everything that could leave the connection root or change how the server resolves
// the path: an empty component, a relative component, a separator in any spelling, and a control
// character. Percent signs are refused outright, so no input can be encoded twice or smuggle a separator.
func checkSegment(segment string) error {
	switch {
	case segment == "":
		return errors.New("a path must not contain an empty component")
	case segment == "." || segment == "..":
		return errors.New("a path must not contain a relative component")
	case len(segment) > maxSegmentLen:
		return errors.New("a path component is too long")
	}
	for _, r := range segment {
		switch {
		case r == '/' || r == '\\':
			return errors.New("a path component must not contain a separator")
		case r == '%':
			return errors.New("a path must be written literally, not percent-encoded")
		case r < 0x20 || r == 0x7f:
			return errors.New("a path component must not contain control characters")
		}
	}
	return nil
}

// escapePath encodes decoded segments into a request path, escaping every segment exactly once.
func escapePath(segments []string) string {
	escaped := make([]string, len(segments))
	for i, segment := range segments {
		escaped[i] = url.PathEscape(segment)
	}
	return "/" + strings.Join(escaped, "/")
}

// requestURL builds the absolute URL of one node below the connection root. The origin, the installation
// path, the identity, and the root folder come from the configuration; only rel comes from the request.
func (c *Client) requestURL(rel []string) string {
	segments := append(append(append([]string{}, c.prefix...), c.root...), rel...)
	path := escapePath(segments)
	if len(c.root)+len(rel) == 0 {
		// The Files root of an identity is addressed as a collection.
		path += "/"
	}
	return c.origin + path
}

// transport carries every Nextcloud request. A nil value is Go's default transport; the package's own
// tests replace it with recorded responses.
var transport http.RoundTripper

// newHTTPClient bounds every request in time and keeps a credential-carrying redirect on the configured
// origin and inside the Files root of the configured identity.
func newHTTPClient(origin, prefix string) *http.Client {
	return &http.Client{
		Timeout:   defaultTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme+"://"+req.URL.Host != origin {
				return &redirectRefusedError{}
			}
			escaped := req.URL.EscapedPath()
			if escaped != prefix && !strings.HasPrefix(escaped, prefix+"/") {
				return &redirectRefusedError{}
			}
			return nil
		},
	}
}

// redirectRefusedError reports a redirect that would have carried the app password off the configured
// origin or out of the Files root of the configured identity. Its message names neither.
type redirectRefusedError struct{}

func (e *redirectRefusedError) Error() string {
	return "refused to follow a redirect that leaves the configured Nextcloud instance or root"
}

// TestConnection performs the smallest safe authenticated read: one PROPFIND of depth 0 on the fixed root
// folder. It proves that the instance answers Files WebDAV, that the app password is accepted, and that
// the identity may read the folder the connection is bound to. Nothing is written and no content is read.
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

	entry, err := c.stat(ctx, op, nil, true)
	if err != nil {
		var providerErr *provider.Error
		if !errors.As(err, &providerErr) {
			return provider.ClassProviderError, nil
		}
		// A missing root is reported with its own explanation, because no stable class can say that the
		// instance and the credential are fine while the configured folder is not there.
		if providerErr.Class == provider.ClassProviderError && providerErr.Message == messageNotFound {
			return "", errors.New(
				"this Nextcloud identity does not hold the root folder this connection is bound to")
		}
		return providerErr.Class, nil
	}
	if entry.Type != typeFolder {
		return "", errors.New("the configured Nextcloud root is a file, not a folder")
	}
	if !entry.Readable {
		return "", errors.New("this Nextcloud identity may not read the configured root folder")
	}
	return provider.ClassOK, nil
}

// The two node types this provider reports.
const (
	typeFile   = "file"
	typeFolder = "folder"
)

// Entry is the stable Callbell view of one file or folder: where it sits below the connection root, what
// it is called, and the fixed metadata set the adapter asks for. Content is never part of it.
type Entry struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	ModifiedAt  string `json:"modified_at,omitempty"`
	ETag        string `json:"etag,omitempty"`
	FileID      string `json:"file_id,omitempty"`
	Readable    bool   `json:"readable"`
}

// ListResult is the normalised, one level deep listing of one folder below the connection root.
type ListResult struct {
	Path    string  `json:"path"`
	Entries []Entry `json:"entries"`
	Count   int     `json:"count"`
}

// ListFiles reads the immediate children of the connection root, or of one folder below it, with a single
// PROPFIND of depth 1. The requested folder itself is normalised out of the children.
func (c *Client) ListFiles(ctx context.Context, path string) (*ListResult, error) {
	const op = "list files"
	rel, err := splitRelative(path)
	if err != nil {
		return nil, providerError(op, err.Error())
	}

	resources, err := c.propfind(ctx, op, rel, depthChildren)
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(resources))
	var self *Entry
	for i := range resources {
		relative, err := c.relativeOf(op, resources[i].href)
		if err != nil {
			return nil, err
		}
		switch {
		case len(relative) == len(rel):
			if !equalSegments(relative, rel) {
				return nil, invalidResponse(op, messageForeignEntry)
			}
			if self != nil {
				return nil, invalidResponse(op, "Nextcloud reported the requested folder twice")
			}
			if err := resources[i].failure(op); err != nil {
				return nil, err
			}
			self = c.entryOf(relative, &resources[i])
		case len(relative) == len(rel)+1 && equalSegments(relative[:len(rel)], rel):
			// A child whose properties the server refused carries no usable metadata, so it is left out
			// rather than reported as an entry the caller could act on.
			if resources[i].failure(op) == nil {
				entries = append(entries, *c.entryOf(relative, &resources[i]))
			}
		default:
			return nil, invalidResponse(op, messageForeignEntry)
		}
	}

	if self == nil {
		return nil, invalidResponse(op, "Nextcloud answered without the requested folder")
	}
	if self.Type != typeFolder {
		return nil, providerError(op, "this path is a file; read it with "+filesStat.ID)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return &ListResult{Path: self.Path, Entries: entries, Count: len(entries)}, nil
}

// StatFile reads the metadata of exactly one node below the connection root with a single PROPFIND of
// depth 0.
func (c *Client) StatFile(ctx context.Context, path string) (*Entry, error) {
	const op = "stat file"
	rel, err := splitRelative(path)
	if err != nil {
		return nil, providerError(op, err.Error())
	}
	return c.stat(ctx, op, rel, false)
}

// stat performs one depth 0 PROPFIND and normalises the single node it must answer with. Only the
// connection test asks for the capability check as well; a normal read must not fail because a proxy
// dropped a header.
func (c *Client) stat(ctx context.Context, op string, rel []string, capabilities bool) (*Entry, error) {
	resources, err := c.propfindWith(ctx, op, rel, depthSelf, capabilities)
	if err != nil {
		return nil, err
	}
	if len(resources) != 1 {
		return nil, invalidResponse(op, "Nextcloud answered with more than the requested node")
	}
	relative, err := c.relativeOf(op, resources[0].href)
	if err != nil {
		return nil, err
	}
	if !equalSegments(relative, rel) {
		return nil, invalidResponse(op, messageForeignEntry)
	}
	if err := resources[0].failure(op); err != nil {
		return nil, err
	}
	return c.entryOf(relative, &resources[0]), nil
}

// entryOf normalises one multi-status resource into the stable envelope. Every value stays untrusted
// provider data; only its length, its form, and its meaning are normalised.
func (c *Client) entryOf(relative []string, res *resource) *Entry {
	entry := &Entry{
		Path:     strings.Join(relative, "/"),
		Name:     bounded(res.props[propDisplayName]),
		Type:     typeFile,
		ETag:     bounded(strings.Trim(res.props[propETag], `"`)),
		Readable: readable(res.props[propPermissions]),
	}
	if res.collection {
		entry.Type = typeFolder
	}
	if entry.Name == "" {
		entry.Name = lastSegment(relative, c.root)
	}
	if entry.Type == typeFile {
		entry.ContentType = bounded(res.props[propContentType])
		entry.Size = number(res.props[propContentLength])
	} else {
		entry.Size = number(res.props[propSize])
	}
	if id := res.props[propFileID]; digitsOnly(id) {
		entry.FileID = id
	}
	if parsed, err := http.ParseTime(res.props[propLastModified]); err == nil {
		entry.ModifiedAt = parsed.UTC().Format(time.RFC3339)
	}
	return entry
}

// lastSegment names a node whose display name the server did not report: the last segment of its path,
// and for the connection root itself the last segment of that root.
func lastSegment(relative, root []string) string {
	if len(relative) > 0 {
		return relative[len(relative)-1]
	}
	if len(root) > 0 {
		return root[len(root)-1]
	}
	return ""
}

// readable reports the effective read permission of the identity. Nextcloud spells the permissions as
// letters, and G is the readable one. A server that reports none is taken at the word of its answer: it
// delivered the metadata, so the node is readable.
func readable(permissions string) bool {
	if permissions == "" {
		return true
	}
	return strings.ContainsAny(permissions, "Gg")
}

func number(raw string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func digitsOnly(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// bounded keeps an oversized provider string out of the result without interpreting it.
func bounded(value string) string {
	if len(value) > maxValueLength {
		return value[:maxValueLength]
	}
	return value
}

func equalSegments(a, b []string) bool {
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

// propfind performs one bounded PROPFIND and returns the resources of the multi-status answer.
func (c *Client) propfind(ctx context.Context, op string, rel []string, depth string) ([]resource, error) {
	return c.propfindWith(ctx, op, rel, depth, false)
}

// propfindWith is the single request path of this adapter. It is the only place that builds an HTTP
// request, and it can build no method other than PROPFIND.
func (c *Client) propfindWith(ctx context.Context, op string, rel []string, depth string,
	capabilities bool) ([]resource, error) {
	req, err := http.NewRequestWithContext(ctx, methodPropfind, c.requestURL(rel),
		strings.NewReader(propfindBody))
	if err != nil {
		return nil, providerError(op, "the request could not be built")
	}
	req.ContentLength = int64(len(propfindBody))
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("Depth", depth)

	response, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(op, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusMultiStatus {
		return nil, statusError(op, response.StatusCode)
	}
	if capabilities {
		if err := checkWebDAV(op, response.Header); err != nil {
			return nil, err
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return nil, invalidResponse(op, "the Nextcloud response could not be read within the size limit")
	}
	return parseMultiStatus(op, body)
}

// checkWebDAV verifies that the instance announces WebDAV class 1, which is what the Files app serves. A
// server that announces nothing is accepted: a proxy may drop the header, and the multi-status answer
// itself is the stronger evidence.
func checkWebDAV(op string, header http.Header) error {
	announced := header.Get("Dav")
	if strings.TrimSpace(announced) == "" {
		return nil
	}
	for _, class := range strings.Split(announced, ",") {
		if strings.TrimSpace(class) == "1" {
			return nil
		}
	}
	return invalidResponse(op, "this server does not announce the WebDAV class the Files app needs")
}

// messageNotFound is the one classified message the connection test has to recognise, so it is written
// once rather than compared as free text in two places.
const messageNotFound = "this Nextcloud connection does not hold this path"

// messageForeignEntry covers every answer that names a node outside the folder the request addressed.
const messageForeignEntry = "Nextcloud answered with an entry outside the requested folder"

// statusError maps an HTTP status to a stable class. The provider body is never read into the message:
// Nextcloud echoes the request path into it, and the class plus the status is what a caller can act on.
func statusError(op string, status int) error {
	switch status {
	case http.StatusUnauthorized:
		return &provider.Error{
			Class: provider.ClassAuth, Op: op,
			Message: "Nextcloud rejected the user ID or the app password",
		}
	case http.StatusForbidden:
		return &provider.Error{
			Class: provider.ClassPermission, Op: op,
			Message: "this Nextcloud identity may not read this path",
		}
	case http.StatusNotFound:
		return &provider.Error{Class: provider.ClassProviderError, Op: op, Message: messageNotFound}
	case http.StatusMethodNotAllowed, http.StatusConflict:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op, Message: "Nextcloud refused this read on this path",
		}
	case http.StatusLocked:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op, Message: "this Nextcloud node is locked",
		}
	case http.StatusInsufficientStorage:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op,
			Message: "the storage quota of this Nextcloud identity is exhausted",
		}
	case http.StatusTooManyRequests:
		return &provider.Error{
			Class: provider.ClassRateLimited, Op: op, Message: "Nextcloud rate-limited the operation",
		}
	case http.StatusServiceUnavailable:
		return &provider.Error{
			Class: provider.ClassUnreachable, Op: op,
			Message: "Nextcloud is unavailable or in maintenance mode",
		}
	case http.StatusGatewayTimeout:
		return &provider.Error{
			Class: provider.ClassTimeout, Op: op, Message: "Nextcloud did not answer in time",
		}
	default:
		return &provider.Error{
			Class: provider.ClassProviderError, Op: op,
			Message: fmt.Sprintf("Nextcloud rejected the operation (HTTP %d)", status),
		}
	}
}

// transportError classifies a failure that happened before a status code existed. The shared classifier
// owns the rules, so Nextcloud publishes the same class and the same transport cause as every other
// provider, and the original error text is never copied.
func transportError(op string, err error) error {
	// A refused redirect is a policy decision, not an unreachable server.
	var refused *redirectRefusedError
	if errors.As(err, &refused) {
		return providerError(op, refused.Error())
	}
	return provider.Transport(op, "Nextcloud", err)
}

func providerError(op, message string) error {
	return &provider.Error{Class: provider.ClassProviderError, Op: op, Message: message}
}

func invalidResponse(op, message string) error {
	return &provider.Error{Class: provider.ClassInvalidResponse, Op: op, Message: message}
}

// validUserID keeps an unusable identity out of a request path and out of the basic-auth header. The real
// check is the provider's; this one only refuses what would change the meaning of either.
func validUserID(value string) bool {
	if value == "" || len(value) > maxUserIDLen || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		switch {
		case r == '/', r == '\\', r == ':', r == '%', r == '?', r == '#':
			return false
		case r < 0x20 || r == 0x7f:
			return false
		}
	}
	return true
}

// validPassword keeps an obviously unusable app password out of a header. A Nextcloud app password is a
// printable ASCII string; a control character in one would be a header injection, not a credential.
func validPassword(value string) bool {
	if len(value) < 8 || len(value) > maxValueLength {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
