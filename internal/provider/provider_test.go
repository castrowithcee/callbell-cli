package provider_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/provider"
)

// The canaries stand for everything a transport failure carries and no diagnostic may publish: the full
// request URL with its credential, the target host, and the raw operating system message.
const (
	urlCanary     = "https://bot7799:token-canary-4f21@wiki.internal.example/api/pages?target=-1001"
	hostCanary    = "wiki.internal.example"
	messageCanary = "no such host: raw-os-canary-9134"
)

// dial wraps err the way net/http does: the operation, the socket call, and the request URL each add a
// layer, so the classifier has to walk the typed chain rather than look at the outermost error.
func dial(op string, err error) error {
	return &url.Error{
		Op: "Post", URL: urlCanary,
		Err: &net.OpError{Op: op, Net: "tcp", Addr: nil, Err: err},
	}
}

func TestTransportClassifiesTypedCausesWithoutLeakingDetail(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantClass provider.Class
		wantCause provider.Cause
	}{
		{
			name:      "a name that does not resolve",
			err:       dial("dial", &net.DNSError{Err: messageCanary, Name: hostCanary, IsNotFound: true}),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseDNS,
		},
		{
			name:      "a refused connection",
			err:       dial("dial", os.NewSyscallError("connect", syscall.ECONNREFUSED)),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseConnectionRefused,
		},
		{
			// Some platforms hand the raw errno to net.OpError instead of wrapping it in os.SyscallError.
			name:      "a refused connection without the syscall layer",
			err:       dial("dial", syscall.ECONNREFUSED),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseConnectionRefused,
		},
		{
			name:      "a sandbox that refuses the socket",
			err:       dial("dial", os.NewSyscallError("socket", syscall.EACCES)),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseNetworkPolicy,
		},
		{
			name:      "a network without a route",
			err:       dial("dial", os.NewSyscallError("connect", syscall.ENETUNREACH)),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseNetworkPolicy,
		},
		{
			name:      "a host without a route",
			err:       dial("dial", os.NewSyscallError("connect", syscall.EHOSTUNREACH)),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseNetworkPolicy,
		},
		{
			// The proxy is the answer even though the failure behind it would classify on its own.
			name:      "a proxy that cannot be reached",
			err:       dial("proxyconnect", os.NewSyscallError("connect", syscall.ECONNREFUSED)),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseProxy,
		},
		{
			name:      "a proxy whose name does not resolve",
			err:       dial("proxyconnect", &net.DNSError{Err: messageCanary, Name: hostCanary}),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseProxy,
		},
		{
			name:      "a connection the peer reset",
			err:       dial("read", os.NewSyscallError("read", syscall.ECONNRESET)),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseConnectionReset,
		},
		{
			name:      "a connection that broke while writing",
			err:       dial("write", os.NewSyscallError("write", syscall.EPIPE)),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseConnectionReset,
		},
		{
			name:      "an aborted connection",
			err:       dial("read", os.NewSyscallError("read", syscall.ECONNABORTED)),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseConnectionReset,
		},
		{
			name:      "a failure no typed error explains",
			err:       dial("dial", errors.New(messageCanary)),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseUnknown,
		},
		{
			name:      "a bare error",
			err:       errors.New(messageCanary),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseUnknown,
		},
		{
			name:      "an exhausted request deadline",
			err:       fmt.Errorf("send: %w", context.DeadlineExceeded),
			wantClass: provider.ClassTimeout,
		},
		{
			name:      "a resolver that timed out",
			err:       dial("dial", &net.DNSError{Err: messageCanary, Name: hostCanary, IsTimeout: true}),
			wantClass: provider.ClassTimeout,
		},
		{
			name:      "a socket that timed out",
			err:       dial("read", os.NewSyscallError("read", syscall.ETIMEDOUT)),
			wantClass: provider.ClassTimeout,
		},
		{
			name:      "an untrusted certificate",
			err:       dial("dial", &tls.CertificateVerificationError{Err: errors.New(messageCanary)}),
			wantClass: provider.ClassTLS,
		},
		{
			name:      "a certificate for another name",
			err:       dial("dial", x509.HostnameError{Host: hostCanary}),
			wantClass: provider.ClassTLS,
		},
		{
			name:      "a cancelled request",
			err:       fmt.Errorf("send: %w", context.Canceled),
			wantClass: provider.ClassUnreachable, wantCause: provider.CauseUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := provider.Transport("send message", "Telegram", tt.err)
			if got.Class != tt.wantClass || got.Cause != tt.wantCause {
				t.Fatalf("Transport() = class %q cause %q, want class %q cause %q",
					got.Class, got.Cause, tt.wantClass, tt.wantCause)
			}
			for _, canary := range []string{urlCanary, hostCanary, messageCanary, "token-canary-4f21", "-1001"} {
				if strings.Contains(got.Error(), canary) {
					t.Errorf("diagnostic %q carries the canary %q", got.Error(), canary)
				}
			}
		})
	}
}

// A timeout and a TLS failure stay unambiguous: they publish no cause, because a request that ran into a
// deadline may well have arrived. A caller must not read a proven "nothing was sent" into them.
func TestTimeoutAndTLSCarryNoTransportCause(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, &tls.CertificateVerificationError{}} {
		got := provider.Transport("send message", "Telegram", err)
		if got.Cause != "" {
			t.Errorf("Transport(%T) = cause %q, want no cause", err, got.Cause)
		}
	}
}

// The published diagnostic is the message plus the cause; an error without a cause keeps the old form.
func TestErrorPublishesTheCause(t *testing.T) {
	with := &provider.Error{
		Class: provider.ClassUnreachable, Op: "send message", Message: "Telegram could not be reached",
		Cause: provider.CauseDNS,
	}
	if got, want := with.Error(), "send message: Telegram could not be reached (cause: dns)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	without := &provider.Error{Class: provider.ClassAuth, Op: "send message", Message: "Telegram rejected the token"}
	if got, want := without.Error(), "send message: Telegram rejected the token"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
