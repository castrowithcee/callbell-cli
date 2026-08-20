package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

// maxAgentRequestBytes bounds the complete stdin request before JSON decoding. One MiB leaves ample room
// for operation arguments while preventing an untrusted agent from making the short-lived CLI allocate an
// unbounded request body.
const maxAgentRequestBytes = 1 << 20

func newSearchCommand(opts *Options, registry *capability.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "search",
		Short: "Search operation contracts from one JSON request on stdin",
		Args:  noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			var request application.SearchRequest
			if err := decodeAgentRequest(c.InOrStdin(), &request); err != nil {
				return &UsageError{err}
			}
			core, err := applicationCore(opts, registry, false)
			if err != nil {
				return err
			}
			response, err := core.Search(request)
			if err != nil {
				return classifyUserError(err)
			}
			return writeEnvelope(c.OutOrStdout(), response)
		},
	}
}

func newDescribeCommand(opts *Options, registry *capability.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "describe",
		Short: "Describe one operation contract from a JSON request on stdin",
		Args:  noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			var request application.DescribeRequest
			if err := decodeAgentRequest(c.InOrStdin(), &request); err != nil {
				return &UsageError{err}
			}
			core, err := applicationCore(opts, registry, false)
			if err != nil {
				return err
			}
			response, err := core.Describe(request)
			if err != nil {
				return classifyUserError(err)
			}
			return writeEnvelope(c.OutOrStdout(), response)
		},
	}
}

func newInvokeCommand(opts *Options, registry *capability.Registry) *cobra.Command {
	return &cobra.Command{
		Use:   "invoke",
		Short: "Invoke one operation from a JSON request on stdin",
		Args:  noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			var request application.InvokeRequest
			if err := decodeAgentRequest(c.InOrStdin(), &request); err != nil {
				return &UsageError{err}
			}
			core, err := applicationCore(opts, registry, true)
			if err != nil {
				return err
			}
			var audit bytes.Buffer
			core.SetAudit(&audit)
			response, err := core.Invoke(c.Context(), request)
			if err != nil {
				return withAudit(classifyUserError(err), audit.Bytes())
			}
			if err := writeEnvelope(c.OutOrStdout(), response); err != nil {
				return withAudit(err, audit.Bytes())
			}
			writeAudit(c.ErrOrStderr(), audit.Bytes(), opts.Redactor)
			return nil
		},
	}
}

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

func decodeAgentRequest(reader io.Reader, target any) error {
	input, err := io.ReadAll(io.LimitReader(reader, maxAgentRequestBytes+1))
	if err != nil {
		return &application.InvalidRequestError{Message: "stdin JSON request could not be read"}
	}
	if len(input) > maxAgentRequestBytes {
		return &application.InvalidRequestError{
			Message: fmt.Sprintf("stdin JSON request exceeds %d bytes", maxAgentRequestBytes),
		}
	}

	decoder := json.NewDecoder(bytes.NewReader(input))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return &application.InvalidRequestError{Message: "stdin must contain one JSON request"}
		}
		return &application.InvalidRequestError{Message: "stdin does not contain a valid JSON request"}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return &application.InvalidRequestError{Message: "stdin must contain exactly one JSON request"}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return &application.InvalidRequestError{Message: "stdin request must be a JSON object"}
	}
	request := json.NewDecoder(bytes.NewReader(trimmed))
	request.DisallowUnknownFields()
	if err := request.Decode(target); err != nil {
		return &application.InvalidRequestError{Message: "stdin does not contain a valid JSON request"}
	}
	return nil
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
