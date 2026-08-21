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
	"reflect"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// The canaries stand for app passwords, for a file name, and for a provider body. No test reaches a
// productive installation: every request is answered by the package's own transport seam.
const (
	aliceUser  = "alice"
	bobUser    = "bobby"
	carolUser  = "carol"
	aliceToken = "canary-nextcloud-app-password-alice-6b19"
	bobToken   = "canary-nextcloud-app-password-bobby-2f74"
	carolToken = "canary-nextcloud-app-password-carol-8d35"

	aliceUserEnv  = "TEST_NEXTCLOUD_ALICE_USER"
	aliceTokenEnv = "TEST_NEXTCLOUD_ALICE_PASSWORD"
	bobUserEnv    = "TEST_NEXTCLOUD_BOB_USER"
	bobTokenEnv   = "TEST_NEXTCLOUD_BOB_PASSWORD"
	carolUserEnv  = "TEST_NEXTCLOUD_CAROL_USER"
	carolTokenEnv = "TEST_NEXTCLOUD_CAROL_PASSWORD"

	mainInstance    = "https://cloud.example.invalid"
	partnerInstance = "https://partner.example.invalid/nextcloud"
	partnerOrigin   = "https://partner.example.invalid"

	nameCanary = "name-canary-nextcloud-7c1e"
	bodyCanary = "provider-body-canary-nextcloud-3b8a"
)

// The Files roots the fixtures answer for.
const (
	aliceRoot   = "/remote.php/dav/files/" + aliceUser + "/Reports"
	bobRoot     = "/remote.php/dav/files/" + bobUser + "/Audit"
	carolRoot   = "/nextcloud/remote.php/dav/files/" + carolUser + "/Shared/Callbell"
	archiveRoot = "/remote.php/dav/files/" + aliceUser + "/Archive/2026"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// call is one request the adapter produced, recorded so a test can prove the method, the depth, and the
// path a connection is bound to.
type call struct {
	method string
	url    *url.URL
	depth  string
	auth   string
	body   string
}

// serve replaces the package transport for one test and records every request. Production keeps Go's
// default transport.
func serve(t *testing.T, handler func(*http.Request) (*http.Response, error)) *[]call {
	t.Helper()
	calls := &[]call{}
	previous := transport
	transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := ""
		if request.Body != nil {
			raw, _ := io.ReadAll(request.Body)
			body = string(raw)
		}
		*calls = append(*calls, call{
			method: request.Method, url: request.URL, depth: request.Header.Get("Depth"),
			auth: request.Header.Get("Authorization"), body: body,
		})
		return handler(request)
	})
	t.Cleanup(func() { transport = previous })
	return calls
}

// refuse fails the test when any provider request is attempted.
func refuse(t *testing.T) {
	t.Helper()
	serve(t, func(request *http.Request) (*http.Response, error) {
		t.Errorf("the provider was contacted: %s %s", request.Method, request.URL.Path)
		return nil, errors.New("no request expected")
	})
}

func xmlResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/xml; charset=utf-8"},
			"Dav":          []string{"1, 3, extended-mkcol"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func multistatus(entries ...string) string {
	return `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" ` +
		`xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">` +
		strings.Join(entries, "") + `</d:multistatus>`
}

// folderXML is one collection as Nextcloud reports it, including the properties the adapter did not ask
// for and the 404 block sabre/dav adds for a property a collection does not have.
func folderXML(href, name, id, size string) string {
	return `<d:response><d:href>` + href + `</d:href><d:propstat><d:prop>` +
		`<d:displayname>` + name + `</d:displayname>` +
		`<d:resourcetype><d:collection/></d:resourcetype>` +
		`<d:getlastmodified>Mon, 02 Mar 2026 11:15:00 GMT</d:getlastmodified>` +
		`<d:getetag>&quot;60f1c8a2e4b19&quot;</d:getetag>` +
		`<oc:fileid>` + id + `</oc:fileid><oc:size>` + size + `</oc:size>` +
		`<oc:permissions>RGDNVCK</oc:permissions><nc:has-preview>false</nc:has-preview>` +
		`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>` +
		`<d:propstat><d:prop><d:getcontenttype/><d:getcontentlength/></d:prop>` +
		`<d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response>`
}

func fileXML(href, name, id, length string) string {
	return `<d:response><d:href>` + href + `</d:href><d:propstat><d:prop>` +
		`<d:displayname>` + name + `</d:displayname><d:resourcetype/>` +
		`<d:getcontenttype>application/pdf</d:getcontenttype>` +
		`<d:getcontentlength>` + length + `</d:getcontentlength>` +
		`<d:getlastmodified>Wed, 01 Apr 2026 07:05:00 GMT</d:getlastmodified>` +
		`<d:getetag>&quot;3a71bd0c1&quot;</d:getetag>` +
		`<oc:fileid>` + id + `</oc:fileid><oc:permissions>RGDNVW</oc:permissions>` +
		`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
}

// reportsListing is the depth 1 answer for the fixed root of the reports connection: the root itself and
// three children, one of them carrying the untrusted name canary.
var reportsListing = multistatus(
	folderXML(aliceRoot+"/", "Reports", "1001", "4096"),
	folderXML(aliceRoot+"/2026/", "2026", "1002", "2048"),
	fileXML(aliceRoot+"/q1.pdf", "q1.pdf", "1003", "5120"),
	fileXML(aliceRoot+"/"+url.PathEscape(nameCanary)+".txt", nameCanary+".txt", "1004", "12"),
)

func resolvedConnection(name, credential, userEnv, tokenEnv, instance, target string) *config.Resolved {
	return &config.Resolved{
		Name: name, Provider: Provider, BaseURL: instance, Service: "nextcloud",
		Credential: credential, Target: target,
		Secrets: config.Credential{
			Type: config.CredentialTypeEnv,
			Values: map[string]string{
				roleUserID: userEnv, roleAppPassword: tokenEnv,
			},
		},
	}
}

func resolver(red *redact.Redactor) *secret.Resolver {
	return secret.NewWith(func(name string) string {
		switch name {
		case aliceUserEnv:
			return aliceUser
		case aliceTokenEnv:
			return aliceToken
		case bobUserEnv:
			return bobUser
		case bobTokenEnv:
			return bobToken
		case carolUserEnv:
			return carolUser
		case carolTokenEnv:
			return carolToken
		}
		return ""
	}, nil, nil, red)
}

// client opens the reports connection with the package transport currently installed.
func client(t *testing.T) (*Client, *redact.Redactor) {
	t.Helper()
	red := &redact.Redactor{}
	c, err := Open(resolvedConnection("reports", "cloud-reader", aliceUserEnv, aliceTokenEnv,
		mainInstance, "Reports"), resolver(red), red)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	return c, red
}

func basicAuth(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

func classOf(err error) provider.Class {
	var providerErr *provider.Error
	if errors.As(err, &providerErr) {
		return providerErr.Class
	}
	return ""
}

// Register publishes the configuration metadata the TUI needs and exactly two read-only operations. The
// identity is two credential roles, and the fixed root folder is a required target.
func TestRegisterPublishesMetadataAndTwoReadOnlyOperations(t *testing.T) {
	reg := capability.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	metadata, ok := reg.ProviderMetadata(Provider)
	if !ok || metadata.Name != "Nextcloud" || metadata.DefaultBaseURL != "" {
		t.Fatalf("metadata = %+v, %v", metadata, ok)
	}
	if len(metadata.SecretRoles) != 2 || metadata.SecretRoles[0].Name != roleUserID ||
		metadata.SecretRoles[1].Name != roleAppPassword {
		t.Fatalf("secret roles = %+v, want the identity and its app password", metadata.SecretRoles)
	}
	if !strings.Contains(metadata.SecretRoles[1].Description, "app password") {
		t.Errorf("app password role = %q, want the revocable app password named", metadata.SecretRoles[1].Description)
	}
	if !metadata.Target.Required || metadata.Target.Label != "root folder" ||
		!strings.Contains(metadata.Target.Description, "Files of this identity") {
		t.Fatalf("target metadata = %+v, want a required root folder", metadata.Target)
	}

	descriptors := reg.Provider(Provider)
	if len(descriptors) != 2 || descriptors[0].ID != "nextcloud.files.list" ||
		descriptors[1].ID != "nextcloud.files.stat" {
		t.Fatalf("descriptors = %+v", descriptors)
	}
	for _, descriptor := range descriptors {
		if descriptor.Risk.Effect != capability.EffectRead ||
			descriptor.Risk.Idempotency != capability.IdempotencySafe ||
			descriptor.Risk.Confirmation != capability.ConfirmationNone ||
			!descriptor.Risk.OpenWorld || descriptor.Risk.DataSensitivity != dataSensitivity {
			t.Errorf("descriptor %s risk = %+v", descriptor.ID, descriptor.Risk)
		}
		if !descriptor.RequiresExplicitConnection {
			t.Errorf("descriptor %s does not require an explicit connection", descriptor.ID)
		}
		// Neither instance, nor identity, nor root folder is an argument: all three are configuration.
		for _, forbidden := range []string{
			"base_url", "instance", "user", "password", "root", "href", "depth", "method", "url",
		} {
			if strings.Contains(string(descriptor.InputSchema), forbidden) {
				t.Errorf("the input schema of %s offers %q: %s", descriptor.ID, forbidden, descriptor.InputSchema)
			}
		}
	}
}

// One connection is one instance, one identity, and one fixed root folder. An installation path stays in
// front of the Files root, and the app password only ever appears as a basic-auth header.
func TestOpenBindsInstanceIdentityAndRoot(t *testing.T) {
	tests := []struct {
		name       string
		connection *config.Resolved
		origin     string
		root       string
		auth       string
	}{
		{
			name: "the first identity of an instance",
			connection: resolvedConnection("reports", "cloud-reader", aliceUserEnv, aliceTokenEnv,
				mainInstance, "Reports"),
			origin: mainInstance, root: aliceRoot, auth: basicAuth(aliceUser, aliceToken),
		},
		{
			name: "a second identity of the same instance",
			connection: resolvedConnection("audit", "cloud-auditor", bobUserEnv, bobTokenEnv,
				mainInstance, "Audit"),
			origin: mainInstance, root: bobRoot, auth: basicAuth(bobUser, bobToken),
		},
		{
			name: "another instance below an installation path",
			connection: resolvedConnection("partner", "partner-reader", carolUserEnv, carolTokenEnv,
				partnerInstance, "Shared/Callbell"),
			origin: partnerOrigin, root: carolRoot, auth: basicAuth(carolUser, carolToken),
		},
		{
			name: "the same credential bound to a second root folder",
			connection: resolvedConnection("archive", "cloud-reader", aliceUserEnv, aliceTokenEnv,
				mainInstance, "/Archive/2026/"),
			origin: mainInstance, root: archiveRoot, auth: basicAuth(aliceUser, aliceToken),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			red := &redact.Redactor{}
			c, err := Open(tt.connection, resolver(red), red)
			if err != nil {
				t.Fatalf("Open() = %v", err)
			}
			if c.origin != tt.origin {
				t.Errorf("origin = %q, want %q", c.origin, tt.origin)
			}
			if got := c.requestURL(nil); got != tt.origin+tt.root {
				t.Errorf("root URL = %q, want %q", got, tt.origin+tt.root)
			}
			if c.auth != tt.auth {
				t.Errorf("the authorization header is not the configured identity")
			}
			if red.Apply(c.auth) == c.auth {
				t.Errorf("the authorization header was not registered for redaction")
			}
		})
	}
}

// A connection bound to the whole Files root of an identity addresses it as a collection.
func TestTheWholeFilesRootIsAValidConnectionRoot(t *testing.T) {
	red := &redact.Redactor{}
	c, err := Open(resolvedConnection("all", "cloud-reader", aliceUserEnv, aliceTokenEnv, mainInstance, "/"),
		resolver(red), red)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if got := c.requestURL(nil); got != mainInstance+"/remote.php/dav/files/"+aliceUser+"/" {
		t.Errorf("root URL = %q", got)
	}
	if got := c.requestURL([]string{"Reports", "q1.pdf"}); got !=
		mainInstance+"/remote.php/dav/files/"+aliceUser+"/Reports/q1.pdf" {
		t.Errorf("path URL = %q", got)
	}
}

// An unusable service or target is refused when the connection is opened, before any request exists.
func TestUnusableServicesAndTargetsAreRefused(t *testing.T) {
	refuse(t)
	tests := []struct {
		name     string
		instance string
		target   string
	}{
		{"a service without transport security", "http://cloud.example.invalid", "Reports"},
		{"a service carrying userinfo", "https://user:pw@cloud.example.invalid", "Reports"},
		{"a service carrying a query", "https://cloud.example.invalid?a=b", "Reports"},
		{"a service carrying a fragment", "https://cloud.example.invalid#x", "Reports"},
		{"a service without a host", "https://", "Reports"},
		{"an installation path that traverses", "https://cloud.example.invalid/../x", "Reports"},
		{"a connection without a root folder", mainInstance, ""},
		{"a root folder that traverses", mainInstance, "Reports/../../etc"},
		{"a root folder with a percent encoding", mainInstance, "Reports%2f.."},
		{"a root folder with a backslash", mainInstance, `Reports\..`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			red := &redact.Redactor{}
			_, err := Open(resolvedConnection("x", "cloud-reader", aliceUserEnv, aliceTokenEnv,
				tt.instance, tt.target), resolver(red), red)
			if err == nil {
				t.Fatal("the connection was opened")
			}
			if got := classOf(err); got != provider.ClassProviderError {
				t.Errorf("class = %q, want a provider error", got)
			}
		})
	}
}

// An unusable identity is refused as an authentication problem, so nothing reaches a header or a path.
func TestUnusableIdentitiesAreRefused(t *testing.T) {
	refuse(t)
	tests := map[string]struct{ user, password string }{
		"a user ID carrying a separator":           {"alice/../bob", aliceToken},
		"a user ID carrying the basic-auth colon":  {"alice:bob", aliceToken},
		"a relative user ID":                       {"..", aliceToken},
		"an app password with a control character": {aliceUser, "abcdefgh\ni"},
		"an app password that is too short":        {aliceUser, "abc"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			red := &redact.Redactor{}
			secrets := secret.NewWith(func(env string) string {
				if env == aliceUserEnv {
					return tt.user
				}
				return tt.password
			}, nil, nil, red)
			_, err := Open(resolvedConnection("x", "cloud-reader", aliceUserEnv, aliceTokenEnv,
				mainInstance, "Reports"), secrets, red)
			if got := classOf(err); got != provider.ClassAuth {
				t.Fatalf("class = %q, want an authentication problem, error %v", got, err)
			}
		})
	}
}

// The list operation is exactly one PROPFIND of depth 1 inside the connection root, and it reports only
// the immediate children: the folder itself is normalised out of them.
func TestListFilesSendsOneDepthOnePropfindAndReportsOnlyChildren(t *testing.T) {
	calls := serve(t, func(*http.Request) (*http.Response, error) {
		return xmlResponse(http.StatusMultiStatus, reportsListing), nil
	})
	c, _ := client(t)

	result, err := c.ListFiles(context.Background(), "")
	if err != nil {
		t.Fatalf("ListFiles() = %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %+v, want exactly one request", *calls)
	}
	request := (*calls)[0]
	if request.method != methodPropfind || request.depth != depthChildren {
		t.Errorf("request = %s depth %q, want a PROPFIND of depth 1", request.method, request.depth)
	}
	if request.url.String() != mainInstance+aliceRoot {
		t.Errorf("request URL = %q, want the connection root", request.url)
	}
	if request.body != propfindBody {
		t.Errorf("request body = %q, want the fixed property set", request.body)
	}
	if request.auth != basicAuth(aliceUser, aliceToken) {
		t.Error("the request did not carry the identity of the connection")
	}

	if result.Path != "" || result.Count != 3 || len(result.Entries) != 3 {
		t.Fatalf("result = %+v, want the three children of the root", result)
	}
	want := []Entry{
		{
			Path: "2026", Name: "2026", Type: typeFolder, Size: 2048,
			ModifiedAt: "2026-03-02T11:15:00Z", ETag: "60f1c8a2e4b19", FileID: "1002", Readable: true,
		},
		{
			Path: nameCanary + ".txt", Name: nameCanary + ".txt", Type: typeFile,
			ContentType: "application/pdf", Size: 12, ModifiedAt: "2026-04-01T07:05:00Z",
			ETag: "3a71bd0c1", FileID: "1004", Readable: true,
		},
		{
			Path: "q1.pdf", Name: "q1.pdf", Type: typeFile, ContentType: "application/pdf", Size: 5120,
			ModifiedAt: "2026-04-01T07:05:00Z", ETag: "3a71bd0c1", FileID: "1003", Readable: true,
		},
	}
	if !reflect.DeepEqual(result.Entries, want) {
		t.Errorf("entries = %+v, want %+v", result.Entries, want)
	}
}

// A path argument stays below the connection root and never replaces it.
func TestListFilesResolvesAPathBelowTheConnectionRoot(t *testing.T) {
	calls := serve(t, func(*http.Request) (*http.Response, error) {
		return xmlResponse(http.StatusMultiStatus, multistatus(
			folderXML(aliceRoot+"/2026/", "2026", "1002", "2048"),
			fileXML(aliceRoot+"/2026/q1.pdf", "q1.pdf", "1003", "5120"),
		)), nil
	})
	c, _ := client(t)

	result, err := c.ListFiles(context.Background(), "2026")
	if err != nil {
		t.Fatalf("ListFiles() = %v", err)
	}
	if (*calls)[0].url.String() != mainInstance+aliceRoot+"/2026" {
		t.Fatalf("request URL = %q", (*calls)[0].url)
	}
	if result.Path != "2026" || len(result.Entries) != 1 || result.Entries[0].Path != "2026/q1.pdf" {
		t.Fatalf("result = %+v, want the child of the requested folder", result)
	}
}

// The stat operation is exactly one PROPFIND of depth 0 for the resolved path and answers one node.
func TestStatFileSendsOneDepthZeroPropfind(t *testing.T) {
	calls := serve(t, func(*http.Request) (*http.Response, error) {
		return xmlResponse(http.StatusMultiStatus,
			multistatus(fileXML(aliceRoot+"/2026/q1.pdf", "q1.pdf", "1003", "5120"))), nil
	})
	c, _ := client(t)

	entry, err := c.StatFile(context.Background(), "2026/q1.pdf")
	if err != nil {
		t.Fatalf("StatFile() = %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].depth != depthSelf ||
		(*calls)[0].url.String() != mainInstance+aliceRoot+"/2026/q1.pdf" {
		t.Fatalf("calls = %+v, want one depth 0 PROPFIND of the resolved path", *calls)
	}
	want := &Entry{
		Path: "2026/q1.pdf", Name: "q1.pdf", Type: typeFile, ContentType: "application/pdf",
		Size: 5120, ModifiedAt: "2026-04-01T07:05:00Z", ETag: "3a71bd0c1", FileID: "1003", Readable: true,
	}
	if !reflect.DeepEqual(entry, want) {
		t.Errorf("entry = %+v, want %+v", entry, want)
	}
}

// A name that needs encoding is encoded exactly once, and a listing round-trips through the stat
// operation without a second encoding.
func TestPathSegmentsAreEncodedExactlyOnce(t *testing.T) {
	const folder = "Berichte & Zahlen"
	calls := serve(t, func(*http.Request) (*http.Response, error) {
		return xmlResponse(http.StatusMultiStatus, multistatus(folderXML(
			aliceRoot+"/Berichte%20&amp;%20Zahlen/", "Berichte &amp; Zahlen", "1005", "10"))), nil
	})
	c, _ := client(t)

	entry, err := c.StatFile(context.Background(), folder)
	if err != nil {
		t.Fatalf("StatFile() = %v", err)
	}
	got := (*calls)[0].url
	if got.EscapedPath() != aliceRoot+"/Berichte%20&%20Zahlen" {
		t.Errorf("escaped path = %q, want one encoding of the segment", got.EscapedPath())
	}
	if got.Path != aliceRoot+"/"+folder {
		t.Errorf("decoded path = %q, want the literal segment", got.Path)
	}
	if entry.Path != folder || entry.Name != folder {
		t.Errorf("entry = %q / %q, want the decoded segment", entry.Path, entry.Name)
	}
}

// Everything that could leave the connection root is refused before a request is built.
func TestPathsOutsideTheConnectionRootAreRefusedBeforeAnyRequest(t *testing.T) {
	refuse(t)
	c, _ := client(t)

	paths := map[string]string{
		"a parent traversal":            "../bob",
		"a traversal in the middle":     "2026/../../Audit",
		"a current directory component": "./2026",
		"an absolute path":              "/remote.php/dav/files/" + bobUser + "/Audit",
		"an absolute URL":               "https://evil.example.invalid/x",
		"a protocol relative URL":       "//evil.example.invalid/x",
		"an encoded separator":          "2026%2F..%2FAudit",
		"a double encoded separator":    "2026%252F..",
		"an encoded parent":             "%2e%2e/Audit",
		"a backslash separator":         `2026\..\Audit`,
		"an empty component":            "2026//q1.pdf",
		"a trailing separator":          "2026/",
		"a control character":           "2026/q\x001.pdf",
		"a path that is too long":       strings.Repeat("a", maxPathLength+1),
		"too many components":           strings.Repeat("a/", maxSegments) + "a",
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			if _, err := c.ListFiles(context.Background(), path); err == nil {
				t.Error("the list operation accepted the path")
			} else if classOf(err) != provider.ClassProviderError {
				t.Errorf("class = %q", classOf(err))
			}
			if _, err := c.StatFile(context.Background(), path); err == nil {
				t.Error("the stat operation accepted the path")
			}
		})
	}
}

// This adapter produces exactly one HTTP method. A server can therefore receive no writing and no
// content-delivering request through it.
func TestTheAdapterProducesOnlyPropfind(t *testing.T) {
	calls := serve(t, func(*http.Request) (*http.Response, error) {
		return xmlResponse(http.StatusMultiStatus, reportsListing), nil
	})
	c, _ := client(t)

	if _, err := c.ListFiles(context.Background(), ""); err != nil {
		t.Fatalf("ListFiles() = %v", err)
	}
	if _, err := c.StatFile(context.Background(), "q1.pdf"); err == nil {
		// The listing fixture answers with more than one node, which stat refuses; the request itself is
		// what this test inspects.
		t.Log("stat accepted the listing fixture")
	}
	if _, err := c.testConnection(context.Background()); err != nil {
		t.Fatalf("testConnection() = %v", err)
	}
	if len(*calls) != 3 {
		t.Fatalf("calls = %d, want one request per operation", len(*calls))
	}
	forbidden := []string{
		http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete, "MKCOL", "MOVE", "COPY",
		"PROPPATCH", "LOCK", "UNLOCK", "REPORT", "SEARCH",
	}
	for _, request := range *calls {
		if request.method != methodPropfind {
			t.Errorf("method = %q, want only PROPFIND", request.method)
		}
		for _, method := range forbidden {
			if request.method == method {
				t.Errorf("the adapter produced %q", method)
			}
		}
	}
}

// A credential-carrying redirect stays on the configured origin and inside the Files root of the
// configured identity; everything else is refused instead of followed.
func TestRedirectsStayOnTheOriginAndInsideTheIdentityRoot(t *testing.T) {
	tests := []struct {
		name     string
		location string
		followed bool
	}{
		{"a redirect to another origin", "https://evil.example.invalid" + aliceRoot, false},
		{"a redirect to plain http", "http://cloud.example.invalid" + aliceRoot, false},
		{"a redirect to another identity", mainInstance + bobRoot, false},
		{"a redirect out of the Files root", mainInstance + "/index.php/apps/files", false},
		{"a redirect inside the identity root", mainInstance + aliceRoot + "/", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redirected := false
			calls := serve(t, func(request *http.Request) (*http.Response, error) {
				if !redirected {
					redirected = true
					return &http.Response{
						StatusCode: http.StatusTemporaryRedirect,
						Header:     http.Header{"Location": []string{tt.location}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				}
				return xmlResponse(http.StatusMultiStatus, reportsListing), nil
			})
			c, red := client(t)

			_, err := c.ListFiles(context.Background(), "")
			if tt.followed {
				if err != nil {
					t.Fatalf("ListFiles() = %v, want the same-origin redirect to be followed", err)
				}
				if len(*calls) != 2 {
					t.Errorf("calls = %d, want the redirect to be followed", len(*calls))
				}
				return
			}
			if err == nil {
				t.Fatal("the redirect was followed")
			}
			if classOf(err) != provider.ClassProviderError {
				t.Errorf("class = %q, want a refused redirect", classOf(err))
			}
			for _, canary := range []string{aliceToken, tt.location} {
				if strings.Contains(red.Error(err), canary) {
					t.Errorf("the error carries %q: %v", canary, err)
				}
			}
			if len(*calls) != 1 {
				t.Errorf("calls = %d, want the credential to travel once", len(*calls))
			}
		})
	}
}

// An answer that is too large, too deep, or holds too many nodes is refused before a single value is
// handed on.
func TestOversizedMultiStatusAnswersAreRefused(t *testing.T) {
	entries := make([]string, 0, maxEntries+1)
	for i := 0; i <= maxEntries; i++ {
		entries = append(entries, fileXML(fmt.Sprintf("%s/f%d.txt", aliceRoot, i),
			fmt.Sprintf("f%d.txt", i), "1", "1"))
	}
	deep := multistatus(`<d:response><d:href>` + aliceRoot + `</d:href><d:propstat><d:prop>` +
		strings.Repeat("<d:x>", maxXMLDepth) + strings.Repeat("</d:x>", maxXMLDepth) +
		`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)

	tests := map[string]string{
		"more nodes than one read may report": multistatus(
			append([]string{folderXML(aliceRoot+"/", "Reports", "1001", "1")}, entries...)...),
		"a body beyond the size limit": strings.Repeat("a", maxBodyBytes+1),
		"a document nested too deeply": deep,
		"a document that is not XML":   "not xml at all",
		"a document that is not a multi-status": `<?xml version="1.0"?>` +
			`<d:error xmlns:d="DAV:"><s:message xmlns:s="http://sabredav.org/ns">` + bodyCanary +
			`</s:message></d:error>`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			serve(t, func(*http.Request) (*http.Response, error) {
				return xmlResponse(http.StatusMultiStatus, body), nil
			})
			c, red := client(t)

			_, err := c.ListFiles(context.Background(), "")
			if err == nil {
				t.Fatal("the answer was accepted")
			}
			if classOf(err) != provider.ClassInvalidResponse {
				t.Errorf("class = %q, want an invalid provider response", classOf(err))
			}
			if strings.Contains(red.Error(err), bodyCanary) {
				t.Errorf("the error carries the provider body: %v", err)
			}
		})
	}
}

// A multi-status answer mixes successful and refused nodes. A refused child is left out, and a refused
// request node becomes the classified error of the whole read.
func TestResourceLevelFailuresAreEvaluated(t *testing.T) {
	t.Run("a refused child is not reported", func(t *testing.T) {
		serve(t, func(*http.Request) (*http.Response, error) {
			return xmlResponse(http.StatusMultiStatus, multistatus(
				folderXML(aliceRoot+"/", "Reports", "1001", "1"),
				fileXML(aliceRoot+"/q1.pdf", "q1.pdf", "1003", "5120"),
				`<d:response><d:href>`+aliceRoot+`/secret.pdf</d:href>`+
					`<d:status>HTTP/1.1 403 Forbidden</d:status></d:response>`,
				`<d:response><d:href>`+aliceRoot+`/gone.pdf</d:href><d:propstat><d:prop>`+
					`<d:displayname/></d:prop><d:status>HTTP/1.1 404 Not Found</d:status>`+
					`</d:propstat></d:response>`,
			)), nil
		})
		c, _ := client(t)

		result, err := c.ListFiles(context.Background(), "")
		if err != nil {
			t.Fatalf("ListFiles() = %v", err)
		}
		if result.Count != 1 || result.Entries[0].Path != "q1.pdf" {
			t.Fatalf("entries = %+v, want only the readable child", result.Entries)
		}
	})

	t.Run("a refused request node is classified", func(t *testing.T) {
		serve(t, func(*http.Request) (*http.Response, error) {
			return xmlResponse(http.StatusMultiStatus, multistatus(
				`<d:response><d:href>`+aliceRoot+`</d:href>`+
					`<d:status>HTTP/1.1 403 Forbidden</d:status></d:response>`)), nil
		})
		c, _ := client(t)

		_, err := c.StatFile(context.Background(), "")
		if classOf(err) != provider.ClassPermission {
			t.Fatalf("class = %q, want a permission problem", classOf(err))
		}
	})

	t.Run("a property block that failed contributes nothing", func(t *testing.T) {
		serve(t, func(*http.Request) (*http.Response, error) {
			return xmlResponse(http.StatusMultiStatus, multistatus(
				`<d:response><d:href>`+aliceRoot+`/q1.pdf</d:href>`+
					`<d:propstat><d:prop><d:resourcetype/><d:getcontentlength>5</d:getcontentlength>`+
					`<oc:permissions>RGDNVW</oc:permissions></d:prop>`+
					`<d:status>HTTP/1.1 200 OK</d:status></d:propstat>`+
					`<d:propstat><d:prop><d:displayname>`+bodyCanary+`</d:displayname>`+
					`<d:resourcetype><d:collection/></d:resourcetype></d:prop>`+
					`<d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response>`)), nil
		})
		c, _ := client(t)

		entry, err := c.StatFile(context.Background(), "q1.pdf")
		if err != nil {
			t.Fatalf("StatFile() = %v", err)
		}
		if entry.Type != typeFile || entry.Name != "q1.pdf" || entry.Size != 5 {
			t.Fatalf("entry = %+v, want the successful property block only", entry)
		}
		if strings.Contains(entry.Name, bodyCanary) {
			t.Errorf("a refused property reached the result: %+v", entry)
		}
	})
}

// A node the server places outside the folder that was addressed is refused, whatever it claims to be.
func TestForeignNodesAreRefused(t *testing.T) {
	tests := map[string]string{
		"a node of another identity": multistatus(
			folderXML(aliceRoot+"/", "Reports", "1001", "1"),
			fileXML(bobRoot+"/salary.pdf", "salary.pdf", "9", "9")),
		"a node above the connection root": multistatus(
			folderXML(aliceRoot+"/", "Reports", "1001", "1"),
			fileXML("/remote.php/dav/files/"+aliceUser+"/private.pdf", "private.pdf", "9", "9")),
		"a node of another instance": multistatus(
			folderXML(aliceRoot+"/", "Reports", "1001", "1"),
			fileXML("https://evil.example.invalid"+aliceRoot+"/x.pdf", "x.pdf", "9", "9")),
		"a node two levels below": multistatus(
			folderXML(aliceRoot+"/", "Reports", "1001", "1"),
			fileXML(aliceRoot+"/2026/q1.pdf", "q1.pdf", "9", "9")),
		"a node whose location traverses": multistatus(
			folderXML(aliceRoot+"/", "Reports", "1001", "1"),
			fileXML(aliceRoot+"/../../"+bobUser+"/Audit/x.pdf", "x.pdf", "9", "9")),
		"a node without a location": multistatus(
			folderXML(aliceRoot+"/", "Reports", "1001", "1"),
			fileXML("", "x.pdf", "9", "9")),
		"a listing without the requested folder": multistatus(
			fileXML(aliceRoot+"/q1.pdf", "q1.pdf", "9", "9")),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			serve(t, func(*http.Request) (*http.Response, error) {
				return xmlResponse(http.StatusMultiStatus, body), nil
			})
			c, _ := client(t)

			if _, err := c.ListFiles(context.Background(), ""); classOf(err) != provider.ClassInvalidResponse {
				t.Fatalf("class = %q, want an invalid provider response, error %v", classOf(err), err)
			}
		})
	}
}

// Every provider status becomes a stable class, and no provider body reaches the message.
func TestProviderStatusesAreClassified(t *testing.T) {
	tests := map[int]provider.Class{
		http.StatusUnauthorized:        provider.ClassAuth,
		http.StatusForbidden:           provider.ClassPermission,
		http.StatusNotFound:            provider.ClassProviderError,
		http.StatusMethodNotAllowed:    provider.ClassProviderError,
		http.StatusLocked:              provider.ClassProviderError,
		http.StatusInsufficientStorage: provider.ClassProviderError,
		http.StatusTooManyRequests:     provider.ClassRateLimited,
		http.StatusServiceUnavailable:  provider.ClassUnreachable,
		http.StatusGatewayTimeout:      provider.ClassTimeout,
		http.StatusInternalServerError: provider.ClassProviderError,
		http.StatusOK:                  provider.ClassProviderError,
	}
	for status, want := range tests {
		t.Run(http.StatusText(status), func(t *testing.T) {
			serve(t, func(*http.Request) (*http.Response, error) {
				return xmlResponse(status, `<?xml version="1.0"?><d:error xmlns:d="DAV:">`+
					`<s:message xmlns:s="http://sabredav.org/ns">`+bodyCanary+`</s:message></d:error>`), nil
			})
			c, red := client(t)

			_, err := c.ListFiles(context.Background(), "")
			if got := classOf(err); got != want {
				t.Fatalf("class = %q, want %q", got, want)
			}
			for _, canary := range []string{bodyCanary, aliceToken, aliceRoot} {
				if strings.Contains(red.Error(err), canary) {
					t.Errorf("the error carries %q: %v", canary, err)
				}
			}
		})
	}
}

// A transport failure is classified without copying the original text, which could carry a URL.
func TestTransportFailuresAreClassified(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp " + mainInstance + ": connection refused")
	})
	c, red := client(t)

	_, err := c.ListFiles(context.Background(), "")
	if classOf(err) != provider.ClassUnreachable {
		t.Fatalf("class = %q, want unreachable", classOf(err))
	}
	if strings.Contains(red.Error(err), "connection refused") {
		t.Errorf("the error copies the transport message: %v", err)
	}
}

// The connection test is one read-only PROPFIND that proves the instance, the identity, and the fixed
// root folder together.
func TestConnectionTestChecksTheRootWithoutWriting(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		status   int
		header   http.Header
		want     provider.Class
		wantText string
	}{
		{
			name: "a readable root folder", status: http.StatusMultiStatus,
			body: multistatus(folderXML(aliceRoot+"/", "Reports", "1001", "1")),
			want: provider.ClassOK,
		},
		{
			name: "a rejected app password", status: http.StatusUnauthorized, body: "",
			want: provider.ClassAuth,
		},
		{
			name: "a root folder the identity may not read", status: http.StatusForbidden, body: "",
			want: provider.ClassPermission,
		},
		{
			name: "a missing root folder", status: http.StatusNotFound, body: "",
			wantText: "does not hold the root folder",
		},
		{
			name: "a root that is a file", status: http.StatusMultiStatus,
			body:     multistatus(fileXML(aliceRoot, "Reports", "1001", "1")),
			wantText: "is a file, not a folder",
		},
		{
			name: "a root without the read permission", status: http.StatusMultiStatus,
			body: multistatus(`<d:response><d:href>` + aliceRoot + `</d:href><d:propstat><d:prop>` +
				`<d:resourcetype><d:collection/></d:resourcetype>` +
				`<oc:permissions>S</oc:permissions></d:prop>` +
				`<d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`),
			wantText: "may not read the configured root folder",
		},
		{
			name: "a server without the WebDAV class the Files app needs", status: http.StatusMultiStatus,
			body:   multistatus(folderXML(aliceRoot+"/", "Reports", "1001", "1")),
			header: http.Header{"Dav": []string{"2"}},
			want:   provider.ClassInvalidResponse,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := serve(t, func(*http.Request) (*http.Response, error) {
				response := xmlResponse(tt.status, tt.body)
				if tt.header != nil {
					response.Header = tt.header
				}
				return response, nil
			})
			red := &redact.Redactor{}
			class, err := TestConnection(context.Background(),
				resolvedConnection("reports", "cloud-reader", aliceUserEnv, aliceTokenEnv,
					mainInstance, "Reports"), resolver(red), red)

			if tt.wantText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantText) {
					t.Fatalf("error = %v, want %q", err, tt.wantText)
				}
			} else {
				if err != nil {
					t.Fatalf("TestConnection() = %v", err)
				}
				if class != tt.want {
					t.Fatalf("class = %q, want %q", class, tt.want)
				}
			}
			if len(*calls) != 1 || (*calls)[0].method != methodPropfind || (*calls)[0].depth != depthSelf {
				t.Errorf("calls = %+v, want one depth 0 PROPFIND", *calls)
			}
		})
	}
}

// Two instances and two identities of one instance never mix: every request carries the identity of its
// own connection and stays inside the root folder that connection is bound to.
func TestInstancesIdentitiesAndRootsStaySeparated(t *testing.T) {
	calls := serve(t, func(request *http.Request) (*http.Response, error) {
		root := request.URL.EscapedPath()
		return xmlResponse(http.StatusMultiStatus, multistatus(
			folderXML(root+"/", "root", "1", "1"),
			fileXML(root+"/note.txt", "note.txt", "2", "2"))), nil
	})

	connections := []struct {
		connection *config.Resolved
		origin     string
		root       string
		auth       string
	}{
		{resolvedConnection("reports", "cloud-reader", aliceUserEnv, aliceTokenEnv, mainInstance, "Reports"),
			mainInstance, aliceRoot, basicAuth(aliceUser, aliceToken)},
		{resolvedConnection("audit", "cloud-auditor", bobUserEnv, bobTokenEnv, mainInstance, "Audit"),
			mainInstance, bobRoot, basicAuth(bobUser, bobToken)},
		{resolvedConnection("partner", "partner-reader", carolUserEnv, carolTokenEnv, partnerInstance,
			"Shared/Callbell"), partnerOrigin, carolRoot, basicAuth(carolUser, carolToken)},
		{resolvedConnection("archive", "cloud-reader", aliceUserEnv, aliceTokenEnv, mainInstance,
			"Archive/2026"), mainInstance, archiveRoot, basicAuth(aliceUser, aliceToken)},
	}
	for _, tt := range connections {
		red := &redact.Redactor{}
		c, err := Open(tt.connection, resolver(red), red)
		if err != nil {
			t.Fatalf("Open(%s) = %v", tt.connection.Name, err)
		}
		result, err := c.ListFiles(context.Background(), "")
		if err != nil {
			t.Fatalf("ListFiles(%s) = %v", tt.connection.Name, err)
		}
		if len(result.Entries) != 1 || result.Entries[0].Path != "note.txt" {
			t.Errorf("%s entries = %+v", tt.connection.Name, result.Entries)
		}
	}

	if len(*calls) != len(connections) {
		t.Fatalf("calls = %d, want one per connection", len(*calls))
	}
	seen := map[string]bool{}
	for i, request := range *calls {
		want := connections[i]
		if request.url.String() != want.origin+want.root {
			t.Errorf("request %d = %q, want %q", i, request.url, want.origin+want.root)
		}
		if request.auth != want.auth {
			t.Errorf("request %d did not carry the identity of its connection", i)
		}
		if seen[request.url.String()] {
			t.Errorf("two connections addressed the same root %q", request.url)
		}
		seen[request.url.String()] = true
	}
	// The two identities of the same instance used different app passwords.
	if (*calls)[0].auth == (*calls)[1].auth {
		t.Error("both identities of the instance used the same credential")
	}
	// The same credential served two connections without moving either root.
	if (*calls)[0].auth != (*calls)[3].auth || (*calls)[0].url.String() == (*calls)[3].url.String() {
		t.Error("a reused credential did not keep two separate roots")
	}
}

// Both operations satisfy their contract through the application core, over the same connections the
// configuration defines.
func TestOperationsSatisfyTheirContractThroughTheApplicationCore(t *testing.T) {
	serve(t, func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Depth") == depthSelf {
			return xmlResponse(http.StatusMultiStatus,
				multistatus(fileXML(request.URL.EscapedPath(), "q1.pdf", "1003", "5120"))), nil
		}
		return xmlResponse(http.StatusMultiStatus, reportsListing), nil
	})

	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)

	list, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "nextcloud.files.list", Connection: "reports", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("invoke list = %v", err)
	}
	var listed ListResult
	if err := json.Unmarshal(list.Result, &listed); err != nil {
		t.Fatalf("list result = %s: %v", list.Result, err)
	}
	if listed.Count != 3 || listed.Entries[0].Path != "2026" || listed.Entries[0].Type != typeFolder {
		t.Errorf("list result = %s", list.Result)
	}
	if strings.Contains(string(list.Result), "has-preview") {
		t.Errorf("a property outside the fixed set reached the result: %s", list.Result)
	}

	stat, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "nextcloud.files.stat", Connection: "audit",
		Arguments: json.RawMessage(`{"path":"2026/q1.pdf"}`),
	})
	if err != nil {
		t.Fatalf("invoke stat = %v", err)
	}
	if !strings.Contains(string(stat.Result), `"path":"2026/q1.pdf"`) ||
		!strings.Contains(string(stat.Result), `"type":"file"`) {
		t.Errorf("stat result = %s", stat.Result)
	}
}

// The core refuses what the contract does not allow before a provider is contacted, and it never guesses
// an instance, an identity, or a root folder.
func TestTheCoreRefusesUnsupportedRequestsBeforeProviderIO(t *testing.T) {
	refuse(t)
	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)

	tests := []struct {
		name    string
		request application.InvokeRequest
	}{
		{"several connections without an explicit one", application.InvokeRequest{
			Operation: "nextcloud.files.list", Arguments: json.RawMessage(`{}`)}},
		{"an instance as an argument", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "reports",
			Arguments: json.RawMessage(`{"base_url":"https://evil.example.invalid"}`)}},
		{"an identity as an argument", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "reports",
			Arguments: json.RawMessage(`{"user":"` + bobUser + `"}`)}},
		{"a root folder as an argument", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "reports",
			Arguments: json.RawMessage(`{"root":"/"}`)}},
		{"a depth as an argument", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "reports",
			Arguments: json.RawMessage(`{"depth":"infinity"}`)}},
		{"an absolute path", application.InvokeRequest{
			Operation: "nextcloud.files.stat", Connection: "reports",
			Arguments: json.RawMessage(`{"path":"/remote.php/dav/files/` + bobUser + `/Audit"}`)}},
		{"an absolute URL", application.InvokeRequest{
			Operation: "nextcloud.files.stat", Connection: "reports",
			Arguments: json.RawMessage(`{"path":"https://evil.example.invalid/x"}`)}},
		{"a traversal", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "reports",
			Arguments: json.RawMessage(`{"path":"../Audit"}`)}},
		{"a traversal in the middle", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "reports",
			Arguments: json.RawMessage(`{"path":"2026/../../Audit"}`)}},
		{"an encoded separator", application.InvokeRequest{
			Operation: "nextcloud.files.stat", Connection: "reports",
			Arguments: json.RawMessage(`{"path":"2026%2F..%2FAudit"}`)}},
		{"a double encoded separator", application.InvokeRequest{
			Operation: "nextcloud.files.stat", Connection: "reports",
			Arguments: json.RawMessage(`{"path":"2026%252F.."}`)}},
		{"a backslash separator", application.InvokeRequest{
			Operation: "nextcloud.files.stat", Connection: "reports",
			Arguments: json.RawMessage(`{"path":"2026\\..\\Audit"}`)}},
		{"an empty component", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "reports",
			Arguments: json.RawMessage(`{"path":"2026//q1.pdf"}`)}},
		{"a trailing separator", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "reports",
			Arguments: json.RawMessage(`{"path":"2026/"}`)}},
		{"an empty path", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "reports",
			Arguments: json.RawMessage(`{"path":""}`)}},
		{"a path beyond the length bound", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "reports",
			Arguments: json.RawMessage(`{"path":"` + strings.Repeat("a", maxPathLength+1) + `"}`)}},
		{"a connection of another provider", application.InvokeRequest{
			Operation: "nextcloud.files.list", Connection: "wiki", Arguments: json.RawMessage(`{}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := core.Invoke(context.Background(), tt.request); err == nil {
				t.Fatal("the core accepted the request")
			}
		})
	}
}

// No credential and no provider content reaches a result, a diagnostic, or an error, whatever the
// instance answers.
func TestCredentialsAndProviderDataNeverReachTheOutput(t *testing.T) {
	serve(t, func(*http.Request) (*http.Response, error) {
		return xmlResponse(http.StatusMultiStatus, multistatus(
			folderXML(aliceRoot+"/", aliceToken, "1001", "1"),
			fileXML(aliceRoot+"/note.txt", "Basic "+aliceToken, "1002", "2"))), nil
	})

	red := &redact.Redactor{}
	core := application.New(registry(t), coreConfig(), resolver(red), red)
	response, err := core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "nextcloud.files.list", Connection: "reports", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("invoke = %v", err)
	}
	if strings.Contains(string(response.Result), aliceToken) {
		t.Errorf("the result carries the app password: %s", response.Result)
	}
	if !strings.Contains(string(response.Result), redact.Marker) {
		t.Errorf("the result was not redacted: %s", response.Result)
	}
	var value any
	if err := json.Unmarshal(response.Result, &value); err != nil {
		t.Errorf("the redacted result is not valid JSON: %v", err)
	}

	// A failing read reports a class, never the document or the path the instance answered with.
	serve(t, func(*http.Request) (*http.Response, error) {
		return xmlResponse(http.StatusBadRequest, `<?xml version="1.0"?><d:error xmlns:d="DAV:">`+
			`<s:message xmlns:s="http://sabredav.org/ns">`+bodyCanary+` `+nameCanary+
			` `+aliceRoot+`</s:message></d:error>`), nil
	})
	_, err = core.Invoke(context.Background(), application.InvokeRequest{
		Operation: "nextcloud.files.list", Connection: "reports", Arguments: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("the failing read was reported as a result")
	}
	for _, canary := range []string{bodyCanary, nameCanary, aliceToken, aliceRoot} {
		if strings.Contains(red.Error(err), canary) {
			t.Errorf("the error carries the canary %q: %v", canary, err)
		}
	}
}

// registry returns a registry with Nextcloud and one foreign provider, so a wrong route is provable.
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

// coreConfig configures two Nextcloud instances, two identities of the first instance, one credential
// reused by two connections, and one foreign connection.
func coreConfig() *config.Config {
	credential := func(userEnv, tokenEnv string) config.Credential {
		return config.Credential{
			Type: config.CredentialTypeEnv,
			Values: map[string]string{
				roleUserID: userEnv, roleAppPassword: tokenEnv,
			},
		}
	}
	return &config.Config{
		Version: 1,
		Services: map[string]config.Service{
			"cloud-main":    {Provider: Provider, BaseURL: mainInstance},
			"cloud-partner": {Provider: Provider, BaseURL: partnerInstance},
			"wiki":          {Provider: "wiki", BaseURL: "https://wiki.example.invalid"},
		},
		Credentials: map[string]config.Credential{
			"cloud-reader":   credential(aliceUserEnv, aliceTokenEnv),
			"cloud-auditor":  credential(bobUserEnv, bobTokenEnv),
			"partner-reader": credential(carolUserEnv, carolTokenEnv),
			"wiki-reader":    {Type: config.CredentialTypeEnv, Values: map[string]string{"token-id": "WIKI_ID"}},
		},
		Connections: map[string]config.Connection{
			"reports": {Service: "cloud-main", Credential: "cloud-reader", Target: "Reports"},
			"archive": {Service: "cloud-main", Credential: "cloud-reader", Target: "Archive/2026"},
			"audit":   {Service: "cloud-main", Credential: "cloud-auditor", Target: "Audit"},
			"partner": {Service: "cloud-partner", Credential: "partner-reader", Target: "Shared/Callbell"},
			"wiki":    {Service: "wiki", Credential: "wiki-reader"},
		},
	}
}
