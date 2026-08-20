package output

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// The expectations in this file come from the TOON specification 4.1 (reference state v4.1.1). Section
// references name the rule under test; the documents in TestMarshalTOONSpecExamples are the examples of
// Appendix A verbatim.

// obj builds an object from alternating name and value arguments, keeping the declared order.
func obj(pairs ...any) Object {
	if len(pairs)%2 != 0 {
		panic("obj wants name/value pairs")
	}
	fields := make([]Field, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		fields = append(fields, Field{Name: pairs[i].(string), Value: pairs[i+1]})
	}
	return Object{Fields: fields}
}

// doc trims the leading newline that keeps the expected documents readable in the source.
func doc(s string) string {
	return strings.TrimPrefix(s, "\n")
}

func marshalTOON(t *testing.T, v any) string {
	t.Helper()
	out, err := MarshalTOON(v)
	if err != nil {
		t.Fatalf("MarshalTOON() = %v", err)
	}
	return string(out)
}

func TestMarshalTOONSpecExamples(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "object",
			value: obj("id", 123, "name", "Ada", "active", true),
			want: doc(`
id: 123
name: Ada
active: true`),
		},
		{
			name:  "nested object",
			value: obj("user", obj("id", 123, "name", "Ada")),
			want: doc(`
user:
  id: 123
  name: Ada`),
		},
		{
			name:  "primitive array",
			value: obj("tags", []any{"admin", "ops", "dev"}),
			want:  "tags[3]: admin,ops,dev",
		},
		{
			name:  "array of primitive arrays",
			value: obj("pairs", []any{[]any{1, 2}, []any{3, 4}}),
			want: doc(`
pairs[2]:
  - [2]: 1,2
  - [2]: 3,4`),
		},
		{
			name: "tabular array",
			value: obj("items", []any{
				obj("sku", "A1", "qty", 2, "price", 9.99),
				obj("sku", "B2", "qty", 1, "price", 14.5),
			}),
			want: doc(`
items[2]{sku,qty,price}:
  A1,2,9.99
  B2,1,14.5`),
		},
		{
			name: "tabular array with nested field group",
			value: obj("orders", []any{
				obj("id", 1, "customer", obj("name", "Ada", "country", "DK"), "total", 99),
				obj("id", 2, "customer", obj("name", "Bob", "country", "UK"), "total", 149),
			}),
			want: doc(`
orders[2]{id,customer{name,country},total}:
  1,Ada,DK,99
  2,Bob,UK,149`),
		},
		{
			name: "keyed tabular object",
			value: obj("users", obj(
				"alice", obj("age", 30, "city", "Berlin"),
				"bob", obj("age", 25, "city", "Oslo"),
			)),
			want: doc(`
users[2:]{age,city}:
  alice: 30,Berlin
  bob: 25,Oslo`),
		},
		{
			name: "keyed tabular object at the root",
			value: obj(
				"alice", obj("age", 30, "city", "Berlin"),
				"bob", obj("age", 25, "city", "Oslo"),
			),
			want: doc(`
[2:]{age,city}:
  alice: 30,Berlin
  bob: 25,Oslo`),
		},
		{
			name:  "mixed array",
			value: obj("items", []any{1, obj("a", 1), "text"}),
			want: doc(`
items[3]:
  - 1
  - a: 1
  - text`),
		},
		{
			name: "objects as list items",
			value: obj("items", []any{
				obj("id", 1, "name", "First"),
				obj("id", 2, "name", "Second", "extra", true),
			}),
			want: doc(`
items[2]:
  - id: 1
    name: First
  - id: 2
    name: Second
    extra: true`),
		},
		{
			name: "nested tabular inside a list item",
			value: obj("items", []any{
				obj("users", []any{obj("id", 1, "name", "Ada"), obj("id", 2, "name", "Bob")}, "status", "active"),
			}),
			want: doc(`
items[1]:
  - users[2]{id,name}:
      1,Ada
      2,Bob
    status: active`),
		},
		{
			// The array is not tabular (the elements differ in their keys), so the first field of the
			// first item keeps the keyed tabular form on the hyphen line with its entries at depth +2.
			name: "keyed tabular object as the first field of a list item",
			value: obj("items", []any{
				obj("byUser", obj("alice", obj("a", 1), "bob", obj("a", 2)), "status", "ok"),
				obj("other", 1),
			}),
			want: doc(`
items[2]:
  - byUser[2:]{a}:
      alice: 1
      bob: 2
    status: ok
  - other: 1`),
		},
		{
			name: "quoted colons in rows",
			value: obj("links", []any{
				obj("id", 1, "url", "http://a:b"),
				obj("id", 2, "url", "https://example.com?q=a:b"),
			}),
			want: doc(`
links[2]{id,url}:
  1,"http://a:b"
  2,"https://example.com?q=a:b"`),
		},
		{
			name:  "empty string value",
			value: obj("name", ""),
			want:  `name: ""`,
		},
		{
			name:  "empty array value",
			value: obj("tags", []any{}),
			want:  "tags: []",
		},
		{
			name:  "strings that look like other types",
			value: obj("version", "123", "enabled", "true"),
			want: doc(`
version: "123"
enabled: "true"`),
		},
		{
			name: "deep nesting",
			value: obj("root", obj("level1", obj("level2", obj("level3", obj(
				"items", []any{obj("id", 1, "val", "a"), obj("id", 2, "val", "b")},
			))))),
			want: doc(`
root:
  level1:
    level2:
      level3:
        items[2]{id,val}:
          1,a
          2,b`),
		},
		{
			name:  "unicode stays unescaped",
			value: obj("message", "Hello 世界 👋", "tags", []any{"🎉", "🎊", "🎈"}),
			want: doc(`
message: Hello 世界 👋
tags[3]: 🎉,🎊,🎈`),
		},
		{
			name:  "large and fractional numbers",
			value: obj("bignum", int64(9007199254740992), "decimal", 0.3333333333333333),
			want: doc(`
bignum: 9007199254740992
decimal: 0.3333333333333333`),
		},
		{
			name:  "quoted key with a primitive array",
			value: obj("my-key", []any{1, 2, 3}),
			want:  `"my-key"[3]: 1,2,3`,
		},
		{
			name: "quoted key with a tabular array",
			value: obj("x-items", []any{
				obj("id", 1, "name", "Ada"),
				obj("id", 2, "name", "Bob"),
			}),
			want: doc(`
"x-items"[2]{id,name}:
  1,Ada
  2,Bob`),
		},
		{
			name: "quoted key with a list-form array",
			value: obj("x-items", []any{
				obj("id", 1),
				obj("id", 2, "label", "archived"),
			}),
			want: doc(`
"x-items"[2]:
  - id: 1
  - id: 2
    label: archived`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := marshalTOON(t, tt.value); got != tt.want {
				t.Errorf("MarshalTOON() =\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

// Root forms per §5, plus the empty values of §8 and §9.1.
func TestMarshalTOONRootAndEmptyValues(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"empty root object", Object{}, ""},
		{"empty root map", map[string]any{}, ""},
		{"empty root array", []any{}, "[]"},
		{"empty root collection", Collection{Columns: []string{"a"}}, "[]"},
		{"root primitive string", "hello", "hello"},
		{"root primitive number", 42, "42"},
		{"root primitive bool", true, "true"},
		{"root primitive null", nil, "null"},
		{"root primitive needing quotes", "-x", `"-x"`},
		{"root primitive array", []any{1, 2, 3}, "[3]: 1,2,3"},
		{"nested empty object", obj("a", Object{}), "a:"},
		{"nested empty array", obj("a", []any{}), "a: []"},
		{"null field", obj("a", nil), "a: null"},
		{"empty string field", obj("a", ""), `a: ""`},
		{"empty object list item", obj("a", []any{Object{}, obj("b", 1)}), doc(`
a[2]:
  -
  - b: 1`)},
		{"empty inner array list item", obj("a", []any{[]any{}, []any{1}}), doc(`
a[2]:
  - [0]:
  - [1]: 1`)},
		{"single entry object does not go keyed", obj("a", obj("only", obj("x", 1))), doc(`
a:
  only:
    x: 1`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := marshalTOON(t, tt.value); got != tt.want {
				t.Errorf("MarshalTOON() =\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

// Quoting of string values per §7.2 and the escape set of §7.1.
func TestMarshalTOONStringQuoting(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"", `""`},
		{" lead", `" lead"`},
		{"trail ", `"trail "`},
		{"\tlead", `"\tlead"`},
		{"true", `"true"`},
		{"false", `"false"`},
		{"null", `"null"`},
		{"42", `"42"`},
		{"-3.14", `"-3.14"`},
		{"05", `"05"`},
		{"+1", `"+1"`},
		{"1e-6", `"1e-6"`},
		{"1E9", `"1E9"`},
		{"a:b", `"a:b"`},
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		{"a[b", `"a[b"`},
		{"a]b", `"a]b"`},
		{"a{b", `"a{b"`},
		{"a}b", `"a}b"`},
		{"a,b", `"a,b"`},
		{"a\nb", `"a\nb"`},
		{"a\rb", `"a\rb"`},
		{"a\tb", `"a\tb"`},
		{"a\x01b", `"a\u0001b"`},
		{"-", `"-"`},
		{"-x", `"-x"`},
		{"#", `"#"`},
		{"#x", `"#x"`},
		// Nothing in the list above applies, so these stay unquoted.
		{"hello world", "hello world"},
		{"a|b", "a|b"},
		{"a-b", "a-b"},
		{"Hello 世界 👋", "Hello 世界 👋"},
		{"True", "True"},
		{"1.", "1."},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got := marshalTOON(t, obj("k", tt.value))
			want := "k: " + tt.want
			if got != want {
				t.Errorf("MarshalTOON() = %q, want %q", got, want)
			}
		})
	}
}

// A value carrying the active delimiter must be quoted wherever it is a cell (§11.1).
func TestMarshalTOONQuotesDelimiterInEveryCellPosition(t *testing.T) {
	value := obj(
		"inline", []any{"a,b", "c"},
		"rows", []any{obj("x", "a,b"), obj("x", "c")},
		"keyed", obj("one", obj("x", "a,b"), "two", obj("x", "c")),
	)
	want := doc(`
inline[2]: "a,b",c
rows[2]{x}:
  "a,b"
  c
keyed[2:]{x}:
  one: "a,b"
  two: c`)
	if got := marshalTOON(t, value); got != want {
		t.Errorf("MarshalTOON() =\n%s\nwant:\n%s", got, want)
	}
}

// Key encoding per §7.3: keys outside the unquoted pattern are quoted in every position.
func TestMarshalTOONKeyQuoting(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"name", "name"},
		{"_name", "_name"},
		{"a.b.c", "a.b.c"},
		{"A1", "A1"},
		{"my-key", `"my-key"`},
		{"1abc", `"1abc"`},
		{"", `""`},
		{"with space", `"with space"`},
		{"Ünicode", `"Ünicode"`},
		{`quo"te`, `"quo\"te"`},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := marshalTOON(t, obj(tt.key, 1))
			if want := tt.want + ": 1"; got != want {
				t.Errorf("field key: MarshalTOON() = %q, want %q", got, want)
			}

			got = marshalTOON(t, obj("rows", []any{obj(tt.key, 1), obj(tt.key, 2)}))
			want := "rows[2]{" + tt.want + "}:\n  1\n  2"
			if got != want {
				t.Errorf("header field name: MarshalTOON() = %q, want %q", got, want)
			}

			got = marshalTOON(t, obj("keyed", obj(tt.key, obj("x", 1), tt.key+"2", obj("x", 2))))
			if !strings.Contains(got, "\n  "+tt.want+": 1") {
				t.Errorf("entry key: MarshalTOON() = %q, want an entry key %q", got, tt.want)
			}
		})
	}
}

// Canonical number form per §2 and the NaN/infinity normalization of §3.
func TestMarshalTOONCanonicalNumbers(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"int", 42, "42"},
		{"negative int", -7, "-7"},
		{"int64 max", int64(math.MaxInt64), "9223372036854775807"},
		{"uint64 max", uint64(math.MaxUint64), "18446744073709551615"},
		{"float integral", 1.0, "1"},
		{"float trailing zeros", 1.5000, "1.5"},
		{"float exponent in range", 1e6, "1000000"},
		{"float small in range", 1e-6, "0.000001"},
		{"float upper bound", 1e20, "100000000000000000000"},
		{"float negative zero", math.Copysign(0, -1), "0"},
		{"float zero", 0.0, "0"},
		{"float32", float32(1.5), "1.5"},
		{"float repeating", 0.3333333333333333, "0.3333333333333333"},
		{"float below canonical range", 1e-7, "1e-07"},
		{"float above canonical range", 1e21, "1e+21"},
		{"float negative above range", -1e21, "-1e+21"},
		{"nan", math.NaN(), "null"},
		{"positive infinity", math.Inf(1), "null"},
		{"negative infinity", math.Inf(-1), "null"},
		{"json number integer", json.Number("42"), "42"},
		{"json number negative zero", json.Number("-0"), "0"},
		{"json number trailing zeros", json.Number("1.5000"), "1.5"},
		{"json number exponent", json.Number("1e6"), "1000000"},
		{"json number beyond float64", json.Number("123456789012345678901234567890"), "123456789012345678901234567890"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := marshalTOON(t, obj("n", tt.value))
			if want := "n: " + tt.want; got != want {
				t.Errorf("MarshalTOON() = %q, want %q", got, want)
			}
		})
	}
}

// Tabular detection per §9.3: only arrays whose columns are all uniform-primitive or nested-uniform take
// the tabular form; everything else falls back to list form (§9.4).
func TestMarshalTOONTabularDetection(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "uniform objects use the tabular form",
			value: obj("a", []any{obj("x", 1, "y", 2), obj("x", 3, "y", 4)}),
			want:  "a[2]{x,y}:\n  1,2\n  3,4",
		},
		{
			name:  "key order follows the first element",
			value: obj("a", []any{obj("x", 1, "y", 2), obj("y", 4, "x", 3)}),
			want:  "a[2]{x,y}:\n  1,2\n  3,4",
		},
		{
			name:  "null is a primitive column",
			value: obj("a", []any{obj("x", nil), obj("x", 1)}),
			want:  "a[2]{x}:\n  null\n  1",
		},
		{
			name:  "differing key sets fall back to list form",
			value: obj("a", []any{obj("x", 1), obj("y", 1)}),
			want:  "a[2]:\n  - x: 1\n  - y: 1",
		},
		{
			name:  "an empty object disqualifies the tabular form",
			value: obj("a", []any{obj("x", 1), Object{}}),
			want:  "a[2]:\n  - x: 1\n  -",
		},
		{
			name:  "an array column disqualifies the tabular form",
			value: obj("a", []any{obj("x", []any{1}), obj("x", []any{2})}),
			want:  "a[2]:\n  - x[1]: 1\n  - x[1]: 2",
		},
		{
			name:  "a column mixing null and objects disqualifies the tabular form",
			value: obj("a", []any{obj("x", obj("y", 1)), obj("x", nil)}),
			want:  "a[2]:\n  - x:\n      y: 1\n  - x: null",
		},
		{
			name:  "nested groups nest further",
			value: obj("a", []any{obj("x", obj("y", obj("z", 1))), obj("x", obj("y", obj("z", 2)))}),
			want:  "a[2]{x{y{z}}}:\n  1\n  2",
		},
		{
			// §9.5: as the value of a nested-uniform column an object never takes the keyed tabular form.
			name:  "a keyed-eligible object inside a tabular column becomes a nested field group",
			value: obj("a", []any{obj("byUser", obj("alice", obj("x", 1), "bob", obj("x", 2)))}),
			want:  "a[1]{byUser{alice{x},bob{x}}}:\n  1,2",
		},
		{
			name:  "a nested array inside a list item stays in list form",
			value: obj("a", []any{[]any{obj("x", 1), obj("x", 2)}}),
			want:  "a[1]:\n  - [2]:\n    - x: 1\n    - x: 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := marshalTOON(t, tt.value); got != tt.want {
				t.Errorf("MarshalTOON() =\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

// A Collection is a uniform array of objects, so it renders as a table; a missing row value stays null.
func TestMarshalTOONCollection(t *testing.T) {
	c := Collection{
		Columns: []string{"name", "count", "note"},
		Rows: []Row{
			{"name": "alpha", "count": int64(1), "note": "ok"},
			{"name": "beta", "count": int64(2)},
		},
	}
	want := doc(`
[2]{name,count,note}:
  alpha,1,ok
  beta,2,null`)
	if got := marshalTOON(t, c); got != want {
		t.Errorf("MarshalTOON() =\n%s\nwant:\n%s", got, want)
	}
}

// §2: declared field order is preserved, while map keys are sorted so that the output is deterministic.
func TestMarshalTOONFieldOrder(t *testing.T) {
	declared := obj("zulu", 1, "alpha", 2, "mike", 3)
	if got, want := marshalTOON(t, declared), "zulu: 1\nalpha: 2\nmike: 3"; got != want {
		t.Errorf("declared order: MarshalTOON() = %q, want %q", got, want)
	}

	fromMap := map[string]any{"zulu": 1, "alpha": 2, "mike": 3}
	if got, want := marshalTOON(t, fromMap), "alpha: 2\nmike: 3\nzulu: 1"; got != want {
		t.Errorf("map order: MarshalTOON() = %q, want %q", got, want)
	}
}

// The same input must always produce the same bytes, including for map-backed values.
func TestMarshalTOONIsByteStable(t *testing.T) {
	value := obj(
		"meta", map[string]any{"zulu": 1, "alpha": "a,b", "mike": []any{1, 2}},
		"rows", []any{obj("id", 1, "tags", obj("a", 1, "b", 2)), obj("id", 2, "tags", obj("a", 3, "b", 4))},
		"mixed", []any{1, "x", obj("k", nil), []any{}, map[string]any{"q": true}},
		"keyed", obj("one", obj("x", 1.5), "two", obj("x", 2.5)),
	)

	first, err := MarshalTOON(value)
	if err != nil {
		t.Fatalf("MarshalTOON() = %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := MarshalTOON(value)
		if err != nil {
			t.Fatalf("MarshalTOON() = %v", err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("run %d differs:\n%s\nfirst:\n%s", i+2, got, first)
		}
	}
}

// §12: LF line endings, no trailing spaces, and no trailing newline.
func TestMarshalTOONWhitespaceInvariants(t *testing.T) {
	value := obj(
		"a", obj("b", []any{obj("x", 1, "y", obj("z", 2))}),
		"c", []any{obj("d", 1), 2, []any{3}},
		"e", obj("one", obj("x", 1), "two", obj("x", 2)),
	)
	out := marshalTOON(t, value)

	if strings.Contains(out, "\r") {
		t.Errorf("output contains CR:\n%q", out)
	}
	if strings.HasSuffix(out, "\n") {
		t.Errorf("output ends with a newline:\n%q", out)
	}
	for i, line := range strings.Split(out, "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d has trailing whitespace: %q", i+1, line)
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent%len(toonIndent) != 0 {
			t.Errorf("line %d is not indented in multiples of %d: %q", i+1, len(toonIndent), line)
		}
		if strings.HasPrefix(strings.TrimLeft(line, " "), "\t") {
			t.Errorf("line %d indents with a tab: %q", i+1, line)
		}
	}
}

func TestEncodeTOONWritesTheSameBytes(t *testing.T) {
	value := obj("a", 1, "b", []any{obj("x", 1), obj("x", 2)})
	want, err := MarshalTOON(value)
	if err != nil {
		t.Fatalf("MarshalTOON() = %v", err)
	}
	var got bytes.Buffer
	if err := EncodeTOON(&got, value); err != nil {
		t.Fatalf("EncodeTOON() = %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("EncodeTOON() = %q, want %q", got.String(), want)
	}
}

func TestMarshalTOONErrors(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{"unsupported type", obj("a", struct{ X int }{1})},
		{"unsupported type in a row", []any{obj("a", struct{}{}), obj("a", struct{}{})}},
		{"invalid utf-8 in a value", obj("a", "\xff")},
		{"invalid utf-8 in a key", obj("\xff", 1)},
		{"lone surrogate in a value", obj("a", "\xed\xa0\x80")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if out, err := MarshalTOON(tt.value); err == nil {
				t.Errorf("MarshalTOON() = %q, want an error", out)
			}
		})
	}
}
