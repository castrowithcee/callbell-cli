package application

import (
	"fmt"
	"strings"
)

// InvalidRequestError reports malformed JSON or arguments that do not satisfy the input schema.
type InvalidRequestError struct{ Message string }

func (e *InvalidRequestError) Error() string { return "invalid request: " + e.Message }

// UnknownOperationError reports an operation ID or requested version absent from the registry.
type UnknownOperationError struct {
	Operation string
	Version   int
}

func (e *UnknownOperationError) Error() string {
	if e.Version > 0 {
		return fmt.Sprintf("unknown operation %q at version %d", e.Operation, e.Version)
	}
	return fmt.Sprintf("unknown operation %q", e.Operation)
}

// ConnectionAmbiguousError reports that local configuration has several valid routes and no unique default.
type ConnectionAmbiguousError struct {
	Operation   string
	Connections []string
}

func (e *ConnectionAmbiguousError) Error() string {
	return fmt.Sprintf("operation %q has multiple matching connections: %s",
		e.Operation, strings.Join(e.Connections, ", "))
}

// ConnectionSelectionError reports a registered operation for which no configured connection can be
// selected. It names no CLI flag because agent requests carry their connection in JSON.
type ConnectionSelectionError struct{ Operation string }

func (e *ConnectionSelectionError) Error() string {
	return fmt.Sprintf("no configured connection can invoke operation %q", e.Operation)
}

// ConfirmationRequiredError reports a mutating request without its request-bound confirmation.
type ConfirmationRequiredError struct{ Operation string }

func (e *ConfirmationRequiredError) Error() string {
	return fmt.Sprintf("operation %q requires confirmation in this invoke request", e.Operation)
}

// PolicyDeniedError deliberately omits policy internals from the public diagnostic.
type PolicyDeniedError struct{ Operation string }

func (e *PolicyDeniedError) Error() string {
	return fmt.Sprintf("policy denied operation %q", e.Operation)
}

// InvalidProviderResponseError reports output that does not satisfy the registered contract.
type InvalidProviderResponseError struct{ Operation string }

func (e *InvalidProviderResponseError) Error() string {
	return fmt.Sprintf("operation %q returned an invalid provider response", e.Operation)
}
