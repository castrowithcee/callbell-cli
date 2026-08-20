package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

func TestRegisterContainsMetadataAndNoOperation(t *testing.T) {
	reg := capability.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register() = %v", err)
	}
	metadata, ok := reg.ProviderMetadata(Provider)
	if !ok || metadata.DefaultBaseURL != defaultURL || !metadata.Target.Required ||
		len(metadata.SecretRoles) != 1 || metadata.SecretRoles[0].Name != roleBotToken {
		t.Fatalf("metadata = %+v, %v", metadata, ok)
	}
	if operations := reg.Provider(Provider); len(operations) != 0 {
		t.Fatalf("operations = %v, want none", operations)
	}
}

func TestConnectionTestUsesOnlyGetMeAndRedactsToken(t *testing.T) {
	const token = "canary-telegram-bot-token-91:abc"
	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	credential := config.Credential{Type: config.CredentialTypeEnv,
		Values: map[string]string{roleBotToken: "TEST_TELEGRAM_BOT_TOKEN"}}
	t.Setenv("TEST_TELEGRAM_BOT_TOKEN", token)
	red := &redact.Redactor{}
	resolver := secret.NewWith(os.Getenv, nil, nil, red)
	class, err := TestConnection(context.Background(), &config.Resolved{
		Name: "alerts", Provider: Provider, BaseURL: server.URL, Credential: "notifier", Secrets: credential,
	}, resolver, red)
	if err != nil || class != provider.ClassOK {
		t.Fatalf("TestConnection() = %q, %v", class, err)
	}
	if gotMethod != http.MethodGet || gotPath != "/bot"+token+"/getMe" {
		t.Fatalf("request = %s %s, want GET /bot<token>/getMe", gotMethod, gotPath)
	}
	if strings.Contains(red.Apply("request "+gotPath), token) {
		t.Fatal("redactor did not remove the Telegram token canary")
	}
}
