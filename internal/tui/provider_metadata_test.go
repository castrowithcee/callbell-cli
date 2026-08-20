package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider/bookstack"
	"github.com/castrowithcee/callbell-cli/internal/provider/telegram"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

func TestProviderMetadataDrivesTelegramFormsAndTargets(t *testing.T) {
	reg := capability.NewRegistry()
	if err := bookstack.Register(reg); err != nil {
		t.Fatal(err)
	}
	if err := telegram.Register(reg); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "missing", "config.yaml")
	store := config.NewStore(path, reg)
	m, err := New(store, nil, nil, &redact.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	if m.configExists {
		t.Fatal("a missing config path was treated as an existing file")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("New() created the config file: %v", err)
	}

	m.section = sectionServices
	m.fields = m.buildFields("")
	if got := m.fields[1].choices; len(got) != 2 || got[0] != "bookstack" || got[1] != "telegram" {
		t.Fatalf("provider choices = %v, want BookStack and Telegram registry IDs", got)
	}
	m.fields[1].index = 1
	m.providerChosen("bookstack")
	if got := m.fieldValue("base url"); got != "https://api.telegram.org" {
		t.Fatalf("Telegram default base URL = %q", got)
	}
	m.section = sectionCredentials
	credentialFields := m.buildFields("notifier")
	if len(credentialFields) != 5 || credentialFields[2].label != "bot-token" {
		t.Fatalf("credential fields = %+v, want Telegram bot-token from the registry", credentialFields)
	}
	m.cfg.Services["telegram-main"] = config.Service{Provider: "telegram", BaseURL: "https://api.telegram.org"}
	m.cfg.Credentials["notifier"] = config.Credential{Type: config.CredentialTypeKeyring}
	m.cfg.Connections["alerts"] = config.Connection{Service: "telegram-main", Credential: "notifier", Target: "-1001"}
	m.cfg.Connections["operations"] = config.Connection{Service: "telegram-main", Credential: "notifier", Target: "-1002"}
	m.section = sectionConnections
	connectionFields := m.buildFields("alerts")
	if connectionFields[3].value() != "-1001" || !strings.Contains(connectionFields[3].hint, "required for Telegram") {
		t.Fatalf("target field = value %q, hint %q", connectionFields[3].value(), connectionFields[3].hint)
	}
	if got := m.dashboardEntry(sectionConnections, "operations"); !strings.Contains(got, "-1002") {
		t.Fatalf("dashboard entry = %q, want distinct target", got)
	}
}
