package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/castrowithcee/callbell-cli/internal/config"
)

// Canary values prove that a secret in the environment never reaches the editor or the file.
const (
	canaryID     = "canary-token-id-4f21"
	canarySecret = "canary-token-secret-9ab3"
)

func newModel(t *testing.T) (*Model, *config.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "callbell", "config.yaml")
	store := config.NewStore(path)

	model, err := New(store, nil, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return model, store, path
}

// press sends key events the way a terminal would.
func press(t *testing.T, m *Model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "shift+tab":
			msg = tea.KeyMsg{Type: tea.KeyShiftTab}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "left":
			msg = tea.KeyMsg{Type: tea.KeyLeft}
		case "right":
			msg = tea.KeyMsg{Type: tea.KeyRight}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m.Update(msg)
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
	for i := 0; i < int(s); i++ {
		press(t, m, "down")
	}
	press(t, m, "enter")
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

func addCredential(t *testing.T, m *Model, name string, envNames ...string) {
	t.Helper()
	openSectionByName(t, m, sectionCredentials)
	press(t, m, "n")
	typeText(t, m, name)
	for _, env := range envNames {
		press(t, m, "tab")
		typeText(t, m, env)
	}
	press(t, m, "enter")
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
	if _, err := config.Load(path); err != nil {
		t.Errorf("the saved file does not load: %v", err)
	}
}

func TestNavigation(t *testing.T) {
	m, _, _ := newModel(t)

	t.Run("the menu wraps around", func(t *testing.T) {
		m.screen, m.cursor = screenMenu, 0
		press(t, m, "up")
		if m.cursor != int(sectionCount)-1 {
			t.Errorf("cursor = %d, want %d", m.cursor, int(sectionCount)-1)
		}
		press(t, m, "down")
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
	t.Setenv("WIKI_READER_ID", canaryID)
	t.Setenv("WIKI_READER_SECRET", canarySecret)

	m, _, path := newModel(t)
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
	// The variable names and their state are what the editor shows instead.
	if !strings.Contains(rendered.String(), "WIKI_READER_ID") || !strings.Contains(rendered.String(), "(set)") {
		t.Errorf("the editor should show the variable name and its state:\n%s", rendered.String())
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
	view := m.View()
	for _, want := range []string{"the NAME of an environment variable", "never the secret itself"} {
		if !strings.Contains(view, want) {
			t.Errorf("the credential form does not say %q:\n%s", want, view)
		}
	}
	// Every role field carries the hint, not just the first one.
	if got, want := strings.Count(view, envHint), len(config.SecretRoles()); got != want {
		t.Errorf("hint appears %d times, want once per role field (%d):\n%s", got, want, view)
	}
	if !strings.Contains(view, nameHint) {
		t.Errorf("the name field has no hint:\n%s", view)
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

	openSectionByName(t, m, sectionDefaults)
	press(t, m, "n")
	if view := m.View(); !strings.Contains(view, domainHint) {
		t.Errorf("the domain field has no hint:\n%s", view)
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

// An unset variable is reported as such, again without reading any value.
func TestEnvState(t *testing.T) {
	t.Setenv("CALLBELL_TUI_PRESENT", "value")
	t.Setenv("CALLBELL_TUI_ABSENT", "")
	_ = os.Unsetenv("CALLBELL_TUI_ABSENT")

	if got := envState("CALLBELL_TUI_PRESENT"); got != "(set)" {
		t.Errorf("envState() = %q, want (set)", got)
	}
	if got := envState("CALLBELL_TUI_ABSENT"); got != "(not set)" {
		t.Errorf("envState() = %q, want (not set)", got)
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
	if view := m.View(); !strings.Contains(view, "Services") {
		t.Errorf("view = %q", view)
	}
}
