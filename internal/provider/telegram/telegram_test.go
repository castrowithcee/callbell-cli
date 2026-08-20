package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

const testToken = "123456:canary_telegram_bot-token"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func telegramClient(t *testing.T, target string, transport http.RoundTripper) (*Client, *redact.Redactor) {
	t.Helper()
	red := &redact.Redactor{}
	resolver := secret.NewWith(func(name string) string {
		if name == "TEST_TELEGRAM_BOT_TOKEN" {
			return testToken
		}
		return ""
	}, nil, nil, red)
	httpClient := newHTTPClient()
	httpClient.Transport = transport
	client, err := openWithHTTP(&config.Resolved{
		Name: "alerts", Provider: Provider, BaseURL: "https://api.telegram.test", Target: target,
		Credential: "notifier", Secrets: config.Credential{Type: config.CredentialTypeEnv,
			Values: map[string]string{roleBotToken: "TEST_TELEGRAM_BOT_TOKEN"}},
	}, resolver, red, httpClient)
	if err != nil {
		t.Fatalf("openWithHTTP() = %v", err)
	}
	return client, red
}

func TestRegisterContainsMetadataAndSendDescriptor(t *testing.T) {
	reg := capability.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	metadata, ok := reg.ProviderMetadata(Provider)
	if !ok || metadata.DefaultBaseURL != defaultURL || !metadata.Target.Required ||
		len(metadata.SecretRoles) != 1 || metadata.SecretRoles[0].Name != roleBotToken {
		t.Fatalf("metadata = %+v, %v", metadata, ok)
	}
	operations := reg.Provider(Provider)
	if len(operations) != 1 {
		t.Fatalf("operations = %v, want one", operations)
	}
	descriptor := operations[0]
	if descriptor.ID != "telegram.messages.send" || !descriptor.RequiresExplicitConnection ||
		descriptor.Risk.Effect != capability.EffectCreate ||
		descriptor.Risk.Idempotency != capability.IdempotencyNonIdempotent ||
		descriptor.Risk.Confirmation != capability.ConfirmationRequired || !descriptor.Risk.OpenWorld ||
		descriptor.Risk.DataSensitivity != dataSensitivity {
		t.Fatalf("descriptor = %+v", descriptor)
	}
}

func TestSendMessageUsesOnePOSTWithOnlyFixedTargetAndPlainText(t *testing.T) {
	const (
		target = "-1001234567890"
		text   = "Build finished: <b>plain</b>"
	)
	calls := 0
	client, red := telegramClient(t, target, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || request.URL.Scheme != "https" ||
			request.URL.Host != "api.telegram.test" ||
			request.URL.Path != "/bot"+testToken+"/sendMessage" {
			t.Errorf("request = %s %s", request.Method, request.URL.Redacted())
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request = %v", err)
		}
		if len(body) != 2 || body["chat_id"] != target || body["text"] != text {
			t.Errorf("body = %#v", body)
		}
		if request.Header.Get("Authorization") != "" {
			t.Error("bot token was duplicated into an Authorization header")
		}
		return response(http.StatusOK,
			`{"ok":true,"result":{"message_id":91,"date":1787220000,"text":"provider copy"}}`), nil
	}))

	got, err := client.SendMessage(context.Background(), text)
	if err != nil {
		t.Fatalf("SendMessage() = %v", err)
	}
	if calls != 1 {
		t.Fatalf("requests = %d, want exactly 1", calls)
	}
	if len(got) != 2 || got["message_id"] != int64(91) || got["date"] != int64(1787220000) {
		t.Errorf("result = %#v", got)
	}
	if strings.Contains(red.Apply("request /bot"+testToken+"/sendMessage"), testToken) {
		t.Fatal("redactor did not remove the Telegram token canary")
	}
}

func TestConnectionTestUsesOnlyGetMe(t *testing.T) {
	calls := 0
	client, _ := telegramClient(t, "@alerts_channel", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.Path != "/bot"+testToken+"/getMe" {
			t.Errorf("request = %s %s", request.Method, request.URL.Redacted())
		}
		if request.Body != nil {
			t.Error("getMe request has a body")
		}
		return response(http.StatusOK, `{"ok":true,"result":{"id":1,"is_bot":true}}`), nil
	}))
	if got := client.testConnection(context.Background()); got != provider.ClassOK {
		t.Fatalf("testConnection() = %q, want ok", got)
	}
	if calls != 1 {
		t.Fatalf("requests = %d, want 1", calls)
	}
}

func TestSendMessageNormalizesProviderAndTransportFailuresWithoutRetry(t *testing.T) {
	timeout := &timeoutError{}
	tests := []struct {
		name   string
		status int
		err    error
		want   provider.Class
	}{
		{name: "authentication", status: http.StatusUnauthorized, want: provider.ClassAuth},
		{name: "permission", status: http.StatusForbidden, want: provider.ClassPermission},
		{name: "rate limit", status: http.StatusTooManyRequests, want: provider.ClassRateLimited},
		{name: "provider", status: http.StatusBadRequest, want: provider.ClassProviderError},
		{name: "timeout", err: timeout, want: provider.ClassTimeout},
		{name: "unreachable", err: errors.New("network ended after write"), want: provider.ClassUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client, _ := telegramClient(t, "-1001", roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if tt.err != nil {
					return nil, tt.err
				}
				return response(tt.status, `{"ok":false,"description":"private provider body"}`), nil
			}))
			_, err := client.SendMessage(context.Background(), "one message")
			var providerErr *provider.Error
			if !errors.As(err, &providerErr) || providerErr.Class != tt.want {
				t.Fatalf("SendMessage() = %T %v, want class %q", err, err, tt.want)
			}
			if strings.Contains(err.Error(), "private provider body") {
				t.Fatalf("error leaks provider body: %v", err)
			}
			if calls != 1 {
				t.Fatalf("requests = %d, want exactly 1", calls)
			}
		})
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "private timeout detail" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

func TestSendMessageRejectsInvalidOrOversizedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{not-json`},
		{name: "not ok", body: `{"ok":false}`},
		{name: "missing message id", body: `{"ok":true,"result":{"date":1787220000}}`},
		{name: "missing date", body: `{"ok":true,"result":{"message_id":1}}`},
		{name: "oversized", body: strings.Repeat("x", maxResponseBytes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := telegramClient(t, "-1001", roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, tt.body), nil
			}))
			_, err := client.SendMessage(context.Background(), "message")
			var providerErr *provider.Error
			if !errors.As(err, &providerErr) || providerErr.Class != provider.ClassInvalidResponse {
				t.Fatalf("SendMessage() = %T %v, want invalid response", err, err)
			}
		})
	}
}

func TestSendMessageValidatesTextBeforeIO(t *testing.T) {
	calls := 0
	client, _ := telegramClient(t, "-1001", roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(http.StatusOK, `{}`), nil
	}))
	for _, text := range []string{"", strings.Repeat("x", maxMessageLength+1)} {
		if _, err := client.SendMessage(context.Background(), text); err == nil {
			t.Fatalf("SendMessage(%d characters) = nil, want error", len(text))
		}
	}
	if calls != 0 {
		t.Fatalf("requests = %d, want 0", calls)
	}
	if _, err := client.SendMessage(context.Background(), strings.Repeat("🙂", maxMessageLength)); err == nil {
		// The transport response is intentionally invalid; reaching it proves the rune-count boundary.
		t.Fatal("SendMessage(at rune limit) unexpectedly succeeded with invalid provider response")
	}
	if calls != 1 {
		t.Fatalf("requests at rune limit = %d, want 1", calls)
	}
}

func TestConnectionsKeepBotsAndTargetsSeparate(t *testing.T) {
	type observed struct{ path, target string }
	var seen []observed
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Target string `json:"chat_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, observed{path: request.URL.Path, target: body.Target})
		return response(http.StatusOK, `{"ok":true,"result":{"message_id":1,"date":1787220000}}`), nil
	})
	open := func(name, target, token string) *Client {
		red := &redact.Redactor{}
		resolver := secret.NewWith(func(string) string { return token }, nil, nil, red)
		httpClient := newHTTPClient()
		httpClient.Transport = transport
		client, err := openWithHTTP(&config.Resolved{
			Name: name, Provider: Provider, BaseURL: "https://api.telegram.test", Target: target,
			Credential: name, Secrets: config.Credential{Type: config.CredentialTypeEnv,
				Values: map[string]string{roleBotToken: "BOT_TOKEN"}},
		}, resolver, red, httpClient)
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	first := open("alerts", "-1001", "111:first_bot_token")
	second := open("operations", "-1002", "222:second_bot_token")
	if _, err := first.SendMessage(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.SendMessage(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	want := []observed{
		{path: "/bot111:first_bot_token/sendMessage", target: "-1001"},
		{path: "/bot222:second_bot_token/sendMessage", target: "-1002"},
	}
	if len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("requests = %#v, want %#v", seen, want)
	}
}

func TestOpenRejectsUnsafeConfigurationBeforeSecretResolution(t *testing.T) {
	tests := []struct {
		name, base, target string
	}{
		{name: "HTTP", base: "http://api.telegram.test", target: "-1001"},
		{name: "userinfo", base: "https://user@api.telegram.test", target: "-1001"},
		{name: "query", base: "https://api.telegram.test?x=1", target: "-1001"},
		{name: "blank target", base: "https://api.telegram.test", target: " "},
		{name: "free target", base: "https://api.telegram.test", target: "chat name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolutions := 0
			resolver := secret.NewWith(func(string) string { resolutions++; return testToken }, nil, nil, nil)
			_, err := openWithHTTP(&config.Resolved{
				Name: "alerts", Provider: Provider, BaseURL: tt.base, Target: tt.target,
				Credential: "notifier", Secrets: config.Credential{Type: config.CredentialTypeEnv,
					Values: map[string]string{roleBotToken: "BOT_TOKEN"}},
			}, resolver, nil, nil)
			if err == nil {
				t.Fatal("openWithHTTP() = nil, want error")
			}
			if resolutions != 0 {
				t.Fatalf("secret resolutions = %d, want 0", resolutions)
			}
		})
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	calls := 0
	client, _ := telegramClient(t, "-1001", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		redirect := response(http.StatusFound, "provider body")
		redirect.Header.Set("Location", "https://other.invalid/capture")
		redirect.Request = request
		return redirect, nil
	}))
	_, err := client.SendMessage(context.Background(), "one")
	if err == nil {
		t.Fatal("SendMessage() = nil, want provider error")
	}
	if calls != 1 {
		t.Fatalf("requests = %d, want 1", calls)
	}
}

func TestRequestBodyIsBoundedByDescriptor(t *testing.T) {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(messagesSend.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(schema["properties"], []byte(`"maxLength":4096`)) {
		t.Fatalf("input schema = %s", messagesSend.InputSchema)
	}
}

func TestContextDeadlineIsTimeout(t *testing.T) {
	client, _ := telegramClient(t, "-1001", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := client.SendMessage(ctx, "one")
	var providerErr *provider.Error
	if !errors.As(err, &providerErr) || providerErr.Class != provider.ClassTimeout {
		t.Fatalf("SendMessage() = %v, want timeout", err)
	}
}
