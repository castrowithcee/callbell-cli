package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

// maxAgentRequestBytes bounds the complete stdin input before JSON decoding. One MiB leaves ample room for
// tool arguments while preventing an untrusted agent from making the short-lived CLI allocate an unbounded
// request body.
const maxAgentRequestBytes = 1 << 20

// auditedError carries a completed mutation audit event without changing the wrapped error's public
// classification. The central runner prints the diagnostic and optional usage before it writes the event.
type auditedError struct {
	err   error
	audit []byte
}

func (e *auditedError) Error() string { return e.err.Error() }
func (e *auditedError) Unwrap() error { return e.err }

func withAudit(err error, audit []byte) error {
	if err == nil || len(bytes.TrimSpace(audit)) == 0 {
		return err
	}
	return &auditedError{err: err, audit: append([]byte(nil), audit...)}
}

func auditFrom(err error) []byte {
	var audited *auditedError
	if errors.As(err, &audited) {
		return audited.audit
	}
	return nil
}

func writeAudit(writer io.Writer, audit []byte, redactor *redact.Redactor) {
	if len(audit) == 0 {
		return
	}
	value := string(audit)
	if redactor != nil {
		value = redactor.Apply(value)
	}
	_, _ = io.WriteString(writer, value)
	if !strings.HasSuffix(value, "\n") {
		_, _ = io.WriteString(writer, "\n")
	}
}

func applicationCore(opts *Options, registry *capability.Registry, withSecrets bool) (*application.Core, error) {
	path, err := config.Path(opts.Config)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path, registry)
	if err != nil {
		return nil, classifyUserError(err)
	}
	if !withSecrets {
		return application.New(registry, cfg, nil, opts.Redactor), nil
	}
	secrets, err := opts.resolver()
	if err != nil {
		return nil, err
	}
	return application.New(registry, cfg, secrets, opts.Redactor), nil
}

// readInvokeArguments reads the schema-dependent arguments of one invocation from stdin. Empty input means
// the tool is invoked without arguments; anything else must be exactly one bounded JSON object. The input
// schema of the tool validates the content afterwards, in the application core.
func readInvokeArguments(reader io.Reader) (json.RawMessage, error) {
	input, err := io.ReadAll(io.LimitReader(reader, maxAgentRequestBytes+1))
	if err != nil {
		return nil, &application.InvalidRequestError{Message: "stdin arguments could not be read"}
	}
	if len(input) > maxAgentRequestBytes {
		return nil, &application.InvalidRequestError{
			Message: fmt.Sprintf("stdin arguments exceed %d bytes", maxAgentRequestBytes),
		}
	}
	if len(bytes.TrimSpace(input)) == 0 {
		return json.RawMessage(`{}`), nil
	}

	decoder := json.NewDecoder(bytes.NewReader(input))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, &application.InvalidRequestError{
			Message: "stdin does not contain a valid JSON arguments object",
		}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, &application.InvalidRequestError{
			Message: "stdin must contain exactly one JSON arguments object",
		}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, &application.InvalidRequestError{Message: "stdin arguments must be a JSON object"}
	}
	return json.RawMessage(trimmed), nil
}

// argumentsFromFlags turns repeated --arg name=value pairs into the JSON object the contract expects. The
// input schema decides what a value is: where it asks for a number, "limit=10" becomes one; where it asks
// for text, the value stays exactly as it was typed. That keeps a flat call free of JSON quoting without
// giving the CLI a second argument contract.
func argumentsFromFlags(schema json.RawMessage, pairs []string) (json.RawMessage, error) {
	values := map[string]any{}
	for _, pair := range pairs {
		name, raw, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return nil, &application.InvalidRequestError{
				Message: fmt.Sprintf("--arg %q must be written as name=value", pair),
			}
		}
		if _, repeated := values[name]; repeated {
			return nil, &application.InvalidRequestError{
				Message: fmt.Sprintf("--arg %s was given more than once", name),
			}
		}
		value, err := typedArgument(schema, name, raw)
		if err != nil {
			return nil, err
		}
		values[name] = value
	}
	return json.Marshal(values)
}

// typedArgument converts one flag value into the type its schema asks for. An unknown name stays a string:
// the core validates the whole object against the schema afterwards and reports it there, in the same
// words a request from stdin would get.
func typedArgument(schema json.RawMessage, name, raw string) (any, error) {
	switch propertyType(schema, name) {
	case "integer":
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			return nil, &application.InvalidRequestError{
				Message: fmt.Sprintf("--arg %s must be a whole number", name),
			}
		}
		return json.Number(raw), nil
	case "number":
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return nil, &application.InvalidRequestError{
				Message: fmt.Sprintf("--arg %s must be a number", name),
			}
		}
		return json.Number(raw), nil
	case "boolean":
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, &application.InvalidRequestError{
				Message: fmt.Sprintf("--arg %s must be true or false", name),
			}
		}
		return value, nil
	case "array", "object":
		// A structured value has no flat spelling worth inventing. It is accepted as JSON here, and the
		// message names the way that was built for it.
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return nil, &application.InvalidRequestError{
				Message: fmt.Sprintf("--arg %s expects JSON; pass the whole arguments object on stdin instead",
					name),
			}
		}
		return value, nil
	default:
		return raw, nil
	}
}

// propertyType reads the declared type of one top-level property. Anything it cannot read means the value
// stays a string, which is what an unconstrained argument is.
func propertyType(schema json.RawMessage, name string) string {
	var document struct {
		Properties map[string]struct {
			Type json.RawMessage `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(schema, &document) != nil {
		return ""
	}
	property, ok := document.Properties[name]
	if !ok {
		return ""
	}
	var single string
	if json.Unmarshal(property.Type, &single) == nil {
		return single
	}
	// A union such as ["string","null"] has no single answer, so the value stays a string.
	return ""
}

func writeEnvelope(writer io.Writer, data any) error {
	envelope := struct {
		Data any `json:"data"`
	}{Data: data}
	if err := json.NewEncoder(writer).Encode(envelope); err != nil {
		return fmt.Errorf("encode JSON envelope: %w", err)
	}
	return nil
}
