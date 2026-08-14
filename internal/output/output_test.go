package output

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The fixtures exercise every character the compact grammar has to escape, both value types, missing
// values, and empty values.
var (
	fixtureCollection = Collection{
		Columns: []string{"name", "note", "count", "active", "ratio", "empty"},
		Rows: []Row{
			{
				"name":   "alpha|beta",
				"note":   `back\slash`,
				"count":  int64(3),
				"active": true,
				"ratio":  1.5,
				"empty":  "",
			},
			{
				// "empty" is absent on purpose: a missing value keeps its column.
				"name":   "line1\nline2",
				"note":   "carriage\rreturn",
				"count":  int64(0),
				"active": false,
				"ratio":  0.0,
			},
			{
				"name":   "key=value",
				"note":   nil,
				"count":  int64(-7),
				"active": true,
				"ratio":  2.25,
				"empty":  "x",
			},
		},
	}

	fixtureObject = Object{Fields: []Field{
		{Name: "name", Value: "alpha|beta"},
		{Name: "note", Value: `back\slash`},
		{Name: "key=with=equals", Value: "a=b"},
		{Name: "body", Value: "line1\nline2"},
		{Name: "count", Value: int64(42)},
		{Name: "active", Value: false},
		{Name: "ratio", Value: 0.125},
		{Name: "nothing", Value: nil},
		{Name: "blank", Value: ""},
	}}
)

func TestEncodeGolden(t *testing.T) {
	tests := []struct {
		name   string
		result Result
		format Format
	}{
		{"collection", fixtureCollection, FormatTable},
		{"collection", fixtureCollection, FormatJSON},
		{"collection", fixtureCollection, FormatCompact},
		{"object", fixtureObject, FormatTable},
		{"object", fixtureObject, FormatJSON},
		{"object", fixtureObject, FormatCompact},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.format), func(t *testing.T) {
			var got bytes.Buffer
			if err := Encode(&got, tt.format, tt.result); err != nil {
				t.Fatalf("Encode() = %v", err)
			}

			golden := filepath.Join("testdata", tt.name+"."+string(tt.format)+".golden")
			if *update {
				if err := os.WriteFile(golden, got.Bytes(), 0o600); err != nil {
					t.Fatalf("write golden: %v", err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Errorf("output differs from %s:\ngot:\n%s\nwant:\n%s", golden, got.String(), want)
			}
		})
	}
}

// Encoding the same fixture repeatedly must produce byte-identical output.
func TestEncodeIsByteStable(t *testing.T) {
	for _, format := range []Format{FormatTable, FormatJSON, FormatCompact} {
		t.Run(string(format), func(t *testing.T) {
			var first bytes.Buffer
			if err := Encode(&first, format, fixtureCollection); err != nil {
				t.Fatalf("Encode() = %v", err)
			}
			for i := 0; i < 20; i++ {
				var got bytes.Buffer
				if err := Encode(&got, format, fixtureCollection); err != nil {
					t.Fatalf("Encode() = %v", err)
				}
				if !bytes.Equal(got.Bytes(), first.Bytes()) {
					t.Fatalf("run %d differs:\n%s", i+2, got.String())
				}
			}
		})
	}
}

// The compact grammar must stay unambiguous: decoding the encoded form reproduces the exact values,
// separators, backslashes, and real line breaks included.
func TestCompactCollectionRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, FormatCompact, fixtureCollection); err != nil {
		t.Fatalf("Encode() = %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != len(fixtureCollection.Rows)+1 {
		t.Fatalf("got %d lines, want %d; a value leaked a real line break",
			len(lines), len(fixtureCollection.Rows)+1)
	}

	if got := splitCompact(lines[0], false); !reflect.DeepEqual(got, fixtureCollection.Columns) {
		t.Errorf("header = %v, want %v", got, fixtureCollection.Columns)
	}
	for i, line := range lines[1:] {
		got := splitCompact(line, false)
		want := make([]string, len(fixtureCollection.Columns))
		for j, col := range fixtureCollection.Columns {
			want[j] = text(fixtureCollection.Rows[i][col])
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("row %d = %q, want %q", i, got, want)
		}
	}
}

func TestCompactObjectRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, FormatCompact, fixtureObject); err != nil {
		t.Fatalf("Encode() = %v", err)
	}

	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		pair := splitCompact(line, true)
		if len(pair) != 2 {
			t.Fatalf("line %q split into %d parts, want key and value", line, len(pair))
		}
		want, ok := fieldValue(fixtureObject, pair[0])
		if !ok {
			t.Fatalf("decoded unknown key %q", pair[0])
		}
		if pair[1] != want {
			t.Errorf("key %q = %q, want %q", pair[0], pair[1], want)
		}
	}
}

// A backslash, an escaped separator, and a real line break must decode to three different things.
func TestCompactDistinguishesEscapes(t *testing.T) {
	c := Collection{
		Columns: []string{"v"},
		Rows: []Row{
			{"v": `\n`},    // literal backslash followed by the letter n
			{"v": "\n"},    // a real line break
			{"v": "|"},     // the separator itself
			{"v": `\|`},    // a literal backslash followed by the separator
			{"v": `\\`},    // two literal backslashes
			{"v": "a\\|b"}, // backslash and separator next to each other
		},
	}

	var buf bytes.Buffer
	if err := Encode(&buf, FormatCompact, c); err != nil {
		t.Fatalf("Encode() = %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != len(c.Rows)+1 {
		t.Fatalf("got %d lines, want %d", len(lines), len(c.Rows)+1)
	}
	seen := map[string]bool{}
	for i, line := range lines[1:] {
		if seen[line] {
			t.Errorf("row %d encodes to %q, which another row already produced", i, line)
		}
		seen[line] = true
		if got := splitCompact(line, false); len(got) != 1 || got[0] != c.Rows[i]["v"] {
			t.Errorf("row %d decoded to %q, want %q", i, got, c.Rows[i]["v"])
		}
	}
}

func TestJSONIsLosslessAndTyped(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, FormatJSON, fixtureCollection); err != nil {
		t.Fatalf("Encode() = %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("the output is not valid JSON: %v", err)
	}
	if len(rows) != len(fixtureCollection.Rows) {
		t.Fatalf("got %d rows, want %d", len(rows), len(fixtureCollection.Rows))
	}

	// Types survive: numbers stay numbers, booleans stay booleans, a missing value becomes null.
	if _, ok := rows[0]["count"].(float64); !ok {
		t.Errorf("count = %T, want a JSON number", rows[0]["count"])
	}
	if _, ok := rows[0]["active"].(bool); !ok {
		t.Errorf("active = %T, want a JSON boolean", rows[0]["active"])
	}
	if v, ok := rows[1]["empty"]; !ok || v != nil {
		t.Errorf("missing value = %v (present: %v), want an explicit null", v, ok)
	}
	if rows[1]["name"] != "line1\nline2" {
		t.Errorf("name = %q, want the real line break preserved", rows[1]["name"])
	}
	// Field order follows the declared columns rather than Go's map order.
	if !strings.HasPrefix(buf.String(), `[{"name":"alpha|beta","note":"back\\slash","count":3,`) {
		t.Errorf("field order changed: %s", buf.String())
	}
}

// JSON keeps every object field, including null and empty ones; table and compact drop them.
func TestObjectEmptyHandling(t *testing.T) {
	var jsonOut, compactOut, tableOut bytes.Buffer
	for _, c := range []struct {
		format Format
		buf    *bytes.Buffer
	}{{FormatJSON, &jsonOut}, {FormatCompact, &compactOut}, {FormatTable, &tableOut}} {
		if err := Encode(c.buf, c.format, fixtureObject); err != nil {
			t.Fatalf("Encode(%s) = %v", c.format, err)
		}
	}

	if !strings.Contains(jsonOut.String(), `"nothing":null`) || !strings.Contains(jsonOut.String(), `"blank":""`) {
		t.Errorf("JSON dropped an empty field: %s", jsonOut.String())
	}
	for name, out := range map[string]string{"compact": compactOut.String(), "table": tableOut.String()} {
		if strings.Contains(out, "nothing") || strings.Contains(out, "blank") {
			t.Errorf("%s kept an empty field:\n%s", name, out)
		}
	}
}

// A collection keeps every column, even when a row has no value for it.
func TestCollectionKeepsEmptyColumns(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, FormatCompact, fixtureCollection); err != nil {
		t.Fatalf("Encode() = %v", err)
	}

	for i, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if got := len(splitCompact(line, false)); got != len(fixtureCollection.Columns) {
			t.Errorf("line %d has %d fields, want %d", i, got, len(fixtureCollection.Columns))
		}
	}
}

// For the tabular fixture the compact form is smaller than JSON.
func TestCompactIsSmallerThanJSON(t *testing.T) {
	var compactOut, jsonOut bytes.Buffer
	if err := Encode(&compactOut, FormatCompact, fixtureCollection); err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	if err := Encode(&jsonOut, FormatJSON, fixtureCollection); err != nil {
		t.Fatalf("Encode() = %v", err)
	}

	if compactOut.Len() >= jsonOut.Len() {
		t.Errorf("compact is %d bytes, JSON is %d bytes", compactOut.Len(), jsonOut.Len())
	}
}

func TestParseFormat(t *testing.T) {
	for _, name := range []string{"table", "json", "compact"} {
		if _, err := ParseFormat(name); err != nil {
			t.Errorf("ParseFormat(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "yaml", "jsonl", "raw", "TABLE"} {
		if _, err := ParseFormat(name); err == nil {
			t.Errorf("ParseFormat(%q) = nil, want an error", name)
		}
	}
}

// The documented error codes must stay the ones the build can emit.
func TestDocumentedCodesMatchTheCode(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "output.md"))
	if err != nil {
		t.Fatalf("read the documentation: %v", err)
	}

	documented := map[string]bool{}
	for _, line := range strings.Split(string(doc), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		if name, _, ok := strings.Cut(strings.TrimPrefix(line, "| `"), "`"); ok {
			documented[name] = true
		}
	}

	for _, code := range AllCodes() {
		if !documented[string(code)] {
			t.Errorf("the code %q is not documented in docs/output.md", code)
		}
		delete(documented, string(code))
	}
	for name := range documented {
		// The table of formats uses the same markup, so only unknown code-like rows matter.
		if strings.Contains(name, "-") || name == "usage" || name == "runtime" {
			t.Errorf("docs/output.md documents %q, which the build cannot emit", name)
		}
	}
}

func TestProject(t *testing.T) {
	t.Run("collection columns are restricted and reordered", func(t *testing.T) {
		got, err := Project(fixtureCollection, []string{"count", "name"})
		if err != nil {
			t.Fatalf("Project() = %v", err)
		}
		if want := []string{"count", "name"}; !reflect.DeepEqual(got.(Collection).Columns, want) {
			t.Errorf("columns = %v, want %v", got.(Collection).Columns, want)
		}
	})

	t.Run("object fields are restricted and reordered", func(t *testing.T) {
		got, err := Project(fixtureObject, []string{"count", "name"})
		if err != nil {
			t.Fatalf("Project() = %v", err)
		}
		fields := got.(Object).Fields
		if len(fields) != 2 || fields[0].Name != "count" || fields[1].Name != "name" {
			t.Errorf("fields = %+v", fields)
		}
	})

	t.Run("an empty projection keeps everything", func(t *testing.T) {
		got, err := Project(fixtureCollection, nil)
		if err != nil {
			t.Fatalf("Project() = %v", err)
		}
		if !reflect.DeepEqual(got, fixtureCollection) {
			t.Error("the result changed")
		}
	})

	t.Run("unknown collection field", func(t *testing.T) {
		_, err := Project(fixtureCollection, []string{"name", "absent"})

		var perr *ProjectionError
		if !asProjection(err, &perr) {
			t.Fatalf("Project() = %v, want a *ProjectionError", err)
		}
		if perr.Field != "absent" {
			t.Errorf("field = %q, want absent", perr.Field)
		}
	})

	t.Run("a repeated field is rejected", func(t *testing.T) {
		_, err := Project(fixtureCollection, []string{"name", "count", "name"})

		var perr *ProjectionError
		if !asProjection(err, &perr) {
			t.Fatalf("Project() = %v, want a *ProjectionError", err)
		}
		if !perr.Duplicate || perr.Field != "name" {
			t.Errorf("error = %+v, want a duplicate report for name", perr)
		}
	})

	t.Run("unknown object field", func(t *testing.T) {
		_, err := Project(fixtureObject, []string{"absent"})

		var perr *ProjectionError
		if !asProjection(err, &perr) {
			t.Fatalf("Project() = %v, want a *ProjectionError", err)
		}
	})

	// Projection must not alter the source result.
	t.Run("the source is untouched", func(t *testing.T) {
		before := append([]string(nil), fixtureCollection.Columns...)
		if _, err := Project(fixtureCollection, []string{"name"}); err != nil {
			t.Fatalf("Project() = %v", err)
		}
		if !reflect.DeepEqual(fixtureCollection.Columns, before) {
			t.Errorf("columns = %v, want %v", fixtureCollection.Columns, before)
		}
	})
}

func TestLimit(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want int
	}{
		{"below the row count", 2, 2},
		{"exactly the row count", 3, 3},
		{"above the row count", 10, 3},
		{"zero means no limit", 0, 3},
		{"negative means no limit", -1, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Limit(fixtureCollection, tt.n).(Collection)

			if len(got.Rows) != tt.want {
				t.Errorf("rows = %d, want %d", len(got.Rows), tt.want)
			}
		})
	}

	t.Run("objects pass through", func(t *testing.T) {
		if got := Limit(fixtureObject, 1); !reflect.DeepEqual(got, fixtureObject) {
			t.Error("the object changed")
		}
	})
}

func TestEncodeRejectsUnknownFormat(t *testing.T) {
	if err := Encode(new(bytes.Buffer), Format("yaml"), fixtureObject); err == nil {
		t.Error("Encode() = nil, want an error")
	}
}

// splitCompact reverses the compact escaping and splits a line on unescaped separators.
func splitCompact(line string, object bool) []string {
	var (
		parts []string
		cur   strings.Builder
		esc   bool
	)
	for _, r := range line {
		switch {
		case esc:
			switch r {
			case 'n':
				cur.WriteByte('\n')
			case 'r':
				cur.WriteByte('\r')
			default:
				cur.WriteRune(r)
			}
			esc = false
		case r == '\\':
			esc = true
		case r == '|' && !object, r == '=' && object && len(parts) == 0:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(parts, cur.String())
}

func fieldValue(o Object, name string) (string, bool) {
	for _, f := range o.Fields {
		if f.Name == name {
			return text(f.Value), true
		}
	}
	return "", false
}

func asProjection(err error, target **ProjectionError) bool {
	p, ok := err.(*ProjectionError)
	if ok {
		*target = p
	}
	return ok
}
