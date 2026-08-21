package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// Canary values prove that a secret in the environment never reaches the editor or the file.
const (
	canaryID     = "canary-token-id-4f21"
	canarySecret = "canary-token-secret-9ab3"
)

func testCatalog(t *testing.T) *capability.Registry {
	t.Helper()
	reg := capability.NewRegistry()
	if err := reg.RegisterProvider(config.ProviderMetadata{
		ID: "bookstack", Name: "BookStack",
		SecretRoles: []config.SecretRole{
			{Name: "token-id", Description: "BookStack token ID: the value labeled Token ID when you create an API token; it is not a name you choose"},
			{Name: "token-secret", Description: "BookStack token secret: the value labeled Token Secret when you create the same API token"},
		},
		Target: config.TargetMetadata{Label: "target", Description: "optional provider-specific scope inside the service"},
	}, nil); err != nil {
		t.Fatalf("register test provider: %v", err)
	}
	return reg
}

func newTestStore(t *testing.T, path string) *config.Store {
	t.Helper()
	return config.NewStore(path, testCatalog(t))
}

func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return config.New(testCatalog(t))
}

func loadTestConfig(t *testing.T, path string) (*config.Config, error) {
	t.Helper()
	return config.Load(path, testCatalog(t))
}

func newModel(t *testing.T) (*Model, *config.Store, string) {
	t.Helper()
	return newEnvModel(t, nil)
}

// newEnvModel builds an editor that sees exactly the environment the test names.
func newEnvModel(t *testing.T, env map[string]string) (*Model, *config.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "callbell")
	path := filepath.Join(dir, "config.yaml")
	store := newTestStore(t, path)

	secrets, _ := newResolver(t, dir, env)
	model, err := New(store, nil, secrets, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return model, store, path
}

// newResolver builds a resolver over a credential store that lives in this process only, a plaintext
// fallback inside the test's own directory, and exactly the environment the test names.
//
// The environment is pinned rather than inherited: a variable that happens to be exported where the suite
// runs must not decide what these tests see, and the derived names of this project are guessable enough
// that someone will have one set. No test ever touches the credential store of the machine either.
func newResolver(t *testing.T, dir string, env map[string]string) (*secret.Resolver, *secret.MemoryStore) {
	t.Helper()
	store := secret.NewMemoryStore()
	file := secret.NewFile(filepath.Join(dir, secret.FileName))
	return secret.NewWith(func(name string) string { return env[name] }, store, file, nil), store
}

// keyMsg is the event a terminal sends for one key.
func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
}

// press sends key events the way a terminal would.
func press(t *testing.T, m *Model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		m.Update(keyMsg(k))
	}
}

// pump presses keys and delivers the messages of the commands they produce, the way the event loop does.
// It is how a test drives everything that reaches the credential store.
func pump(t *testing.T, m *Model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		_, cmd := m.Update(keyMsg(k))
		for i := 0; cmd != nil && i < 8; i++ {
			msg := cmd()
			if msg == nil {
				break
			}
			_, cmd = m.Update(msg)
		}
	}
}

// typeText enters a value into the focused field, one rune at a time.
func typeText(t *testing.T, m *Model, text string) {
	t.Helper()
	for _, r := range text {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// clearField sends backspaces to the focused text field.
func clearField(t *testing.T, m *Model) {
	t.Helper()
	for i := 0; i < 64; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	}
}

// openSectionByName navigates from the menu to a section.
func openSectionByName(t *testing.T, m *Model, s section) {
	t.Helper()
	m.screen, m.cursor = screenMenu, 0
	pump(t, m, string(rune('1'+s)))
	if m.section != s {
		t.Fatalf("section = %v, want %v", m.section, s)
	}
}

// addService walks the real key sequence for creating a service.
func addService(t *testing.T, m *Model, name, baseURL string) {
	t.Helper()
	openSectionByName(t, m, sectionServices)
	press(t, m, "n")
	typeText(t, m, name)
	press(t, m, "tab") // provider, the only choice is preselected
	press(t, m, "tab")
	typeText(t, m, baseURL)
	press(t, m, "enter")
}

// addCredential walks the real key sequence for a credential of type env: name, type, one variable name
// per role.
func addCredential(t *testing.T, m *Model, name string, envNames ...string) {
	t.Helper()
	openSectionByName(t, m, sectionCredentials)
	press(t, m, "n")
	typeText(t, m, name)
	// A credential may name its provider; these helpers leave that choice empty, which is what an entry
	// written before the field looks like, and then every compiled role is offered.
	press(t, m, "tab")
	press(t, m, "tab")
	selectChoice(t, m, config.CredentialTypeEnv)
	for _, env := range envNames {
		press(t, m, "tab")
		typeText(t, m, env)
	}
	press(t, m, "enter")
}

// addKeyringCredential creates a credential whose secrets live in the credential store. It names nothing:
// the roles are filled afterwards, through the masked prompt.
func addKeyringCredential(t *testing.T, m *Model, name string) {
	t.Helper()
	openSectionByName(t, m, sectionCredentials)
	press(t, m, "n")
	typeText(t, m, name)
	press(t, m, "tab")
	press(t, m, "tab")
	selectChoice(t, m, config.CredentialTypeKeyring)
	pump(t, m, "enter")
	if m.fail != "" {
		t.Fatalf("creating the keyring credential reported %q", m.fail)
	}
}

func addConnection(t *testing.T, m *Model, name, service, credential string) {
	t.Helper()
	openSectionByName(t, m, sectionConnections)
	press(t, m, "n")
	typeText(t, m, name)
	press(t, m, "tab")
	selectChoice(t, m, service)
	press(t, m, "tab")
	selectChoice(t, m, credential)
	press(t, m, "enter")
}

func selectChoice(t *testing.T, m *Model, want string) {
	t.Helper()
	f := &m.fields[m.focus]
	for i := 0; i < len(f.choices)+1; i++ {
		if f.value() == want {
			return
		}
		press(t, m, "right")
	}
	t.Fatalf("choice %q not reachable in %v", want, f.choices)
}

// The whole MVP configuration flow works through key events alone.
func TestFullConfigurationFlow(t *testing.T) {
	m, store, path := newModel(t)

	addService(t, m, "wiki-primary", "https://wiki.example.invalid")
	addService(t, m, "wiki-archive", "https://archive.example.invalid")
	addCredential(t, m, "reader", "WIKI_READER_ID", "WIKI_READER_SECRET")
	addCredential(t, m, "auditor", "WIKI_AUDITOR_ID", "WIKI_AUDITOR_SECRET")
	// Two credentials on the same service.
	addConnection(t, m, "wiki", "wiki-primary", "reader")
	addConnection(t, m, "wiki-audit", "wiki-primary", "auditor")
	addConnection(t, m, "archive", "wiki-archive", "reader")

	openSectionByName(t, m, sectionDefaults)
	press(t, m, "n")
	typeText(t, m, "knowledge")
	press(t, m, "tab")
	selectChoice(t, m, "wiki")
	press(t, m, "enter")

	if m.fail != "" {
		t.Fatalf("editor reported %q", m.fail)
	}

	saved, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(saved.Services) != 2 || len(saved.Credentials) != 2 || len(saved.Connections) != 3 {
		t.Fatalf("saved configuration = %+v", saved)
	}
	if saved.Connections["wiki"].Service != saved.Connections["wiki-audit"].Service {
		t.Error("the two connections should share one service")
	}
	if saved.Connections["wiki"].Credential == saved.Connections["wiki-audit"].Credential {
		t.Error("the two connections should use different credentials")
	}
	if saved.Defaults.Connections["knowledge"] != "wiki" {
		t.Errorf("default = %q, want wiki", saved.Defaults.Connections["knowledge"])
	}

	// The file itself must be loadable by the ordinary loader.
	if _, err := loadTestConfig(t, path); err != nil {
		t.Errorf("the saved file does not load: %v", err)
	}
}

func TestNavigation(t *testing.T) {
	m, _, _ := newModel(t)

	t.Run("the grid wraps in each direction", func(t *testing.T) {
		m.screen, m.cursor = screenMenu, 0
		press(t, m, "up")
		if m.cursor != int(sectionConnections) {
			t.Errorf("cursor = %d, want Connections", m.cursor)
		}
		press(t, m, "down")
		if m.cursor != 0 {
			t.Errorf("cursor = %d, want 0", m.cursor)
		}
		press(t, m, "left")
		if m.cursor != int(sectionCredentials) {
			t.Errorf("cursor = %d, want Credentials", m.cursor)
		}
		press(t, m, "right")
		if m.cursor != 0 {
			t.Errorf("cursor = %d, want 0", m.cursor)
		}
	})

	t.Run("escape returns from a list to the menu", func(t *testing.T) {
		openSectionByName(t, m, sectionConnections)
		press(t, m, "esc")
		if m.screen != screenMenu {
			t.Errorf("screen = %v, want the menu", m.screen)
		}
		if m.cursor != int(sectionConnections) {
			t.Errorf("cursor = %d, want the section it came from", m.cursor)
		}
	})

	t.Run("form focus wraps in both directions", func(t *testing.T) {
		openSectionByName(t, m, sectionServices)
		press(t, m, "n")
		last := len(m.fields) - 1
		press(t, m, "shift+tab")
		if m.focus != last {
			t.Errorf("focus = %d, want %d", m.focus, last)
		}
		press(t, m, "tab")
		if m.focus != 0 {
			t.Errorf("focus = %d, want 0", m.focus)
		}
	})
}

func TestDashboardLayoutsFitTheirTerminal(t *testing.T) {
	m, _, _ := newEnvModel(t, map[string]string{
		"WIKI_ID":     canaryID,
		"WIKI_SECRET": canarySecret,
	})
	addService(t, m, "wiki", "https://wiki.example.invalid")
	addCredential(t, m, "reader", "WIKI_ID", "WIKI_SECRET")
	addConnection(t, m, "personal", "wiki", "reader")
	m.screen, m.cursor = screenMenu, int(sectionCredentials)

	tests := []struct {
		name         string
		width        int
		height       int
		cardTopLines int
	}{
		{name: "wide two by two", width: 100, height: 24, cardTopLines: 2},
		{name: "narrow single column", width: 60, height: 24, cardTopLines: 4},
		{name: "compact overview", width: 40, height: 12, cardTopLines: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m.Update(tea.WindowSizeMsg{Width: tt.width, Height: tt.height})
			view := m.View()
			assertViewFits(t, view, tt.width, tt.height)
			for _, want := range []string{"1. Services", "2. Credentials", "3. Connections", "4. Defaults"} {
				if !strings.Contains(view, want) {
					t.Errorf("dashboard does not contain %q:\n%s", want, view)
				}
			}
			cardTopLines := 0
			for _, line := range strings.Split(view, "\n") {
				if strings.Contains(line, "╭") {
					cardTopLines++
				}
			}
			if cardTopLines != tt.cardTopLines {
				t.Errorf("card top lines = %d, want %d:\n%s", cardTopLines, tt.cardTopLines, view)
			}
			for _, secretValue := range []string{canaryID, canarySecret} {
				if strings.Contains(view, secretValue) {
					t.Errorf("dashboard exposed a secret value %q:\n%s", secretValue, view)
				}
			}
			if tt.width == 40 {
				compact := strings.Join(strings.Fields(view), "")
				path := strings.Join(strings.Fields(m.dashboardPath()), "")
				if !strings.Contains(compact, path) {
					t.Errorf("compact dashboard lost the config path %q:\n%s", m.dashboardPath(), view)
				}
				if !strings.Contains(compact, "Next:openConnectionsandpressttotest;Defaultsareoptional") {
					t.Errorf("compact dashboard truncated the next step:\n%s", view)
				}
			}
		})
	}

	// The wide cards contain actual configuration rows, not only navigation labels and counts.
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	view := m.View()
	// The provider leads both state rows, so a card reads down its systems before their names. The
	// credential names no provider, but the connection binds it to a BookStack service, so the card says
	// what that already settles.
	for _, want := range []string{"bookstack · wiki", "bookstack · reader · env", "personal · wiki + reader"} {
		if !strings.Contains(view, want) {
			t.Errorf("dashboard does not contain state row %q:\n%s", want, view)
		}
	}
	for _, want := range []string{"server URLs", "secret sources", "service + credential", "optional choices"} {
		if !strings.Contains(view, want) {
			t.Errorf("dashboard card does not explain its purpose %q:\n%s", want, view)
		}
	}

	// A card stays fixed-size when it has more entries than rows: one real entry remains visible, the
	// rest is explicit, and a long cell ends visibly instead of bleeding into its neighbour.
	m.cfg.Services["archive"] = config.Service{Provider: "bookstack", BaseURL: "https://a-very-long-archive-host.example.invalid"}
	m.cfg.Services["backup"] = config.Service{Provider: "bookstack", BaseURL: "https://backup.example.invalid"}
	m.cfg.Services["copy"] = config.Service{Provider: "bookstack", BaseURL: "https://copy.example.invalid"}
	for _, name := range []string{"copy-1", "copy-2", "copy-3", "copy-4"} {
		m.cfg.Services[name] = config.Service{Provider: "bookstack", BaseURL: "https://" + name + ".example.invalid"}
	}
	view = m.View()
	for _, want := range []string{"bookstack · archive", "+2 more", "…"} {
		if !strings.Contains(view, want) {
			t.Errorf("overflowing dashboard card does not contain %q:\n%s", want, view)
		}
	}

	for _, name := range []string{"copy-5"} {
		m.cfg.Services[name] = config.Service{Provider: "bookstack", BaseURL: "https://" + name + ".example.invalid"}
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 18})
	short := m.View()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	tall := m.View()
	assertViewFits(t, short, 100, 18)
	assertViewFits(t, tall, 100, 40)
	if !strings.Contains(short, "more") || strings.Contains(tall, "more") {
		t.Errorf("dashboard rows did not grow with available height:\nshort:\n%s\n\ntall:\n%s", short, tall)
	}
	if len(strings.Split(tall, "\n")) <= len(strings.Split(short, "\n")) {
		t.Errorf("taller dashboard did not expose more table rows")
	}
}

func TestDashboardNavigationAndDirectSectionKeys(t *testing.T) {
	m, _, _ := newModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	press(t, m, "down")
	if m.cursor != int(sectionConnections) {
		t.Fatalf("down in the 2x2 grid focused section %v, want Connections", section(m.cursor))
	}
	press(t, m, "right")
	if m.cursor != int(sectionDefaults) {
		t.Fatalf("right in the 2x2 grid focused section %v, want Defaults", section(m.cursor))
	}
	m.cursor = 0

	for _, key := range []string{"right", "down", "left", "up", "l", "j", "h", "k", "tab", "shift+tab"} {
		before := m.cursor
		press(t, m, key)
		if m.cursor == before {
			t.Errorf("%q did not move dashboard focus", key)
		}
	}

	press(t, m, "3")
	if m.screen != screenList || m.section != sectionConnections {
		t.Fatalf("3 opened screen %v section %v, want Connections list", m.screen, m.section)
	}
	press(t, m, "1")
	if m.screen != screenList || m.section != sectionServices {
		t.Fatalf("1 from a list opened screen %v section %v, want Services list", m.screen, m.section)
	}

	press(t, m, "n")
	if m.screen != screenForm {
		t.Fatalf("n from Services did not use the existing new form: screen %v", m.screen)
	}
	typeText(t, m, "2")
	if m.section != sectionServices || m.fieldValue("name") != "2" {
		t.Errorf("a numeric key escaped the form: section %v name %q", m.section, m.fieldValue("name"))
	}

	press(t, m, "esc", "esc", "3")
	if m.section != sectionConnections || m.screen != screenList {
		t.Fatalf("3 did not open Connections from the dashboard")
	}
	press(t, m, "n")
	if m.screen != screenList || !strings.Contains(m.fail, "Create a service and a credential") {
		t.Errorf("dashboard/list new did not preserve prerequisite flow: screen %v error %q", m.screen, m.fail)
	}
}

func TestTinyTerminalIsBoundedAndResizePreservesState(t *testing.T) {
	m, _, _ := newModel(t)
	m.screen, m.cursor = screenMenu, int(sectionDefaults)

	for _, size := range []struct{ width, height int }{
		{39, 12}, {40, 11}, {12, 4}, {1, 1}, {0, 0}, {-4, -2},
	} {
		m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		view := m.View()
		width, height := max(size.width, 1), max(size.height, 1)
		assertViewFits(t, view, width, height)
		if !strings.Contains(view, "q") {
			t.Errorf("%dx%d resize view has no visible q key: %q", size.width, size.height, view)
		}
		press(t, m, "1", "n", "enter")
		if m.screen != screenMenu || m.cursor != int(sectionDefaults) {
			t.Fatalf("%dx%d changed hidden state to screen %v cursor %d", size.width, size.height,
				m.screen, m.cursor)
		}
	}

	m.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	if m.screen != screenMenu || m.cursor != int(sectionDefaults) {
		t.Fatalf("resize lost dashboard state: screen %v cursor %d", m.screen, m.cursor)
	}
	if !strings.Contains(m.View(), "> 4. Defaults") {
		t.Errorf("restored dashboard lost focus:\n%s", m.View())
	}

	// The resize overlay is not a new editor screen. A half-completed form remains intact behind it.
	form, _, _ := newModel(t)
	press(t, form, "1", "n")
	typeText(t, form, "draft-service")
	press(t, form, "tab")
	focus := form.focus
	form.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	if view := form.View(); !strings.Contains(view, "Resize terminal") {
		t.Errorf("40x12 form did not degrade to the bounded resize view:\n%s", view)
	} else {
		assertViewFits(t, view, 40, 12)
	}
	form.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	press(t, form, "tab", "n", "1")
	form.Update(tea.WindowSizeMsg{Width: 80, Height: 100})
	if form.screen != screenForm || form.focus != focus || form.fieldValue("name") != "draft-service" {
		t.Errorf("resize changed form state: screen %v focus %d name %q", form.screen, form.focus,
			form.fieldValue("name"))
	}
	if strings.Contains(form.View(), "Resize terminal") {
		t.Errorf("restored form still shows the resize overlay:\n%s", form.View())
	}
}

func assertViewFits(t *testing.T, view string, width, height int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		t.Errorf("view has %d lines, want at most %d:\n%s", len(lines), height, view)
	}
	for _, line := range lines {
		if got := lipgloss.Width(line); got > width {
			t.Errorf("view line has %d cells, want at most %d: %q", got, width, line)
		}
	}
}

// Cancelling a form writes nothing.
func TestCancelWritesNothing(t *testing.T) {
	m, _, path := newModel(t)
	addService(t, m, "wiki", "https://wiki.example.invalid")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	openSectionByName(t, m, sectionServices)
	press(t, m, "n")
	typeText(t, m, "discarded")
	press(t, m, "esc")

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != string(before) {
		t.Error("the file changed although the form was cancelled")
	}
	if _, ok := m.cfg.Services["discarded"]; ok {
		t.Error("the cancelled entry reached the model")
	}
	if m.status != "Cancelled" {
		t.Errorf("status = %q", m.status)
	}
}

func TestValidationErrorsAreShown(t *testing.T) {
	tests := []struct {
		name   string
		build  func(t *testing.T, m *Model)
		wantIn string
	}{
		{
			name: "an empty name is refused",
			build: func(t *testing.T, m *Model) {
				openSectionByName(t, m, sectionServices)
				press(t, m, "n")
				press(t, m, "enter")
			},
			wantIn: "name must not be empty",
		},
		{
			name: "a missing base url is refused by the core",
			build: func(t *testing.T, m *Model) {
				openSectionByName(t, m, sectionServices)
				press(t, m, "n")
				typeText(t, m, "wiki")
				press(t, m, "enter")
			},
			wantIn: "base_url",
		},
		{
			name: "an unusable base url is refused by the core",
			build: func(t *testing.T, m *Model) {
				addService(t, m, "wiki", "ftp://wiki.example.invalid")
			},
			wantIn: "scheme http or https",
		},
		{
			name: "a credential without any role is refused by the core",
			build: func(t *testing.T, m *Model) {
				addCredential(t, m, "empty")
			},
			wantIn: "at least one secret role",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, path := newModel(t)

			tt.build(t, m)

			if !strings.Contains(m.fail, tt.wantIn) {
				t.Errorf("error = %q, want it to contain %q", m.fail, tt.wantIn)
			}
			if _, err := os.Stat(path); err == nil {
				t.Error("a file was written although the entry was invalid")
			}
			if m.screen != screenForm {
				t.Errorf("screen = %v, want to stay in the form", m.screen)
			}
		})
	}
}

func TestDeleteNeedsConfirmation(t *testing.T) {
	m, store, _ := newModel(t)
	addService(t, m, "wiki", "https://wiki.example.invalid")

	t.Run("answering no keeps the entry", func(t *testing.T) {
		openSectionByName(t, m, sectionServices)
		press(t, m, "d")
		if m.screen != screenConfirm {
			t.Fatalf("screen = %v, want the confirmation", m.screen)
		}
		press(t, m, "n")

		saved, err := store.Load()
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if _, ok := saved.Services["wiki"]; !ok {
			t.Error("the entry was deleted although the answer was no")
		}
	})

	t.Run("escape also keeps the entry", func(t *testing.T) {
		openSectionByName(t, m, sectionServices)
		press(t, m, "d")
		press(t, m, "esc")

		if _, ok := m.cfg.Services["wiki"]; !ok {
			t.Error("the entry was deleted although the confirmation was cancelled")
		}
	})

	t.Run("answering yes deletes the entry", func(t *testing.T) {
		openSectionByName(t, m, sectionServices)
		press(t, m, "d")
		press(t, m, "y")

		saved, err := store.Load()
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if _, ok := saved.Services["wiki"]; ok {
			t.Error("the entry survived the confirmation")
		}
		if !strings.HasPrefix(m.status, "Deleted") {
			t.Errorf("status = %q", m.status)
		}
	})
}

// A referenced entry cannot be deleted, and the reason is shown.
func TestDeleteRefusedWhileReferenced(t *testing.T) {
	m, store, _ := newModel(t)
	addService(t, m, "wiki", "https://wiki.example.invalid")
	addCredential(t, m, "reader", "WIKI_ID", "WIKI_SECRET")
	addConnection(t, m, "wiki", "wiki", "reader")

	openSectionByName(t, m, sectionServices)
	press(t, m, "d")
	press(t, m, "y")

	if !strings.Contains(m.fail, "still used by") {
		t.Errorf("error = %q", m.fail)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if _, ok := saved.Services["wiki"]; !ok {
		t.Error("the referenced service was deleted")
	}
}

// The name of an existing entry cannot be changed, because renaming would break every reference to it.
func TestNameOfAnExistingEntryIsReadOnly(t *testing.T) {
	m, store, _ := newModel(t)
	addService(t, m, "wiki", "https://wiki.example.invalid")

	openSectionByName(t, m, sectionServices)
	press(t, m, "enter")
	if !m.fields[0].readOnly {
		t.Fatal("the name field of an existing entry should be read only")
	}
	// The field takes no editing focus either, so the keys of a rename attempt go somewhere else entirely
	// and the stored name cannot change by any route.
	if m.focus == 0 {
		t.Fatal("the form opened on the locked field")
	}
	clearField(t, m)
	typeText(t, m, "renamed")
	press(t, m, "enter")

	saved, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if _, ok := saved.Services["wiki"]; !ok {
		t.Errorf("the entry lost its name: %+v", saved.Services)
	}
	if _, ok := saved.Services["renamed"]; ok {
		t.Error("a rename happened although the field is read only")
	}
}

// Neither the editor nor the file ever carries a secret value.
func TestNoSecretValues(t *testing.T) {
	m, _, path := newEnvModel(t, map[string]string{
		"WIKI_READER_ID":     canaryID,
		"WIKI_READER_SECRET": canarySecret,
	})
	addService(t, m, "wiki", "https://wiki.example.invalid")
	addCredential(t, m, "reader", "WIKI_READER_ID", "WIKI_READER_SECRET")
	addConnection(t, m, "wiki", "wiki", "reader")

	// Every screen the editor can show.
	var rendered strings.Builder
	for _, s := range []section{sectionServices, sectionCredentials, sectionConnections, sectionDefaults} {
		openSectionByName(t, m, s)
		rendered.WriteString(m.View())
		press(t, m, "n")
		rendered.WriteString(m.View())
		press(t, m, "esc")
	}
	openSectionByName(t, m, sectionCredentials)
	rendered.WriteString(m.View())
	press(t, m, "enter")
	rendered.WriteString(m.View())

	for _, canary := range []string{canaryID, canarySecret} {
		if strings.Contains(rendered.String(), canary) {
			t.Errorf("a secret value reached the screen:\n%s", rendered.String())
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, canary := range []string{canaryID, canarySecret} {
		if strings.Contains(string(data), canary) {
			t.Errorf("a secret value reached the file:\n%s", data)
		}
	}
	// The variable names and the source that delivers are what the editor shows instead.
	if !strings.Contains(rendered.String(), "WIKI_READER_ID") ||
		!strings.Contains(rendered.String(), "("+string(secret.SourceEnv)+")") {
		t.Errorf("the editor should show the variable name and its source:\n%s", rendered.String())
	}
}

// A user may paste the secret itself into a credential field instead of the variable name. The editor
// refuses it, and the pasted value reaches no message, no other screen and no file.
func TestPastedSecretIsRefusedWithoutEchoingIt(t *testing.T) {
	const pasted = "canary-pasted-secret-6b28d4"

	m, _, path := newModel(t)
	addService(t, m, "wiki", "https://wiki.example.invalid")
	addCredential(t, m, "reader", pasted, "WIKI_READER_SECRET")

	if m.fail == "" {
		t.Fatal("the pasted secret was accepted, want a refusal")
	}
	if strings.Contains(m.fail, pasted) {
		t.Errorf("the error message echoes the pasted secret: %q", m.fail)
	}
	if !strings.Contains(m.fail, "token-id") {
		t.Errorf("error = %q, want it to name the role", m.fail)
	}
	if _, ok := m.cfg.Credentials["reader"]; ok {
		t.Error("the refused credential reached the model")
	}

	// Leave the form the way a user would, then render every screen the editor can show.
	press(t, m, "esc")
	var rendered strings.Builder
	for _, s := range []section{sectionServices, sectionCredentials, sectionConnections, sectionDefaults} {
		openSectionByName(t, m, s)
		rendered.WriteString(m.View())
		press(t, m, "n")
		rendered.WriteString(m.View())
		press(t, m, "esc")
		rendered.WriteString(m.View())
	}
	if strings.Contains(rendered.String(), pasted) {
		t.Errorf("the pasted secret reached a screen:\n%s", rendered.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), pasted) {
		t.Errorf("the pasted secret reached the file:\n%s", data)
	}
}

// A name outside the allowed character set is refused, and the refusal states the rule.
func TestNameWithASpaceIsRefused(t *testing.T) {
	m, _, path := newModel(t)

	addService(t, m, "wiki primary", "https://wiki.example.invalid")

	if m.fail == "" {
		t.Fatal("a name with a space was accepted")
	}
	if !strings.Contains(m.fail, "must consist of letters, digits") {
		t.Errorf("error = %q, want it to state the name rule", m.fail)
	}
	if m.screen != screenForm {
		t.Errorf("screen = %v, want to stay in the form", m.screen)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a file was written although the name was invalid")
	}
}

// The focused field is drawn by the text input, so the form shows where the cursor stands. The proof is
// that the rendering changes when nothing but the cursor moves: the plain value cannot depend on the cursor.
func TestTheFormShowsWhereTheCursorStands(t *testing.T) {
	const url = "https://wiki.example.invalid"

	m, _, _ := newModel(t)
	openSectionByName(t, m, sectionServices)
	press(t, m, "n")
	press(t, m, "tab", "tab") // name, provider, base url
	typeText(t, m, url)

	if got := m.fields[m.focus].label; got != "base url" {
		t.Fatalf("focused field = %q, want the base url", got)
	}
	atEnd := m.View()
	if !strings.Contains(atEnd, url) {
		t.Fatalf("the typed value is missing from the form:\n%s", atEnd)
	}
	// The cursor sits past the last character, so the text input draws one more cell after the value.
	if !strings.Contains(atEnd, url+" \n") {
		t.Errorf("no cursor cell behind the value, the field is drawn from the plain value:\n%s", atEnd)
	}

	press(t, m, "left", "left", "left")
	if got, want := m.fields[m.focus].input.Position(), len(url)-3; got != want {
		t.Fatalf("cursor position = %d, want %d", got, want)
	}
	inTheMiddle := m.View()
	if inTheMiddle == atEnd {
		t.Errorf("the form is identical wherever the cursor stands:\n%s", inTheMiddle)
	}

	// A cursor that is only visible while a blink timer runs would vanish; this one is always drawn.
	if got := m.fields[m.focus].input.Cursor.Mode(); got != cursor.CursorStatic {
		t.Errorf("cursor mode = %v, want a static one", got)
	}
	if m.fields[m.focus].input.Cursor.Blink {
		t.Error("the cursor of the focused field is currently not drawn")
	}
	if !m.fields[0].input.Cursor.Blink {
		t.Error("an unfocused field should not draw a cursor")
	}
}

// The form shows what a field expects, above all that a credential field wants a variable name.
func TestFieldHintsSayWhatAFieldExpects(t *testing.T) {
	m, _, _ := newModel(t)

	openSectionByName(t, m, sectionCredentials)
	press(t, m, "n")
	press(t, m, "tab")
	press(t, m, "tab")
	selectChoice(t, m, config.CredentialTypeEnv)
	view := m.View()
	words := strings.Join(strings.Fields(view), " ")
	for _, want := range []string{"the NAME of an environment variable", "never the secret"} {
		if !strings.Contains(words, want) {
			t.Errorf("the credential form does not say %q:\n%s", want, view)
		}
	}
	for _, want := range []string{"value labeled Token ID", "not a name you choose", "value labeled Token Secret"} {
		if !strings.Contains(words, want) {
			t.Errorf("the credential form does not explain %q:\n%s", want, view)
		}
	}
	// The sentence is about the kind of row, so it stands once and speaks of every role row. A hint is
	// wrapped into the terminal, so the test looks for a fragment that survives wrapping rather than for
	// the whole sentence.
	if got := strings.Count(view, "the NAME of an environment"); got != 1 {
		t.Errorf("hint appears %d times, want exactly once for all role rows:\n%s", got, view)
	}
	if !strings.Contains(view, "every role row") {
		t.Errorf("the hint does not say that it is about every role row:\n%s", view)
	}
	if !strings.Contains(view, "a key you choose, without spaces") {
		t.Errorf("the name field has no hint:\n%s", view)
	}
	if !strings.Contains(view, "keyring keeps the secrets") {
		t.Errorf("the form does not say what the type decides:\n%s", view)
	}

	openSectionByName(t, m, sectionServices)
	press(t, m, "n")
	view = m.View()
	for _, want := range []string{nameHint, baseURLHint} {
		if !strings.Contains(view, want) {
			t.Errorf("the service form does not show %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "--connection") || !strings.Contains(view, "http or https") {
		t.Errorf("the hints do not say what name and base url are for:\n%s", view)
	}

	m, _, _ = newModel(t)
	addService(t, m, "wiki", "https://wiki.example.invalid")
	addCredential(t, m, "reader", "WIKI_ID", "WIKI_SECRET")
	addConnection(t, m, "wiki", "wiki", "reader")
	openSectionByName(t, m, sectionDefaults)
	press(t, m, "n")
	if view := m.View(); !strings.Contains(view, domainHint) {
		t.Errorf("the domain field has no hint:\n%s", view)
	}
}

// openEntryForm opens the form of the single entry of a section.
func openEntryForm(t *testing.T, m *Model, s section, name string) {
	t.Helper()
	openSectionByName(t, m, s)
	pump(t, m, "enter")
	if m.screen != screenForm || m.editing != name {
		t.Fatalf("screen = %v, editing = %q, want the form of %q", m.screen, m.editing, name)
	}
}

// A connection description is written, changed, and taken away again through the form alone, and what the
// store keeps is exactly what stood in the field. The field also says what makes it different from every
// other field of this editor: what is typed here is published by discovery.
func TestConnectionDescriptionIsEditedThroughTheForm(t *testing.T) {
	m, store, _ := newModel(t)
	addService(t, m, "wiki", "https://wiki.example.invalid")
	addCredential(t, m, "reader", "WIKI_ID", "WIKI_SECRET")
	addConnection(t, m, "wiki", "wiki", "reader")

	saved, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := saved.Connections["wiki"].Description; got != "" {
		t.Errorf("a new connection starts with the description %q, want none", got)
	}

	for _, step := range []struct{ typed, want string }{
		{"the live team wiki, writes land here", "the live team wiki, writes land here"},
		{"the same instance, read for audits only", "the same instance, read for audits only"},
		{"", ""},
	} {
		openEntryForm(t, m, sectionConnections, "wiki")
		focusField(t, m, "description")
		clearField(t, m)
		typeText(t, m, step.typed)
		press(t, m, "enter")
		if m.fail != "" {
			t.Fatalf("editor reported %q", m.fail)
		}
		saved, err := store.Load()
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if got := saved.Connections["wiki"].Description; got != step.want {
			t.Errorf("saved description = %q, want %q", got, step.want)
		}
		// The route itself is untouched by a change to the text that describes it.
		if saved.Connections["wiki"].Service != "wiki" || saved.Connections["wiki"].Credential != "reader" {
			t.Errorf("the connection changed: %+v", saved.Connections["wiki"])
		}
	}

	openEntryForm(t, m, sectionConnections, "wiki")
	view := strings.Join(strings.Fields(m.View()), " ")
	for _, want := range []string{"description", "discovery publishes it", "never carry a secret"} {
		if !strings.Contains(view, want) {
			t.Errorf("the connection form does not say %q:\n%s", want, m.View())
		}
	}
}

// A description the core refuses is reported in the editor and never reaches the file.
func TestATooLongConnectionDescriptionIsRefused(t *testing.T) {
	m, store, _ := newModel(t)
	// A line of 201 characters only fits into a terminal wide enough to draw it; the editor refuses to
	// work in one that cannot show what is being edited.
	m.Update(tea.WindowSizeMsg{Width: 400, Height: 200})
	addService(t, m, "wiki", "https://wiki.example.invalid")
	addCredential(t, m, "reader", "WIKI_ID", "WIKI_SECRET")
	addConnection(t, m, "wiki", "wiki", "reader")

	openEntryForm(t, m, sectionConnections, "wiki")
	focusField(t, m, "description")
	typeText(t, m, strings.Repeat("a", 201))
	press(t, m, "enter")

	if m.fail == "" || !strings.Contains(m.fail, "201 characters") {
		t.Errorf("fail = %q, want the reason the core gave", m.fail)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := saved.Connections["wiki"].Description; got != "" {
		t.Errorf("saved description = %q, want the refused text nowhere on disk", got)
	}
}

// What the form shows is what the store gets: spaces at the edges are dropped where the user can see it.
func TestSpacesAtTheEdgesAreTrimmedVisibly(t *testing.T) {
	m, store, _ := newModel(t)
	openSectionByName(t, m, sectionServices)
	press(t, m, "n")
	typeText(t, m, "  wiki  ")
	press(t, m, "tab")

	if got := m.fields[0].input.Value(); got != "wiki" {
		t.Errorf("the field still holds %q after leaving it", got)
	}
	if view := m.View(); strings.Contains(view, "  wiki  ") {
		t.Errorf("the form still shows spaces the store would drop:\n%s", view)
	}

	press(t, m, "tab")
	typeText(t, m, " https://wiki.example.invalid ")
	press(t, m, "enter")

	if m.fail != "" {
		t.Fatalf("editor reported %q", m.fail)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := saved.Services["wiki"].BaseURL; got != "https://wiki.example.invalid" {
		t.Errorf("saved base url = %q", got)
	}
}

// A variable that carries nothing is reported as missing, again without reading any value. The answer for
// a credential of type env comes from the environment alone: its resolution ends there, so asking costs no
// store round trip.
func TestEnvSourceReportsTheStageWithoutTheValue(t *testing.T) {
	m, _, _ := newEnvModel(t, map[string]string{"CALLBELL_TUI_PRESENT": "value"})

	if got, want := m.envSource("CALLBELL_TUI_PRESENT"), string(secret.SourceEnv); got != want {
		t.Errorf("envSource() = %q, want %q", got, want)
	}
	if got, want := m.envSource("CALLBELL_TUI_ABSENT"), string(secret.SourceMissing); got != want {
		t.Errorf("envSource() = %q, want %q", got, want)
	}
	if got := m.envSource(""); got != sourceUnnamed {
		t.Errorf("envSource() = %q, want %q", got, sourceUnnamed)
	}
}

// The editor starts on a machine that has no configuration yet.
func TestStartsWithoutAConfigurationFile(t *testing.T) {
	m, _, path := newModel(t)

	if m.cfg == nil || m.cfg.Version != config.Version {
		t.Fatalf("configuration = %+v", m.cfg)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a file was created before anything was saved")
	}
	view := m.View()
	for _, want := range []string{
		"created on first save",
		"Next: add a Service",
		"1. Services",
		"2. Credentials",
		"3. Connections",
		"4. Defaults",
		"Config:",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("first-run view does not contain %q:\n%s", want, view)
		}
	}
	if compact := strings.Join(strings.Fields(view), ""); !strings.Contains(compact, path) {
		t.Errorf("first-run view does not contain the config path %q:\n%s", path, view)
	}
	if strings.Contains(view, "Loaded") {
		t.Errorf("a missing file is described as loaded:\n%s", view)
	}
}

func TestDependentSectionsExplainWhatMustBeCreatedFirst(t *testing.T) {
	m, _, _ := newModel(t)

	openSectionByName(t, m, sectionConnections)
	view := m.View()
	for _, want := range []string{"Create a service and a credential", "Press esc"} {
		if !strings.Contains(view, want) {
			t.Errorf("empty Connections does not contain %q:\n%s", want, view)
		}
	}
	press(t, m, "n")
	if m.screen != screenList {
		t.Fatalf("screen = %v, want to stay on the list without prerequisites", m.screen)
	}
	if !strings.Contains(m.fail, "Create a service and a credential") {
		t.Errorf("error = %q", m.fail)
	}

	openSectionByName(t, m, sectionDefaults)
	if view := m.View(); !strings.Contains(view, "Create a connection") ||
		!strings.Contains(view, "Defaults are optional") {
		t.Errorf("empty Defaults does not explain its prerequisite:\n%s", view)
	}
}

func TestNewKeyringCredentialContinuesWithItsSecrets(t *testing.T) {
	m, _, path := newModel(t)

	openSectionByName(t, m, sectionCredentials)
	press(t, m, "n")
	typeText(t, m, "wiki-reader")
	press(t, m, "tab")
	press(t, m, "tab")
	selectChoice(t, m, config.CredentialTypeKeyring)
	press(t, m, "tab")

	before := m.View()
	if !strings.Contains(before, "enter save credential first") {
		t.Errorf("new credential does not explain the first save:\n%s", before)
	}
	if !strings.Contains(before, "value labeled Token ID") || !strings.Contains(before, "not a name you choose") {
		t.Errorf("new credential does not explain token-id:\n%s", before)
	}
	press(t, m, "enter")

	if m.screen != screenForm || m.editing != "wiki-reader" {
		t.Fatalf("screen = %v editing = %q, want the saved credential form", m.screen, m.editing)
	}
	if m.fields[m.focus].kind != fieldSecret {
		t.Fatalf("focused field kind = %v, want a secret role", m.fields[m.focus].kind)
	}
	view := strings.Join(strings.Fields(m.View()), " ")
	for _, want := range []string{"Credential saved", "press s on each role", "p if the system credential store"} {
		if !strings.Contains(view, want) {
			t.Errorf("continued credential form does not contain %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "created on first save") {
		t.Errorf("the saved file is still described as not created:\n%s", view)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("first save did not create the configuration: %v", err)
	}
}

// formLines is the view without the padding the styles add, so a test can look at the shape of the form
// rather than at the cells that fill it out.
func formLines(m *Model) []string {
	lines := strings.Split(m.View(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}

// fieldLine is the line the given field is drawn on. Field lines carry the two-cell margin or the focus
// marker; hint lines are indented further, so they cannot be mistaken for one.
func fieldLine(t *testing.T, lines []string, label string) int {
	t.Helper()
	for i, l := range lines {
		body := strings.TrimPrefix(l, "> ")
		if body == l {
			body = strings.TrimPrefix(l, "  ")
			if body == l {
				continue
			}
		}
		if strings.HasPrefix(body, label) {
			return i
		}
	}
	t.Fatalf("field %q is not on screen:\n%s", label, strings.Join(lines, "\n"))
	return -1
}

// hintBlocks collects every hint of the form, each one joined back into the sentence it was wrapped from.
func hintBlocks(lines []string) []string {
	var blocks []string
	var current []string
	flush := func() {
		if len(current) > 0 {
			blocks = append(blocks, strings.Join(current, " "))
			current = nil
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "    ") {
			current = append(current, strings.TrimSpace(l))
			continue
		}
		flush()
	}
	flush()
	return blocks
}

// The name of an existing entry says that it is locked, keeps no hint that claims otherwise, and cannot be
// focused for editing, while every other field stays reachable and the form still saves.
func TestALockedNameSaysSoAndTakesNoEditingFocus(t *testing.T) {
	m, store, _ := newModel(t)
	addService(t, m, "wiki", "https://wiki.example.invalid")

	openSectionByName(t, m, sectionServices)
	press(t, m, "enter")

	view := m.View()
	if !strings.Contains(view, "read-only") || !strings.Contains(view, "create it again to rename") {
		t.Errorf("the locked field does not say that it is locked:\n%s", view)
	}
	if strings.Contains(view, "connections and --connection refer to it") {
		t.Errorf("the locked field still claims the name can be chosen:\n%s", view)
	}
	// Locked is not hidden: the value stays on screen, it just cannot be entered.
	if !strings.Contains(view, "wiki") {
		t.Errorf("the locked field no longer shows its value:\n%s", view)
	}

	// Walking the form in both directions never stops on it, and the text input behind it stays blurred.
	visited := map[string]bool{}
	for _, key := range []string{"tab", "tab", "tab", "shift+tab", "shift+tab", "shift+tab", "down", "up"} {
		press(t, m, key)
		if m.focus == 0 {
			t.Fatalf("%q put the focus on the locked field", key)
		}
		if m.fields[0].input.Focused() {
			t.Fatalf("the locked field took editing focus after %q", key)
		}
		visited[m.fields[m.focus].label] = true
	}
	for _, f := range m.fields {
		if !f.readOnly && !visited[f.label] {
			t.Errorf("field %q cannot be reached any more", f.label)
		}
	}

	// The form is not a trap: it still saves, under the name that could not be changed.
	for i := 0; i < len(m.fields) && m.fields[m.focus].label != "base url"; i++ {
		press(t, m, "tab")
	}
	if got := m.fields[m.focus].label; got != "base url" {
		t.Fatalf("focused field = %q, want the base url", got)
	}
	clearField(t, m)
	typeText(t, m, "https://wiki.example.invalid/next")
	press(t, m, "enter")

	if m.fail != "" {
		t.Fatalf("editor reported %q", m.fail)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := saved.Services["wiki"].BaseURL; got != "https://wiki.example.invalid/next" {
		t.Errorf("saved base url = %q, the form did not save through the locked field", got)
	}
}

// A form whose first field is locked opens on the first field that takes input, in every section that has
// one.
func TestALockedFieldDoesNotSwallowTheOpeningFocus(t *testing.T) {
	m, _, _ := newModel(t)
	addService(t, m, "wiki", "https://wiki.example.invalid")
	addCredential(t, m, "reader", "WIKI_READER_ID", "WIKI_READER_SECRET")
	addConnection(t, m, "wiki", "wiki", "reader")

	openSectionByName(t, m, sectionDefaults)
	press(t, m, "n")
	typeText(t, m, "wiki.example.invalid")
	press(t, m, "tab")
	selectChoice(t, m, "wiki")
	press(t, m, "enter")
	if m.fail != "" {
		t.Fatalf("editor reported %q", m.fail)
	}

	for _, s := range []section{sectionServices, sectionCredentials, sectionConnections, sectionDefaults} {
		openSectionByName(t, m, s)
		pump(t, m, "enter")
		if m.screen != screenForm {
			t.Fatalf("%v did not open a form", s)
		}
		if m.fields[m.focus].readOnly {
			t.Errorf("%v opened its form on a locked field (%q)", s, m.fields[m.focus].label)
		}
		press(t, m, "esc")
	}
}

// A hint belongs to the field above it: it stands directly under its own row, and a blank line separates
// the block from the next field. It holds at a width that wraps the hints too.
func TestAHintBelongsToTheFieldAboveIt(t *testing.T) {
	for _, width := range []int{100, 46} {
		m, _, _ := newModel(t)
		m.Update(tea.WindowSizeMsg{Width: width})
		openSectionByName(t, m, sectionCredentials)
		press(t, m, "n")
		press(t, m, "tab")
		press(t, m, "tab")
		selectChoice(t, m, config.CredentialTypeEnv)

		lines := formLines(m)
		wrappedHints := 0
		for i, f := range m.fields {
			at := fieldLine(t, lines, f.label)
			if i > 0 && lines[at-1] != "" {
				t.Errorf("width %d: field %q is not separated from the block above it:\n%s",
					width, f.label, strings.Join(lines, "\n"))
			}
			hint := m.fieldHint(f)
			if hint == "" {
				if at+1 < len(lines) && strings.HasPrefix(lines[at+1], "    ") {
					t.Errorf("width %d: field %q has no hint but an indented line under it:\n%s",
						width, f.label, strings.Join(lines, "\n"))
				}
				continue
			}
			// The hint starts on the very next line, and every continuation line of a wrapped hint stays
			// inside the same block, on the same indent.
			height := 0
			for j := at + 1; j < len(lines) && lines[j] != ""; j++ {
				if !strings.HasPrefix(lines[j], "    ") {
					t.Errorf("width %d: the hint block of %q leaves its indent at %q:\n%s",
						width, f.label, lines[j], strings.Join(lines, "\n"))
				}
				height++
			}
			if height == 0 {
				t.Errorf("width %d: the hint of %q does not stand under it:\n%s",
					width, f.label, strings.Join(lines, "\n"))
			}
			if height > 1 {
				wrappedHints++
			}
		}
		if width == 46 && wrappedHints == 0 {
			t.Errorf("width %d: no hint wrapped, the narrow case is not being tested:\n%s",
				width, strings.Join(lines, "\n"))
		}
	}
}

// The same sentence never stands twice under one another. What all role rows have in common is said once;
// what differs per role stays on its own row.
func TestNoHintStandsTwice(t *testing.T) {
	m, _, _ := newModel(t)

	openSectionByName(t, m, sectionCredentials)
	press(t, m, "n")
	press(t, m, "tab")
	press(t, m, "tab")
	selectChoice(t, m, config.CredentialTypeEnv)
	assertNoRepeatedHint(t, m, "the env credential form")
	if got := strings.Count(m.View(), "the NAME of an environment"); got != 1 {
		t.Errorf("the env sentence stands %d times, want once:\n%s", got, m.View())
	}

	// The keyring rows build their hint themselves, out of the keys and the stages the resolver checked.
	addKeyringCredential(t, m, "store")
	openSectionByName(t, m, sectionCredentials)
	pump(t, m, "enter")
	if m.screen != screenForm {
		t.Fatalf("screen = %v, want the form of the keyring credential", m.screen)
	}
	assertNoRepeatedHint(t, m, "the keyring credential form")

	view := m.View()
	words := strings.Join(strings.Fields(view), " ")
	if got := strings.Count(words, "p unencrypted file (asks first)"); got != 1 {
		t.Errorf("the secret keys stand %d times, want once:\n%s", got, view)
	}
	// The stages differ per role, so every role keeps its own.
	if got, want := strings.Count(view, "checked:"), len(m.cfg.SecretRoles()); got != want {
		t.Errorf("the checked stages appear %d times, want once per role (%d):\n%s", got, want, view)
	}
}

func assertNoRepeatedHint(t *testing.T, m *Model, what string) {
	t.Helper()
	blocks := hintBlocks(formLines(m))
	seen := map[string]bool{}
	for _, block := range blocks {
		if seen[block] {
			t.Errorf("%s says the same hint twice: %q\n%s", what, block, m.View())
		}
		seen[block] = true
	}
	if len(blocks) == 0 {
		t.Errorf("%s shows no hint at all:\n%s", what, m.View())
	}
}

// A form too tall for its terminal drops the hints it can spare instead of becoming unreachable, and the
// resize notice is never the only way out of the editor.
func TestATallFormStaysUsableInASmallTerminal(t *testing.T) {
	m, _, _ := newModel(t)
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 80})
	openSectionByName(t, m, sectionCredentials)
	press(t, m, "n")
	full := strings.Count(m.View(), "\n")

	// The same form in a terminal that cannot hold it: it is still the form, only denser.
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	view := m.View()
	if strings.Contains(view, "Resize terminal") {
		t.Fatalf("the form gave up instead of tightening:\n%s", view)
	}
	if dense := strings.Count(view, "\n"); dense >= full {
		t.Errorf("dense form = %d lines, want fewer than the %d of the full one", dense, full)
	}
	for _, want := range []string{"name", typeLabel, "enter save"} {
		if !strings.Contains(view, want) {
			t.Errorf("the dense form dropped %q:\n%s", want, view)
		}
	}
	// The hint of the focused field survives, wrapped to the width; the hints of the others are what paid
	// for the space.
	if !strings.Contains(view, "a key you choose") {
		t.Errorf("the dense form dropped the hint of the focused field:\n%s", view)
	}
	if strings.Contains(view, "credential store") {
		t.Errorf("the dense form kept the hint of an unfocused field:\n%s", view)
	}

	// A terminal too small even for that shows the notice, and esc leaves it.
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 6})
	if !strings.Contains(m.View(), "Resize terminal") {
		t.Fatalf("a 20x6 terminal did not show the resize notice:\n%s", m.View())
	}
	press(t, m, "esc")
	if m.screen != screenList {
		t.Errorf("screen after esc = %v, want the list the form was opened from", m.screen)
	}
	press(t, m, "esc")
	if m.screen != screenMenu {
		t.Errorf("screen after the second esc = %v, want the menu", m.screen)
	}
	if m.quitting {
		t.Error("leaving the notice quit the editor")
	}
}

// Naming the provider of an existing credential is one choice and one save. Replacing the role rows must
// not take the type row with them: without it the entry would be written with no type at all, and the
// core would refuse the file the editor just produced.
func TestNamingTheProviderKeepsTheRestOfTheCredential(t *testing.T) {
	m, store, path := newModel(t)

	addKeyringCredential(t, m, "bookstack-personal")
	// The name of an existing entry is read-only, so the form opens on the provider row.
	editEntry(t, m, "bookstack-personal")
	selectChoice(t, m, "bookstack")
	if got := m.credentialType(); got != config.CredentialTypeKeyring {
		t.Fatalf("type after choosing the provider = %q, want the keyring it was created as", got)
	}
	pump(t, m, "enter")
	if m.fail != "" {
		t.Fatalf("saving reported %q", m.fail)
	}

	saved, err := loadTestConfig(t, path)
	if err != nil {
		t.Fatalf("the editor wrote a file its own core refuses: %v", err)
	}
	cred := saved.Credentials["bookstack-personal"]
	if cred.Provider != "bookstack" || cred.Type != config.CredentialTypeKeyring {
		t.Errorf("stored credential = %+v, want the BookStack keyring credential", cred)
	}
	if _, err := store.Load(); err != nil {
		t.Errorf("Load() = %v", err)
	}

	// Only the two BookStack roles are asked for now.
	editEntry(t, m, "bookstack-personal")
	var roles []string
	for _, f := range m.fields {
		if f.kind == fieldEnvName || f.kind == fieldSecret {
			roles = append(roles, f.label)
		}
	}
	if strings.Join(roles, ",") != "token-id,token-secret" {
		t.Errorf("roles = %v, want only the BookStack pair", roles)
	}
}
