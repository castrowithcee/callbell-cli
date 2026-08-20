// Package provider holds what every provider implementation shares: the stable classes a connection test
// can report, and the normalised error every provider raises instead of leaking transport details.
package provider

import "fmt"

// Class is the stable outcome of a connection test. Callers branch on it instead of on message text.
type Class string

// Stable provider outcome classes used by connection tests and operations.
const (
	ClassOK              Class = "ok"
	ClassUnreachable     Class = "unreachable"
	ClassTLS             Class = "tls"
	ClassAuth            Class = "auth"
	ClassPermission      Class = "permission"
	ClassTimeout         Class = "timeout"
	ClassRateLimited     Class = "rate-limited"
	ClassInvalidResponse Class = "invalid-provider-response"
	ClassProviderError   Class = "provider-error"
)

// Error is a provider failure normalised to a class. Its message never carries credentials, headers, or
// raw transport detail.
type Error struct {
	Class   Class
	Op      string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Op, e.Message)
}
