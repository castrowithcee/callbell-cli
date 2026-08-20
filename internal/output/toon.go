package output

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file implements a TOON encoder for the JSON data model, following the TOON specification 4.1
// (reference state v4.1.1, https://github.com/toon-format/spec/blob/v4.1.1/SPEC.md). Section references
// in the comments below point at that document. Only encoding is implemented; TOON is never parsed.
//
// The encoder uses the specification defaults: two spaces per indentation level and the comma as the
// document delimiter (§13). Because the comma is also the active delimiter of every scope, delimiter-aware
// quoting (§11.1) never has to switch delimiters.

const (
	toonIndent    = "  "
	toonDelimiter = ","
)

var (
	// toonNumericLike is the numeric-like pattern of §7.2: a string matching it must be quoted so that it
	// does not decode as a number.
	toonNumericLike = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

	// toonUnquotedKey is the unquoted-key pattern of §7.3. Every other key is quoted.
	toonUnquotedKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

	// toonCanonicalInt matches an integer literal that is already canonical per §2.
	toonCanonicalInt = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)
)

// MarshalTOON encodes v as a TOON document. Accepted values are the JSON data model: nil, bool, string,
// the Go integer and float types, json.Number, Object and Collection, any slice or array, and any map with
// string keys. Object and Collection carry their own field order; map keys are sorted, because a Go map has
// no encounter order to preserve (§2). The result uses LF line endings and carries no trailing newline, as
// §12 requires; callers that print it to a terminal add the final newline themselves.
func MarshalTOON(v any) ([]byte, error) {
	e := &toonEncoder{}
	if err := e.root(v); err != nil {
		return nil, err
	}
	return []byte(e.b.String()), nil
}

// EncodeTOON writes v to w as a TOON document. See MarshalTOON for the accepted values.
func EncodeTOON(w io.Writer, v any) error {
	out, err := MarshalTOON(v)
	if err != nil {
		return err
	}
	_, err = w.Write(out)
	return err
}

// toonField is one entry of a tabular or keyed header's field list. A leaf field has no children; a field
// with children is a nested field group standing for a nested-uniform column (§9.3).
type toonField struct {
	name     string
	children []toonField
}

// toonEncoder accumulates the document. Lines are separated, not terminated, so the document never ends
// with a newline (§12).
type toonEncoder struct {
	b     strings.Builder
	wrote bool
}

// line appends one line. The caller passes the complete line content including its indentation.
func (e *toonEncoder) line(s string) {
	if e.wrote {
		e.b.WriteByte('\n')
	}
	e.b.WriteString(s)
	e.wrote = true
}

func toonPad(depth int) string {
	return strings.Repeat(toonIndent, depth)
}

// root writes the document's root form (§5). An empty root object yields an empty document (§8).
func (e *toonEncoder) root(v any) error {
	if fields, ok := toonObject(v); ok {
		if nodes, ok := toonKeyedFields(fields); ok {
			e.line(toonHeader("", len(fields), true, nodes))
			return e.entryRows(1, fields, nodes)
		}
		return e.fields(0, fields)
	}
	if elems, ok := toonArray(v); ok {
		if len(elems) == 0 {
			e.line("[]")
			return nil
		}
		if cells, ok, err := toonInlineCells(elems); ok || err != nil {
			if err != nil {
				return err
			}
			e.line(toonHeader("", len(elems), false, nil) + " " + strings.Join(cells, toonDelimiter))
			return nil
		}
		if nodes, ok := toonTableFields(elems); ok {
			e.line(toonHeader("", len(elems), false, nodes))
			return e.rows(1, elems, nodes)
		}
		e.line(toonHeader("", len(elems), false, nil))
		return e.items(1, elems)
	}
	token, err := toonPrimitive(v)
	if err != nil {
		return err
	}
	e.line(token)
	return nil
}

// fields writes the fields of an object whose content lives at the given depth (§8).
func (e *toonEncoder) fields(depth int, fields []Field) error {
	for _, f := range fields {
		if err := e.field(toonPad(depth), depth+1, f); err != nil {
			return err
		}
	}
	return nil
}

// field writes one object field. lineIndent is the prefix of the field's own line, which is either plain
// indentation or the list-item marker of §10; contentDepth is the depth of any scope the field opens.
func (e *toonEncoder) field(lineIndent string, contentDepth int, f Field) error {
	key, err := toonKey(f.Name)
	if err != nil {
		return fmt.Errorf("field %q: %w", f.Name, err)
	}

	if fields, ok := toonObject(f.Value); ok {
		// §9.5: an object of uniform objects in field position must use the keyed tabular form.
		if nodes, ok := toonKeyedFields(fields); ok {
			e.line(lineIndent + toonHeader(key, len(fields), true, nodes))
			return e.entryRows(contentDepth, fields, nodes)
		}
		e.line(lineIndent + key + ":")
		return e.fields(contentDepth, fields)
	}

	if elems, ok := toonArray(f.Value); ok {
		// §9.1: an empty array in field position is spelled out, never as a zero-length header.
		if len(elems) == 0 {
			e.line(lineIndent + key + ": []")
			return nil
		}
		if cells, ok, err := toonInlineCells(elems); ok || err != nil {
			if err != nil {
				return fmt.Errorf("field %q: %w", f.Name, err)
			}
			e.line(lineIndent + toonHeader(key, len(elems), false, nil) + " " + strings.Join(cells, toonDelimiter))
			return nil
		}
		if nodes, ok := toonTableFields(elems); ok {
			e.line(lineIndent + toonHeader(key, len(elems), false, nodes))
			return e.rows(contentDepth, elems, nodes)
		}
		e.line(lineIndent + toonHeader(key, len(elems), false, nil))
		return e.items(contentDepth, elems)
	}

	token, err := toonPrimitive(f.Value)
	if err != nil {
		return fmt.Errorf("field %q: %w", f.Name, err)
	}
	e.line(lineIndent + key + ": " + token)
	return nil
}

// items writes the list items of an array in list form (§9.4).
func (e *toonEncoder) items(depth int, elems []any) error {
	for _, el := range elems {
		if err := e.item(depth, el); err != nil {
			return err
		}
	}
	return nil
}

// item writes one list item at the given depth.
func (e *toonEncoder) item(depth int, v any) error {
	if fields, ok := toonObject(v); ok {
		// §10: an empty object list item is a bare marker; otherwise the first field rides on the hyphen
		// line and stands one level deeper, so any scope it opens has its content two levels deeper.
		if len(fields) == 0 {
			e.line(toonPad(depth) + "-")
			return nil
		}
		if err := e.field(toonPad(depth)+"- ", depth+2, fields[0]); err != nil {
			return err
		}
		for _, f := range fields[1:] {
			if err := e.field(toonPad(depth+1), depth+2, f); err != nil {
				return err
			}
		}
		return nil
	}

	if elems, ok := toonArray(v); ok {
		// §9.2, §10: a keyless header on the hyphen line is the item itself, so its own items stay one
		// level deeper. The "key: []" form of §9.1 does not apply here.
		cells, inline, err := toonInlineCells(elems)
		if err != nil {
			return err
		}
		if inline {
			line := toonPad(depth) + "- " + toonHeader("", len(elems), false, nil)
			if len(elems) > 0 {
				line += " " + strings.Join(cells, toonDelimiter)
			}
			e.line(line)
			return nil
		}
		// §9.4: tabular form is not available in list-item position, so nested arrays stay in list form.
		e.line(toonPad(depth) + "- " + toonHeader("", len(elems), false, nil))
		return e.items(depth+1, elems)
	}

	token, err := toonPrimitive(v)
	if err != nil {
		return err
	}
	e.line(toonPad(depth) + "- " + token)
	return nil
}

// rows writes the rows of a tabular array (§9.3).
func (e *toonEncoder) rows(depth int, elems []any, nodes []toonField) error {
	for _, el := range elems {
		fields, ok := toonObject(el)
		if !ok {
			return fmt.Errorf("tabular row is %T, want an object", el)
		}
		cells, err := toonRowCells(fields, nodes, nil)
		if err != nil {
			return err
		}
		e.line(toonPad(depth) + strings.Join(cells, toonDelimiter))
	}
	return nil
}

// entryRows writes the entry rows of a keyed tabular object (§9.5).
func (e *toonEncoder) entryRows(depth int, fields []Field, nodes []toonField) error {
	for _, f := range fields {
		key, err := toonKey(f.Name)
		if err != nil {
			return fmt.Errorf("entry %q: %w", f.Name, err)
		}
		value, ok := toonObject(f.Value)
		if !ok {
			return fmt.Errorf("entry %q is %T, want an object", f.Name, f.Value)
		}
		cells, err := toonRowCells(value, nodes, nil)
		if err != nil {
			return fmt.Errorf("entry %q: %w", f.Name, err)
		}
		e.line(toonPad(depth) + key + ": " + strings.Join(cells, toonDelimiter))
	}
	return nil
}

// toonRowCells collects one row's cells by walking the field list depth-first in pre-order (§9.3).
func toonRowCells(fields []Field, nodes []toonField, out []string) ([]string, error) {
	byName := make(map[string]any, len(fields))
	for _, f := range fields {
		byName[f.Name] = f.Value
	}
	for _, n := range nodes {
		value := byName[n.name]
		if n.children == nil {
			token, err := toonPrimitive(value)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", n.name, err)
			}
			out = append(out, token)
			continue
		}
		sub, ok := toonObject(value)
		if !ok {
			return nil, fmt.Errorf("field %q is %T, want an object", n.name, value)
		}
		var err error
		if out, err = toonRowCells(sub, n.children, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// toonHeader builds an array header, a tabular header, or – when keyed is true – a keyed header (§6). An
// empty key produces the keyless root form.
func toonHeader(key string, n int, keyed bool, nodes []toonField) string {
	var b strings.Builder
	b.WriteString(key)
	b.WriteByte('[')
	b.WriteString(strconv.Itoa(n))
	if keyed {
		b.WriteByte(':')
	}
	b.WriteByte(']')
	if len(nodes) > 0 {
		b.WriteString(toonFieldList(nodes))
	}
	b.WriteByte(':')
	return b.String()
}

func toonFieldList(nodes []toonField) string {
	parts := make([]string, len(nodes))
	for i, n := range nodes {
		// Detection already accepted the name as a key, so the error cannot occur here.
		name, _ := toonKey(n.name)
		if n.children != nil {
			name += toonFieldList(n.children)
		}
		parts[i] = name
	}
	return "{" + strings.Join(parts, toonDelimiter) + "}"
}

// toonInlineCells encodes an array of primitives for the inline form (§9.1). ok is false when any element
// is not a primitive, which sends the array to a tabular or list form instead.
func toonInlineCells(elems []any) (cells []string, ok bool, err error) {
	for _, el := range elems {
		if !toonIsPrimitive(el) {
			return nil, false, nil
		}
	}
	cells = make([]string, len(elems))
	for i, el := range elems {
		token, err := toonPrimitive(el)
		if err != nil {
			return nil, false, err
		}
		cells[i] = token
	}
	return cells, true, nil
}

// toonTableFields reports the header field list of a tabular array (§9.3). ok is false when tabular
// detection fails, in which case the caller uses list form (§9.4). The same predicate defines a
// nested-uniform column, so nested field groups are found by recursion.
func toonTableFields(elems []any) ([]toonField, bool) {
	if len(elems) == 0 {
		return nil, false
	}
	objects := make([][]Field, len(elems))
	for i, el := range elems {
		fields, ok := toonObject(el)
		if !ok || len(fields) == 0 {
			return nil, false
		}
		objects[i] = fields
	}

	// All objects must carry the same key set. A repeated key inside one object would make the row's cell
	// count disagree with the header, so it disqualifies the array as well.
	first := toonKeySet(objects[0])
	if first == nil {
		return nil, false
	}
	for _, o := range objects[1:] {
		keys := toonKeySet(o)
		if keys == nil || len(keys) != len(first) {
			return nil, false
		}
		for k := range keys {
			if !first[k] {
				return nil, false
			}
		}
	}

	// Field order is the first object's key encounter order.
	nodes := make([]toonField, 0, len(objects[0]))
	for _, f := range objects[0] {
		column := make([]any, len(objects))
		for i, o := range objects {
			column[i] = toonLookup(o, f.Name)
		}
		if toonAllPrimitive(column) {
			nodes = append(nodes, toonField{name: f.Name})
			continue
		}
		children, ok := toonTableFields(column)
		if !ok {
			return nil, false
		}
		nodes = append(nodes, toonField{name: f.Name, children: children})
	}
	return nodes, true
}

// toonKeyedFields reports the header field list of a keyed tabular object (§9.5). ok is false when keyed
// detection fails, in which case the object nests per §8.
func toonKeyedFields(fields []Field) ([]toonField, bool) {
	if len(fields) < 2 {
		return nil, false
	}
	values := make([]any, len(fields))
	for i, f := range fields {
		values[i] = f.Value
	}
	return toonTableFields(values)
}

// toonKeySet returns the object's keys, or nil when a key repeats.
func toonKeySet(fields []Field) map[string]bool {
	keys := make(map[string]bool, len(fields))
	for _, f := range fields {
		if keys[f.Name] {
			return nil
		}
		keys[f.Name] = true
	}
	return keys
}

func toonLookup(fields []Field, name string) any {
	for _, f := range fields {
		if f.Name == name {
			return f.Value
		}
	}
	return nil
}

func toonAllPrimitive(values []any) bool {
	for _, v := range values {
		if !toonIsPrimitive(v) {
			return false
		}
	}
	return true
}

// toonObject views a value as an object with a deterministic member order.
func toonObject(v any) ([]Field, bool) {
	switch x := v.(type) {
	case Object:
		return x.Fields, true
	case map[string]any:
		return toonSortedFields(reflect.ValueOf(x)), true
	}
	if v == nil {
		return nil, false
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String {
		return toonSortedFields(rv), true
	}
	return nil, false
}

// toonSortedFields orders a map's members by key. A Go map has no encounter order, so sorting is what
// makes the output deterministic (§2).
func toonSortedFields(rv reflect.Value) []Field {
	if rv.IsNil() || rv.Len() == 0 {
		return nil
	}
	keys := make([]string, 0, rv.Len())
	for _, k := range rv.MapKeys() {
		keys = append(keys, k.String())
	}
	sort.Strings(keys)
	fields := make([]Field, len(keys))
	for i, k := range keys {
		fields[i] = Field{Name: k, Value: rv.MapIndex(reflect.ValueOf(k).Convert(rv.Type().Key())).Interface()}
	}
	return fields
}

// toonArray views a value as an array. A Collection becomes one object per row, each carrying every column
// in the collection's column order, so a missing value stays a null cell.
func toonArray(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case Collection:
		elems := make([]any, len(x.Rows))
		for i, row := range x.Rows {
			fields := make([]Field, len(x.Columns))
			for j, col := range x.Columns {
				fields[j] = Field{Name: col, Value: row[col]}
			}
			elems[i] = Object{Fields: fields}
		}
		return elems, true
	}
	if v == nil {
		return nil, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil, true
		}
		elems := make([]any, rv.Len())
		for i := range elems {
			elems[i] = rv.Index(i).Interface()
		}
		return elems, true
	}
	return nil, false
}

// toonIsPrimitive reports whether the value encodes as a single token (§1.6).
func toonIsPrimitive(v any) bool {
	switch v.(type) {
	case nil, bool, string, json.Number,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	}
	return false
}

// toonPrimitive encodes one primitive value as a token (§2, §7).
func toonPrimitive(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(x), nil
	case string:
		return toonStringToken(x)
	case json.Number:
		return toonNumberToken(x.String())
	case int:
		return strconv.FormatInt(int64(x), 10), nil
	case int8:
		return strconv.FormatInt(int64(x), 10), nil
	case int16:
		return strconv.FormatInt(int64(x), 10), nil
	case int32:
		return strconv.FormatInt(int64(x), 10), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return toonFloatToken(float64(x), 32), nil
	case float64:
		return toonFloatToken(x, 64), nil
	}
	return "", fmt.Errorf("cannot encode %T as TOON", v)
}

// toonFloatToken renders a float in the canonical number form of §2. Values in the canonical range use
// plain decimal notation; outside it the JSON exponent form is used, with a lowercase "e" and an explicit
// sign for byte-for-byte determinism. NaN and infinities become null (§3).
func toonFloatToken(f float64, bits int) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		// Covers -0, which §2 normalizes to 0.
		return "0"
	}
	if a := math.Abs(f); a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, bits)
	}
	return strconv.FormatFloat(f, 'e', -1, bits)
}

// toonNumberToken canonicalizes a JSON number literal (§2). An integer literal is kept verbatim, which
// keeps integers larger than float64 can hold lossless; everything else goes through the float form.
func toonNumberToken(s string) (string, error) {
	if toonCanonicalInt.MatchString(s) {
		if s == "-0" {
			return "0", nil
		}
		return s, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", fmt.Errorf("number %q is outside the supported numeric domain: %w", s, err)
	}
	return toonFloatToken(f, 64), nil
}

// toonStringToken encodes a string value, quoting it when §7.2 requires it.
func toonStringToken(s string) (string, error) {
	if !utf8.ValidString(s) {
		// §3: a host string that is not a sequence of Unicode scalar values is not representable.
		return "", fmt.Errorf("string is not valid UTF-8")
	}
	if !toonNeedsQuote(s) {
		return s, nil
	}
	return toonQuote(s), nil
}

// toonKey encodes an object key, an entry key, or a header field name (§7.3).
func toonKey(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("key is not valid UTF-8")
	}
	if toonUnquotedKey.MatchString(name) {
		return name, nil
	}
	return toonQuote(name), nil
}

// toonNeedsQuote lists the conditions of §7.2. The relevant delimiter is always the comma here (§11.1).
func toonNeedsQuote(s string) bool {
	if s == "" {
		return true
	}
	switch s {
	case "true", "false", "null":
		return true
	}
	if strings.HasPrefix(s, " ") || strings.HasPrefix(s, "\t") ||
		strings.HasSuffix(s, " ") || strings.HasSuffix(s, "\t") {
		return true
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "#") {
		return true
	}
	if strings.ContainsAny(s, ":\"\\[]{}"+toonDelimiter) {
		return true
	}
	if toonNumericLike.MatchString(s) {
		return true
	}
	for _, r := range s {
		if r < 0x20 {
			return true
		}
	}
	return false
}

// toonQuote writes a quoted string with the escape set of §7.1, which is the only escape set TOON has.
func toonQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
