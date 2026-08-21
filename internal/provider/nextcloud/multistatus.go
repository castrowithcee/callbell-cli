package nextcloud

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// The two namespaces this adapter reads. DAV: carries the standard properties, and the ownCloud namespace
// carries the file identifier, the folder size, and the effective permissions Nextcloud adds to them.
const (
	davNS = "DAV:"
	ocNS  = "http://owncloud.org/ns"
)

// The elements of a multi-status answer this parser navigates by.
var (
	elemMultistatus  = xml.Name{Space: davNS, Local: "multistatus"}
	elemResponse     = xml.Name{Space: davNS, Local: "response"}
	elemHref         = xml.Name{Space: davNS, Local: "href"}
	elemPropstat     = xml.Name{Space: davNS, Local: "propstat"}
	elemProp         = xml.Name{Space: davNS, Local: "prop"}
	elemStatus       = xml.Name{Space: davNS, Local: "status"}
	elemResourceType = xml.Name{Space: davNS, Local: "resourcetype"}
	elemCollection   = xml.Name{Space: davNS, Local: "collection"}
)

// The keys the fixed property set is normalised under.
const (
	propDisplayName   = "displayname"
	propContentType   = "contenttype"
	propContentLength = "contentlength"
	propLastModified  = "lastmodified"
	propETag          = "etag"
	propFileID        = "fileid"
	propSize          = "size"
	propPermissions   = "permissions"
)

// wantedProps maps the requested properties to their keys. Every other property of an answer is dropped,
// so a server cannot widen the result by returning more than it was asked for.
var wantedProps = map[xml.Name]string{
	{Space: davNS, Local: "displayname"}:      propDisplayName,
	{Space: davNS, Local: "getcontenttype"}:   propContentType,
	{Space: davNS, Local: "getcontentlength"}: propContentLength,
	{Space: davNS, Local: "getlastmodified"}:  propLastModified,
	{Space: davNS, Local: "getetag"}:          propETag,
	{Space: ocNS, Local: "fileid"}:            propFileID,
	{Space: ocNS, Local: "size"}:              propSize,
	{Space: ocNS, Local: "permissions"}:       propPermissions,
}

// resource is one d:response of a multi-status answer, reduced to what this adapter reads: where the node
// is, whether it is a collection, the properties a successful propstat reported, and the resource-level
// status a server uses to refuse one node of many.
type resource struct {
	href       string
	status     string
	props      map[string]string
	collection bool
	// read records that at least one propstat of this resource answered with a 2xx status.
	read bool
}

// failure reports why this resource carries no usable metadata. A multi-status answer mixes successful
// and refused nodes, so every node is checked before any of its data is used.
func (r *resource) failure(op string) error {
	if code, ok := statusCodeOf(r.status); ok && (code < 200 || code >= 300) {
		return statusError(op, code)
	}
	if !r.read {
		return invalidResponse(op, "Nextcloud answered without readable properties for this node")
	}
	return nil
}

// parseMultiStatus reads a 207 answer with a bounded token walk rather than a recursive unmarshal, so a
// hostile document can exhaust neither the stack nor the memory of this process. Depth, element count,
// and text length are all capped, and anything that does not match the documented shape is refused before
// a single value is handed on.
func parseMultiStatus(op string, body []byte) ([]resource, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true

	var (
		stack     []xml.Name
		resources []resource
		current   *resource
		pending   map[string]string
		collected bool
		status    string
		text      strings.Builder
	)

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, invalidResponse(op, "Nextcloud returned an invalid multi-status document")
		}

		switch element := token.(type) {
		case xml.StartElement:
			if len(stack) >= maxXMLDepth {
				return nil, invalidResponse(op, "the Nextcloud response is nested too deeply")
			}
			stack = append(stack, element.Name)
			text.Reset()
			switch {
			case len(stack) == 1 && element.Name != elemMultistatus:
				return nil, invalidResponse(op, "Nextcloud did not answer with a multi-status document")
			case len(stack) == 2 && element.Name == elemResponse:
				if len(resources) >= maxEntries {
					return nil, invalidResponse(op,
						"this Nextcloud folder holds more entries than one read may report")
				}
				current = &resource{props: map[string]string{}}
			case len(stack) == 3 && current != nil && element.Name == elemPropstat:
				pending, collected, status = map[string]string{}, false, ""
			case len(stack) == 6 && pending != nil && stack[4] == elemResourceType &&
				element.Name == elemCollection:
				collected = true
			}

		case xml.CharData:
			if text.Len()+len(element) <= maxTextBytes {
				text.Write(element)
			}

		case xml.EndElement:
			switch {
			case len(stack) == 3 && current != nil && element.Name == elemHref:
				current.href = text.String()
			case len(stack) == 3 && current != nil && element.Name == elemStatus:
				current.status = text.String()
			case len(stack) == 3 && current != nil && element.Name == elemPropstat:
				// Properties become part of the resource only when their own propstat succeeded, so a
				// 404 block of a partially answered node cannot contribute a value.
				if code, ok := statusCodeOf(status); ok && code >= 200 && code < 300 {
					current.read = true
					current.collection = current.collection || collected
					for key, value := range pending {
						current.props[key] = value
					}
				}
				pending = nil
			case len(stack) == 4 && pending != nil && element.Name == elemStatus:
				status = text.String()
			case len(stack) == 5 && pending != nil && stack[3] == elemProp:
				if key, ok := wantedProps[element.Name]; ok {
					pending[key] = strings.TrimSpace(text.String())
				}
			case len(stack) == 2 && current != nil && element.Name == elemResponse:
				resources = append(resources, *current)
				current = nil
			}
			text.Reset()
			stack = stack[:len(stack)-1]
		}
	}

	if len(resources) == 0 {
		return nil, invalidResponse(op, "Nextcloud answered without a single node")
	}
	return resources, nil
}

// statusCodeOf reads the numeric code out of a WebDAV status line such as "HTTP/1.1 200 OK".
func statusCodeOf(line string) (int, bool) {
	for _, field := range strings.Fields(line) {
		if code, err := strconv.Atoi(field); err == nil {
			return code, true
		}
	}
	return 0, false
}

// relativeOf turns the location of one answered node into its path below the connection root. The href is
// untrusted: it is compared segment by segment against the origin, the installation path, the identity,
// and the fixed root of this connection, so a server cannot hand back a node outside them.
func (c *Client) relativeOf(op, href string) ([]string, error) {
	trimmed := strings.TrimSpace(href)
	if trimmed == "" {
		return nil, invalidResponse(op, "Nextcloud answered with a node without a location")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return nil, invalidResponse(op, "Nextcloud answered with an unusable node location")
	}
	if (parsed.Scheme != "" || parsed.Host != "") && parsed.Scheme+"://"+parsed.Host != c.origin {
		return nil, invalidResponse(op, "Nextcloud answered with a node of a different instance")
	}

	escaped := parsed.EscapedPath()
	if !strings.HasPrefix(escaped, "/") {
		return nil, invalidResponse(op, "Nextcloud answered with a relative node location")
	}
	// The escaped form is split before it is decoded, so a percent-encoded separator inside a name stays
	// inside that one segment instead of silently becoming a new path component.
	raw := strings.Split(strings.TrimSuffix(escaped, "/"), "/")[1:]
	if len(raw) > len(c.prefix)+len(c.root)+maxSegments {
		return nil, invalidResponse(op, "Nextcloud answered with a node location that is too deep")
	}

	segments := make([]string, 0, len(raw))
	for _, part := range raw {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return nil, invalidResponse(op, "Nextcloud answered with an unusable node location")
		}
		if err := checkResponseSegment(decoded); err != nil {
			return nil, invalidResponse(op, "Nextcloud answered with an unusable node location")
		}
		segments = append(segments, decoded)
	}

	bound := append(append([]string{}, c.prefix...), c.root...)
	if len(segments) < len(bound) || !equalSegments(segments[:len(bound)], bound) {
		return nil, invalidResponse(op, messageForeignEntry)
	}
	return segments[len(bound):], nil
}

// checkResponseSegment applies the path rules to provider data as well. A decoded separator, a relative
// component, or a control character in an href would move a node out of the connection root.
func checkResponseSegment(segment string) error {
	if segment == "" || segment == "." || segment == ".." || len(segment) > maxSegmentLen {
		return errors.New("unusable path component")
	}
	for _, r := range segment {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return errors.New("unusable path component")
		}
	}
	return nil
}
