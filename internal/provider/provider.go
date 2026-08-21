// Package provider holds what every provider implementation shares: the stable classes a connection test
// can report, the stable causes a transport failure can be attributed to, and the normalised error every
// provider raises instead of leaking transport details.
package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

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

// Cause is the stable, safe diagnosis of a transport failure that happened before a status code existed.
// It never carries a URL, a target, a header, or a raw operating system message, so it is published with
// the unreachable class instead of replacing it. An error the typed chain cannot attribute stays
// CauseUnknown, which keeps the compatible unreachable fallback honest.
type Cause string

// The transport causes this build can prove from a typed error chain.
const (
	CauseDNS               Cause = "dns"
	CauseConnectionRefused Cause = "connection-refused"
	CauseNetworkPolicy     Cause = "network-policy"
	CauseProxy             Cause = "proxy"
	CauseConnectionReset   Cause = "connection-reset"
	CauseUnknown           Cause = "unknown"
)

// Error is a provider failure normalised to a class. Its message never carries credentials, headers, or
// raw transport detail. Cause is set only for the transport failures Transport classifies.
type Error struct {
	Class   Class
	Op      string
	Message string
	Cause   Cause
}

func (e *Error) Error() string {
	if e.Cause == "" {
		return fmt.Sprintf("%s: %s", e.Op, e.Message)
	}
	return fmt.Sprintf("%s: %s (cause: %s)", e.Op, e.Message, e.Cause)
}

// Transport normalises a failure that happened before a status code existed. Every provider shares it, so
// the same failure reaches the CLI, the agent output, and MCP as the same class and the same cause.
// Subject names the reachable side in the message, for example "Telegram" or "the server". The original
// error text is never copied, so a URL carrying credentials can never reach the message.
//
// A timeout and a TLS failure stay their own unambiguous class and carry no cause: a request that ran into
// a deadline may well have arrived, which is exactly what a mutation must not treat as a retryable
// failure. Everything else stays unreachable and adds the cause the typed chain can prove.
func Transport(op, subject string, err error) *Error {
	var (
		netErr     net.Error
		certErr    *tls.CertificateVerificationError
		hostErr    x509.HostnameError
		authErr    x509.UnknownAuthorityError
		recordErr  tls.RecordHeaderError
		invalidErr x509.CertificateInvalidError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return &Error{Class: ClassTimeout, Op: op, Message: subject + " did not answer in time"}
	case errors.As(err, &certErr), errors.As(err, &hostErr), errors.As(err, &authErr),
		errors.As(err, &recordErr), errors.As(err, &invalidErr):
		return &Error{Class: ClassTLS, Op: op, Message: "the TLS connection could not be established"}
	}
	return &Error{
		Class: ClassUnreachable, Op: op, Message: subject + " could not be reached", Cause: TransportCause(err),
	}
}

// TransportCause attributes a transport failure to a stable cause by walking the typed error chain. It
// never inspects message text, so a translated or reworded operating system message cannot change the
// answer. What no typed error proves stays CauseUnknown.
func TransportCause(err error) Cause {
	var (
		opErr  *net.OpError
		dnsErr *net.DNSError
	)
	switch {
	// A proxy is named before what went wrong behind it: the fix is the proxy configuration, whether the
	// proxy host did not resolve or refused the connection. net/http wraps every failure to reach a proxy
	// in this typed operation.
	case errors.As(err, &opErr) && opErr.Op == "proxyconnect":
		return CauseProxy
	case errors.As(err, &dnsErr):
		return CauseDNS
	case errors.Is(err, syscall.ECONNREFUSED):
		return CauseConnectionRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNABORTED),
		errors.Is(err, syscall.EPIPE):
		return CauseConnectionReset
	// A sandbox or firewall either refuses the socket outright or drops the route. Both are a local policy
	// the operator can change, and neither says anything about the provider.
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.ENETUNREACH),
		errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETDOWN):
		return CauseNetworkPolicy
	}
	return CauseUnknown
}
