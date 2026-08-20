// Package telegram registers Telegram configuration metadata and its safe connection test. Agent
// operations deliberately remain absent until the send operation is implemented.
package telegram

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

const (
	Provider       = "telegram"
	roleBotToken   = "bot-token"
	defaultURL     = "https://api.telegram.org"
	defaultTimeout = 30 * time.Second
)

// Register adds Telegram as a configurable provider without registering an operation.
func Register(reg *capability.Registry) error {
	return reg.RegisterProvider(config.ProviderMetadata{
		ID: Provider, Name: "Telegram", DefaultBaseURL: defaultURL,
		SecretRoles: []config.SecretRole{{
			Name:        roleBotToken,
			Description: "Telegram bot token issued by BotFather",
		}},
		Target: config.TargetMetadata{
			Label: "chat ID", Description: "fixed Telegram chat ID or @channel username", Required: true,
		},
	}, TestConnection)
}

// TestConnection calls getMe, Telegram's read-only authentication check. It never sends to Target.
func TestConnection(ctx context.Context, resolved *config.Resolved, secrets *secret.Resolver,
	red *redact.Redactor) (provider.Class, error) {
	value, err := secrets.Resolve(resolved.Credential, resolved.Secrets, roleBotToken)
	if err != nil {
		return "", err
	}
	if red != nil {
		red.Add(value.Secret)
	}
	base, err := url.Parse(resolved.BaseURL)
	if err != nil {
		return "", &provider.Error{Class: provider.ClassProviderError, Op: "test", Message: "the base URL is unusable"}
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/bot" + url.PathEscape(value.Secret) + "/getMe"
	base.RawQuery, base.Fragment = "", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return "", &provider.Error{Class: provider.ClassProviderError, Op: "test", Message: "the request could not be built"}
	}
	client := &http.Client{
		Timeout:       defaultTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return classifyTransport(err), nil
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var result struct {
			OK bool `json:"ok"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result); err != nil || !result.OK {
			return provider.ClassProviderError, nil
		}
		return provider.ClassOK, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return provider.ClassAuth, nil
	case http.StatusTooManyRequests:
		return provider.ClassRateLimited, nil
	default:
		return provider.ClassProviderError, nil
	}
}

func classifyTransport(err error) provider.Class {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return provider.ClassUnreachable
	}
	var (
		certErr    *tls.CertificateVerificationError
		hostErr    x509.HostnameError
		authErr    x509.UnknownAuthorityError
		recordErr  tls.RecordHeaderError
		invalidErr x509.CertificateInvalidError
	)
	if errors.As(err, &certErr) || errors.As(err, &hostErr) || errors.As(err, &authErr) ||
		errors.As(err, &recordErr) || errors.As(err, &invalidErr) {
		return provider.ClassTLS
	}
	return provider.ClassUnreachable
}
