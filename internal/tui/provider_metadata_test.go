package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider/bookstack"
	"github.com/castrowithcee/callbell-cli/internal/provider/lexware"
	"github.com/castrowithcee/callbell-cli/internal/provider/seatable"
	"github.com/castrowithcee/callbell-cli/internal/provider/telegram"
	"github.com/castrowithcee/callbell-cli/internal/provider/twentycrm"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

func TestProviderMetadataDrivesMultipleProviderConnections(t *testing.T) {
	reg := capability.NewRegistry()
	if err := bookstack.Register(reg); err != nil {
		t.Fatal(err)
	}
	if err := telegram.Register(reg); err != nil {
		t.Fatal(err)
	}
	if err := lexware.Register(reg); err != nil {
		t.Fatal(err)
	}
	if err := twentycrm.Register(reg); err != nil {
		t.Fatal(err)
	}
	if err := seatable.Register(reg); err != nil {
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
	if got := m.fields[1].choices; len(got) != 5 || got[0] != "bookstack" || got[1] != "lexware" ||
		got[2] != "seatable" || got[3] != "telegram" || got[4] != "twentycrm" {
		t.Fatalf("provider choices = %v, want the BookStack, Lexware, SeaTable, Telegram and Twenty registry IDs", got)
	}
	m.fields[1].index = 1
	m.providerChosen("bookstack")
	if got := m.fieldValue("base url"); got != "https://api.lexware.io" {
		t.Fatalf("Lexware default base URL = %q", got)
	}
	m.fields[1].index = 2
	m.providerChosen("lexware")
	if got := m.fieldValue("base url"); got != "https://cloud.seatable.io" {
		t.Fatalf("SeaTable default instance = %q", got)
	}
	m.fields[1].index = 3
	m.providerChosen("seatable")
	if got := m.fieldValue("base url"); got != "https://api.telegram.org" {
		t.Fatalf("Telegram default base URL = %q", got)
	}
	m.fields[1].index = 4
	m.providerChosen("telegram")
	if got := m.fieldValue("base url"); got != "https://api.twenty.com" {
		t.Fatalf("Twenty CRM default origin = %q", got)
	}
	m.section = sectionCredentials
	credentialFields := m.buildFields("notifier")
	if len(credentialFields) != 7 || credentialFields[2].label != "api-key" ||
		credentialFields[3].label != "api-token" || credentialFields[4].label != "bot-token" {
		t.Fatalf("credential fields = %+v, want the Lexware, SeaTable and Telegram roles from the registry",
			credentialFields)
	}
	m.cfg.Services["telegram-main"] = config.Service{Provider: "telegram", BaseURL: "https://api.telegram.org"}
	m.cfg.Services["lexware-main"] = config.Service{Provider: "lexware", BaseURL: "https://api.lexware.io"}
	m.cfg.Services["wiki-main"] = config.Service{Provider: "bookstack", BaseURL: "https://wiki.example.invalid"}
	m.cfg.Services["crm-cloud"] = config.Service{Provider: "twentycrm", BaseURL: "https://api.twenty.com"}
	m.cfg.Services["crm-selfhosted"] = config.Service{Provider: "twentycrm", BaseURL: "https://crm.example.invalid"}
	m.cfg.Services["tables-cloud"] = config.Service{Provider: "seatable", BaseURL: "https://cloud.seatable.io"}
	m.cfg.Services["tables-onprem"] = config.Service{
		Provider: "seatable", BaseURL: "https://seatable.example.invalid",
	}
	m.cfg.Credentials["notifier"] = config.Credential{Type: config.CredentialTypeKeyring}
	m.cfg.Credentials["reader"] = config.Credential{Type: config.CredentialTypeKeyring}
	m.cfg.Connections["alerts"] = config.Connection{Service: "telegram-main", Credential: "notifier", Target: "-1001"}
	m.cfg.Connections["operations"] = config.Connection{Service: "telegram-main", Credential: "notifier", Target: "-1002"}
	m.cfg.Credentials["accounting"] = config.Credential{Type: config.CredentialTypeKeyring}
	m.cfg.Connections["books-primary"] = config.Connection{Service: "lexware-main", Credential: "accounting"}
	m.cfg.Connections["books-audit"] = config.Connection{Service: "lexware-main", Credential: "accounting"}
	m.cfg.Connections["wiki-primary"] = config.Connection{Service: "wiki-main", Credential: "reader"}
	m.cfg.Connections["wiki-audit"] = config.Connection{Service: "wiki-main", Credential: "reader"}
	m.cfg.Credentials["crm-cloud-reader"] = config.Credential{Type: config.CredentialTypeKeyring}
	m.cfg.Credentials["crm-selfhosted-reader"] = config.Credential{Type: config.CredentialTypeKeyring}
	m.cfg.Connections["crm"] = config.Connection{Service: "crm-cloud", Credential: "crm-cloud-reader"}
	m.cfg.Connections["crm-internal"] = config.Connection{
		Service: "crm-selfhosted", Credential: "crm-selfhosted-reader",
	}
	m.cfg.Credentials["sales-base-reader"] = config.Credential{Type: config.CredentialTypeKeyring}
	m.cfg.Credentials["sales-base-auditor"] = config.Credential{Type: config.CredentialTypeKeyring}
	m.cfg.Credentials["onprem-base-reader"] = config.Credential{Type: config.CredentialTypeKeyring}
	m.cfg.Connections["sales-rows"] = config.Connection{
		Service: "tables-cloud", Credential: "sales-base-reader", Target: "Kunden",
	}
	m.cfg.Connections["sales-rows-audit"] = config.Connection{
		Service: "tables-cloud", Credential: "sales-base-auditor", Target: "Kunden/Aktive",
	}
	m.cfg.Connections["onprem-rows"] = config.Connection{
		Service: "tables-onprem", Credential: "onprem-base-reader", Target: "Tickets",
	}
	m.section = sectionConnections
	connectionFields := m.buildFields("alerts")
	if connectionFields[3].value() != "-1001" || !strings.Contains(connectionFields[3].hint, "required for Telegram") {
		t.Fatalf("target field = value %q, hint %q", connectionFields[3].value(), connectionFields[3].hint)
	}
	if got := m.dashboardEntry(sectionConnections, "operations"); !strings.Contains(got, "-1002") {
		t.Fatalf("dashboard entry = %q, want distinct target", got)
	}
	lexwareFields := m.buildFields("books-primary")
	if strings.Contains(lexwareFields[3].hint, "required") ||
		!strings.Contains(lexwareFields[3].hint, "not used by Lexware") {
		t.Fatalf("Lexware target hint = %q, want an optional target", lexwareFields[3].hint)
	}
	for _, name := range []string{"books-primary", "books-audit"} {
		if got := m.dashboardEntry(sectionConnections, name); !strings.Contains(got, name+" · lexware-main + accounting") {
			t.Fatalf("Lexware dashboard entry = %q, want named connection %q", got, name)
		}
	}
	for _, name := range []string{"wiki-primary", "wiki-audit"} {
		if got := m.dashboardEntry(sectionConnections, name); !strings.Contains(got, name+" · wiki-main + reader") {
			t.Fatalf("BookStack dashboard entry = %q, want named connection %q", got, name)
		}
	}

	// Two Twenty workspaces stay two visibly separate connections, each with its own origin and key, and
	// neither needs a target.
	twentyFields := m.buildFields("crm")
	if strings.Contains(twentyFields[3].hint, "required") ||
		!strings.Contains(twentyFields[3].hint, "not used by Twenty CRM") {
		t.Fatalf("Twenty target hint = %q, want an optional target", twentyFields[3].hint)
	}
	for name, want := range map[string]string{
		"crm":          "crm · crm-cloud + crm-cloud-reader",
		"crm-internal": "crm-internal · crm-selfhosted + crm-selfhosted-reader",
	} {
		if got := m.dashboardEntry(sectionConnections, name); !strings.Contains(got, want) {
			t.Fatalf("Twenty dashboard entry = %q, want %q", got, want)
		}
	}

	// A SeaTable connection names its instance, its base credential, and the fixed table it reads. The
	// table is required, the view is the optional part after the slash, and two tokens of the same base
	// stay two visibly separate connections.
	seatableFields := m.buildFields("sales-rows")
	if seatableFields[3].value() != "Kunden" ||
		!strings.Contains(seatableFields[3].hint, "required for SeaTable") ||
		!strings.Contains(seatableFields[3].hint, "TABLE/VIEW") {
		t.Fatalf("SeaTable target field = value %q, hint %q",
			seatableFields[3].value(), seatableFields[3].hint)
	}
	for name, want := range map[string]string{
		"sales-rows":       "sales-rows · tables-cloud + sales-base-reader",
		"sales-rows-audit": "sales-rows-audit · tables-cloud + sales-base-auditor",
		"onprem-rows":      "onprem-rows · tables-onprem + onprem-base-reader",
	} {
		if got := m.dashboardEntry(sectionConnections, name); !strings.Contains(got, want) {
			t.Fatalf("SeaTable dashboard entry = %q, want %q", got, want)
		}
	}
	if got := m.dashboardEntry(sectionConnections, "sales-rows-audit"); !strings.Contains(got, "Kunden/Aktive") {
		t.Fatalf("SeaTable dashboard entry = %q, want the fixed table and view", got)
	}
}
