package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// Canary values prove that a secret in the environment never reaches the editor or the file.
const (
	canaryID     = "canary-token-id-4f21"
	canarySecret = "canary-token-secret-9ab3"
)

func newModel(t *testing.T) (*Model, *config.Store, string) {
	t.Helper()
	return newEnvModel(t, nil)
}

// newEnvModel builds an editor that sees exactly the environment the test names.
func newEnvModel(t *testing.T, env map[string]string) (*Model, *config.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "callbell")
	path := filepath.Join(dir, "config.yaml")
	store := config.NewStore(path)

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
	for i := 0; i < int(s); i++ {
		press(t, m, "down")
	}
	pump(t, m, "enter")
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
	selectChoice(t, m, config.CredentialTypeEnv)
	view := m.View()
	for _, want := range []string{"the NAME of an environment variable", "never the secret"} {
		if !strings.Contains(view, want) {
			t.Errorf("the credential form does not say %q:\n%s", want, view)
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
	if view := m.View(); !strings.Contains(view, "Services") {
		t.Errorf("view = %q", view)
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
	if got := strings.Count(view, "store it in the plaintext file"); got != 1 {
		t.Errorf("the secret keys stand %d times, want once:\n%s", got, view)
	}
	// The stages differ per role, so every role keeps its own.
	if got, want := strings.Count(view, "checked:"), len(config.SecretRoles()); got != want {
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
