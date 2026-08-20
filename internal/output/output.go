// Package output turns typed core results into the three output formats, applies projection and limits,
// and defines the provider-independent error codes. Encoders never reach into providers: they only render
// what the core produced.
package output

import (
	"fmt"
	"strings"
)

// Format is a rendering of a result.
type Format string

// The three MVP formats.
const (
	FormatTable   Format = "table"
	FormatJSON    Format = "json"
	FormatCompact Format = "compact"
)

// DefaultLimit caps a collection when the caller asks for no specific limit. Zero means no limit.
const DefaultLimit = 50

// ParseFormat validates a format name.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatTable, FormatJSON, FormatCompact:
		return Format(s), nil
	}
	return "", fmt.Errorf("unknown output format %q, want %s, %s, or %s", s, FormatTable, FormatJSON, FormatCompact)
}

// Field is one named value of an object. Values are scalars: string, bool, int64, float64, or nil.
type Field struct {
	Name  string
	Value any
}

// Object is a single record with a stable field order.
type Object struct {
	Fields []Field
}

// Row holds the values of one collection record, addressed by column name. A missing key renders as an
// empty field and as JSON null.
type Row map[string]any

// Collection is an ordered list of records sharing one column order.
type Collection struct {
	Columns []string
	Rows    []Row
}

// Result is what a command hands to the encoders.
type Result interface{ isResult() }

func (Object) isResult()     {}
func (Collection) isResult() {}

// ProjectionError reports a field in --fields that cannot be used. It is a usage problem.
type ProjectionError struct {
	Field     string
	Available []string
	Duplicate bool
}

func (e *ProjectionError) Error() string {
	if e.Duplicate {
		return fmt.Sprintf("field %q is requested more than once", e.Field)
	}
	return fmt.Sprintf("unknown field %q, available fields are %s", e.Field, strings.Join(e.Available, ", "))
}

// Project restricts a result to the named fields, in the order given. An empty list keeps everything.
// Validation is central so the same field names can later be pushed down to a provider.
func Project(result Result, fields []string) (Result, error) {
	if len(fields) == 0 {
		return result, nil
	}

	// A repeated name would produce a duplicate key in the JSON output.
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		if seen[f] {
			return nil, &ProjectionError{Field: f, Duplicate: true}
		}
		seen[f] = true
	}

	switch r := result.(type) {
	case Collection:
		available := r.Columns
		columns := make([]string, 0, len(fields))
		for _, f := range fields {
			if !contains(available, f) {
				return nil, &ProjectionError{Field: f, Available: available}
			}
			columns = append(columns, f)
		}
		return Collection{Columns: columns, Rows: r.Rows}, nil

	case Object:
		available := make([]string, len(r.Fields))
		byName := make(map[string]Field, len(r.Fields))
		for i, f := range r.Fields {
			available[i] = f.Name
			byName[f.Name] = f
		}
		out := make([]Field, 0, len(fields))
		for _, name := range fields {
			f, ok := byName[name]
			if !ok {
				return nil, &ProjectionError{Field: name, Available: available}
			}
			out = append(out, f)
		}
		return Object{Fields: out}, nil
	}
	return result, nil
}

// Limit truncates a collection to at most n rows. A limit of zero or less keeps everything. Objects pass
// through unchanged.
func Limit(result Result, n int) Result {
	c, ok := result.(Collection)
	if !ok || n <= 0 || len(c.Rows) <= n {
		return result
	}
	return Collection{Columns: c.Columns, Rows: c.Rows[:n]}
}

// Code is a provider-independent error class. Agents branch on it instead of parsing messages.
type Code string

// The error codes this build can emit. Providers add their transport classes on top.
const (
	CodeUsage                 Code = "usage"
	CodeInvalidRequest        Code = "invalid-request"
	CodeConfigMissing         Code = "config-missing"
	CodeConfigInvalid         Code = "config-invalid"
	CodeConnectionSelection   Code = "connection-selection"
	CodeUnknownConnection     Code = "unknown-connection"
	CodeConnectionAmbiguous   Code = "connection-ambiguous"
	CodeUnknownOperation      Code = "unknown-operation"
	CodeUnsupportedCapability Code = "unsupported-capability"
	CodeMissingSecret         Code = "missing-secret"
	CodeConfirmationRequired  Code = "confirmation-required"
	CodePolicyDenied          Code = "policy-denied"
	CodeUnreachable           Code = "unreachable"
	CodeTLS                   Code = "tls"
	CodeAuth                  Code = "auth"
	CodeRateLimited           Code = "rate-limited"
	CodeInvalidProviderResult Code = "invalid-provider-response"
	CodeProviderError         Code = "provider-error"
	CodeRuntime               Code = "runtime"
)

// AllCodes lists every error code this build can emit, in the order the documentation shows them.
func AllCodes() []Code {
	return []Code{
		CodeUsage, CodeInvalidRequest, CodeConfigMissing, CodeConfigInvalid, CodeConnectionSelection,
		CodeUnknownConnection, CodeConnectionAmbiguous, CodeUnknownOperation, CodeUnsupportedCapability,
		CodeMissingSecret, CodeConfirmationRequired, CodePolicyDenied, CodeUnreachable, CodeTLS, CodeAuth,
		CodeRateLimited, CodeInvalidProviderResult, CodeProviderError, CodeRuntime,
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
