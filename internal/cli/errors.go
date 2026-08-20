package cli

import (
	"errors"

	"github.com/castrowithcee/callbell-cli/internal/application"
	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// codeFor maps an error to its provider-independent code. Agents branch on the code instead of parsing
// the message.
func codeFor(err error) output.Code {
	var (
		notFound      *config.NotFoundError
		invalid       *config.InvalidError
		selection     *config.SelectionError
		unknownConn   *capability.UnknownConnectionError
		unsupported   *capability.UnsupportedError
		projection    *output.ProjectionError
		usage         *UsageError
		invalidReq    *application.InvalidRequestError
		unknownOp     *application.UnknownOperationError
		ambiguous     *application.ConnectionAmbiguousError
		appSelection  *application.ConnectionSelectionError
		confirmation  *application.ConfirmationRequiredError
		denied        *application.PolicyDeniedError
		invalidResult *application.InvalidProviderResponseError

		missingSecret *secret.MissingSecretError
		permission    *secret.PermissionError
		providerErr   *provider.Error
	)
	switch {
	case errors.As(err, &notFound):
		return output.CodeConfigMissing
	case errors.As(err, &invalid):
		return output.CodeConfigInvalid
	case errors.As(err, &selection):
		// A name that does not exist is the same problem however the command reached it.
		if selection.Name != "" {
			return output.CodeUnknownConnection
		}
		return output.CodeConnectionSelection
	case errors.As(err, &unknownConn):
		return output.CodeUnknownConnection
	case errors.As(err, &ambiguous):
		return output.CodeConnectionAmbiguous
	case errors.As(err, &appSelection):
		return output.CodeConnectionSelection
	case errors.As(err, &unknownOp):
		return output.CodeUnknownOperation
	case errors.As(err, &unsupported):
		return output.CodeUnsupportedCapability
	case errors.As(err, &invalidReq):
		return output.CodeInvalidRequest
	case errors.As(err, &confirmation):
		return output.CodeConfirmationRequired
	case errors.As(err, &denied):
		return output.CodePolicyDenied
	case errors.As(err, &invalidResult):
		return output.CodeInvalidProviderResult
	case errors.As(err, &permission):
		// A credential file others can read is one state with one fix, whichever operation ran into it.
		// It is named before the missing secret it causes, so reading, writing, and deleting all report
		// the file rather than three different things.
		return output.CodeConfigInvalid
	case errors.As(err, &missingSecret):
		return output.CodeMissingSecret
	case errors.As(err, &providerErr):
		return providerCode(providerErr.Class)
	case errors.As(err, &projection), errors.As(err, &usage):
		return output.CodeUsage
	}
	return output.CodeRuntime
}

func providerCode(class provider.Class) output.Code {
	switch class {
	case provider.ClassUnreachable:
		return output.CodeUnreachable
	case provider.ClassTLS:
		return output.CodeTLS
	case provider.ClassAuth:
		return output.CodeAuth
	case provider.ClassPermission:
		return output.CodePermission
	case provider.ClassTimeout:
		return output.CodeTimeout
	case provider.ClassRateLimited:
		return output.CodeRateLimited
	case provider.ClassInvalidResponse:
		return output.CodeInvalidProviderResult
	default:
		return output.CodeProviderError
	}
}

// classifyUserError marks everything the user can fix in their configuration or invocation as a usage
// problem: a missing, malformed, or inconsistent configuration, an unselectable connection, a capability
// that is not offered, and an unknown projection field. Any other failure, for example an unreadable file,
// stays a runtime error.
func classifyUserError(err error) error {
	if err == nil {
		return nil
	}
	var (
		notFound     *config.NotFoundError
		invalid      *config.InvalidError
		selection    *config.SelectionError
		unknownConn  *capability.UnknownConnectionError
		unsupported  *capability.UnsupportedError
		projection   *output.ProjectionError
		invalidReq   *application.InvalidRequestError
		unknownOp    *application.UnknownOperationError
		ambiguous    *application.ConnectionAmbiguousError
		appSelection *application.ConnectionSelectionError
		confirmation *application.ConfirmationRequiredError
		denied       *application.PolicyDeniedError

		missingSecret *secret.MissingSecretError
		permission    *secret.PermissionError
	)
	switch {
	case errors.As(err, &notFound), errors.As(err, &invalid), errors.As(err, &selection),
		errors.As(err, &unknownConn), errors.As(err, &unsupported), errors.As(err, &projection),
		errors.As(err, &missingSecret), errors.As(err, &permission), errors.As(err, &invalidReq),
		errors.As(err, &unknownOp), errors.As(err, &ambiguous), errors.As(err, &confirmation),
		errors.As(err, &appSelection), errors.As(err, &denied):
		return &UsageError{err}
	}
	return err
}
