package config

import (
	"sort"
	"strings"
	"testing"
)

type testProviderCatalog map[string]ProviderMetadata

func (p testProviderCatalog) ProviderMetadata(id string) (ProviderMetadata, bool) {
	metadata, ok := p[id]
	return metadata, ok
}

func (p testProviderCatalog) ProviderMetadataAll() []ProviderMetadata {
	all := make([]ProviderMetadata, 0, len(p))
	for _, metadata := range p {
		all = append(all, metadata)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all
}

var testProviders ProviderCatalog = testProviderCatalog{
	"bookstack": {
		ID: "bookstack", Name: "BookStack",
		SecretRoles: []SecretRole{
			{Name: "token-id", Description: "BookStack token ID: the value labeled Token ID when you create an API token"},
			{Name: "token-secret", Description: "BookStack token secret: the value labeled Token Secret when you create the same API token"},
		},
		Target: TargetMetadata{Label: "target"},
	},
	"telegram": {
		ID: "telegram", Name: "Telegram", DefaultBaseURL: "https://api.telegram.org",
		SecretRoles: []SecretRole{{Name: "bot-token", Description: "Telegram bot token"}},
		Target:      TargetMetadata{Label: "chat ID", Required: true},
	},
}

func TestProviderMetadataValidatesTelegramTargets(t *testing.T) {
	const valid = `version: 1
services:
  telegram-main:
    provider: telegram
    base_url: https://api.telegram.org
credentials:
  notifier:
    type: env
    values:
      bot-token: CALLBELL_TELEGRAM_TOKEN
connections:
  operations:
    service: telegram-main
    credential: notifier
    target: "-1001111111111"
  alerts:
    service: telegram-main
    credential: notifier
    target: "-1002222222222"
defaults:
  connections:
    telegram: operations
`
	cfg, err := Decode(strings.NewReader(valid), testProviders)
	if err != nil {
		t.Fatalf("Decode() = %v", err)
	}
	if cfg.Connections["operations"].Target == cfg.Connections["alerts"].Target {
		t.Fatal("two Telegram connections lost their distinct targets")
	}
	if got := cfg.ProviderSecretRoles("telegram"); len(got) != 1 || got[0] != "bot-token" {
		t.Fatalf("Telegram roles = %v, want bot-token", got)
	}

	missing := strings.Replace(valid, `    target: "-1001111111111"`, `    target: ""`, 1)
	if _, err := Decode(strings.NewReader(missing), testProviders); err == nil ||
		!strings.Contains(err.Error(), `provider "telegram" requires chat ID`) {
		t.Fatalf("Decode() error = %v, want required Telegram target", err)
	}
}

func TestProviderMetadataNeverTreatsTargetAsASecret(t *testing.T) {
	metadata, ok := testProviders.ProviderMetadata("telegram")
	if !ok || !metadata.Target.Required || metadata.Target.Label != "chat ID" {
		t.Fatalf("Telegram metadata = %+v, %v", metadata, ok)
	}
	if got := New(testProviders).SecretRoles(); len(got) != 3 || got[0] != "bot-token" {
		t.Fatalf("secret roles = %v", got)
	}
}
