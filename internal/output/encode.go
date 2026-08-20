package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Compact escaping. The replacer performs a single pass, so an escaped backslash is never escaped again.
var (
	compactCollection = strings.NewReplacer(`\`, `\\`, "|", `\|`, "\n", `\n`, "\r", `\r`)
	compactObject     = strings.NewReplacer(`\`, `\\`, "|", `\|`, "=", `\=`, "\n", `\n`, "\r", `\r`)

	// The human table keeps one record per line, so control characters in a value stay visible instead of
	// breaking the row structure.
	tableCell = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`)
)

// Encode writes a result to w in the given format. Only requested payload data reaches w.
func Encode(w io.Writer, format Format, result Result) error {
	if format == FormatTOON {
		return encodeTOONDocument(w, result)
	}
	switch r := result.(type) {
	case Collection:
		switch format {
		case FormatTable:
			return encodeCollectionTable(w, r)
		case FormatJSON:
			return encodeCollectionJSON(w, r)
		case FormatCompact:
			return encodeCollectionCompact(w, r)
		}
	case Object:
		switch format {
		case FormatTable:
			return encodeObjectTable(w, r)
		case FormatJSON:
			return encodeObjectJSON(w, r)
		case FormatCompact:
			return encodeObjectCompact(w, r)
		}
	}
	return fmt.Errorf("cannot encode %T as %q", result, format)
}

// encodeTOONDocument writes one TOON document and the terminating newline the encoder deliberately omits.
func encodeTOONDocument(w io.Writer, value any) error {
	document, err := MarshalTOON(value)
	if err != nil {
		return err
	}
	_, err = w.Write(append(document, '\n'))
	return err
}

// text renders a scalar for the human and compact formats. A missing value is the empty string.
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

func encodeCollectionTable(w io.Writer, c Collection) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(upper(c.Columns), "\t")); err != nil {
		return err
	}
	for _, row := range c.Rows {
		cells := make([]string, len(c.Columns))
		for i, col := range c.Columns {
			cells[i] = tableCell.Replace(text(row[col]))
		}
		if _, err := fmt.Fprintln(tw, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// encodeObjectTable prints one key-value pair per line. Null and empty values are omitted.
func encodeObjectTable(w io.Writer, o Object) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, f := range o.Fields {
		value := text(f.Value)
		if value == "" {
			continue
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", f.Name, tableCell.Replace(value)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// encodeCollectionCompact writes a header line and one line per row, fields separated by "|".
func encodeCollectionCompact(w io.Writer, c Collection) error {
	cells := make([]string, len(c.Columns))
	for i, col := range c.Columns {
		cells[i] = compactCollection.Replace(col)
	}
	if _, err := fmt.Fprintln(w, strings.Join(cells, "|")); err != nil {
		return err
	}
	for _, row := range c.Rows {
		for i, col := range c.Columns {
			cells[i] = compactCollection.Replace(text(row[col]))
		}
		if _, err := fmt.Fprintln(w, strings.Join(cells, "|")); err != nil {
			return err
		}
	}
	return nil
}

// encodeObjectCompact writes key=value per line. Null and empty values are omitted.
func encodeObjectCompact(w io.Writer, o Object) error {
	for _, f := range o.Fields {
		value := text(f.Value)
		if value == "" {
			continue
		}
		line := compactObject.Replace(f.Name) + "=" + compactObject.Replace(value)
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// encodeCollectionJSON writes an array of objects. Every row carries every column, so a missing value
// becomes null rather than a missing key.
func encodeCollectionJSON(w io.Writer, c Collection) error {
	var b strings.Builder
	b.WriteByte('[')
	for i, row := range c.Rows {
		if i > 0 {
			b.WriteByte(',')
		}
		fields := make([]Field, len(c.Columns))
		for j, col := range c.Columns {
			fields[j] = Field{Name: col, Value: row[col]}
		}
		if err := writeJSONObject(&b, fields); err != nil {
			return err
		}
	}
	b.WriteString("]\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func encodeObjectJSON(w io.Writer, o Object) error {
	var b strings.Builder
	if err := writeJSONObject(&b, o.Fields); err != nil {
		return err
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

// writeJSONObject keeps the declared field order, which encoding/json cannot do for maps, and keeps the
// value types intact.
func writeJSONObject(b *strings.Builder, fields []Field) error {
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		name, err := json.Marshal(f.Name)
		if err != nil {
			return err
		}
		value, err := json.Marshal(f.Value)
		if err != nil {
			return fmt.Errorf("field %q: %w", f.Name, err)
		}
		b.Write(name)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return nil
}

func upper(columns []string) []string {
	out := make([]string, len(columns))
	for i, c := range columns {
		out[i] = strings.ToUpper(c)
	}
	return out
}
