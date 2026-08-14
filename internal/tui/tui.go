// Package tui is a keyboard-driven editor for the local configuration. It is an interface on the shared
// configuration core: it holds no schema knowledge, no provider knowledge, and no validation of its own,
// and it saves only through the validating, atomic store.
//
// A secret is taken in here and handed straight to package secret, which owns the one resolution path. It
// is typed masked, it is never shown, never read back, and never written to the configuration. What the
// editor displays about a secret is where it resolves from, never what it is.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// Tester checks one configured connection and reports the stable outcome class. The editor holds no
// provider knowledge of its own; the caller supplies this function.
type Tester func(ctx context.Context, connection string) (provider.Class, error)

// testDoneMsg carries the outcome of a connection test back into the event loop.
type testDoneMsg struct {
	id    int
	class provider.Class
	err   string
}

type section int

const (
	sectionServices section = iota
	sectionCredentials
	sectionConnections
	sectionDefaults
	sectionCount
)

func (s section) title() string {
	return [...]string{"Services", "Credentials", "Connections", "Defaults"}[s]
}

type screen int

const (
	screenMenu screen = iota
	screenList
	screenForm
	screenConfirm
	screenPlaintextConfirm
	screenSecret
)

type fieldKind int

const (
	fieldText fieldKind = iota
	fieldChoice
	// fieldEnvName holds the NAME of an environment variable, for a credential of type env.
	fieldEnvName
	// fieldSecret is the row of one role of a keyring credential. It holds no value at all: it shows where
	// the role resolves from and carries the keys that store and remove the secret.
	fieldSecret
)

type field struct {
	label   string
	kind    fieldKind
	input   textinput.Model
	choices []string
	index   int
	// hint says in one line what this field expects. It stays general: every rule belongs to the
	// configuration core, and the editor must not grow schema knowledge of its own.
	hint string
	// readOnly marks the name of an existing entry. Renaming would silently break every reference to it,
	// so an entry is changed in place or deleted and created again. A read-only field takes no editing
	// focus and says that it is locked instead of describing a choice that is no longer there.
	readOnly bool
	// roleLead marks the first of the secret role rows. What those rows hold is the same for all of them,
	// so it is said once, above them, rather than repeated under each one.
	roleLead bool
}

// Field hints. They name the shape of an entry, never a rule the core owns: the core states the exact
// character set when it refuses a value, and repeating it here would let the two drift apart.
const (
	nameHint    = "a key you choose, without spaces; connections and --connection refer to it"
	domainHint  = "a key you choose, without spaces; commands look up this default by it"
	baseURLHint = "the root url of the instance, with scheme http or https and without /api"
	// envHint is said once for all role rows and speaks of all of them: they are the same kind of row, so
	// repeating the sentence under the next one would read like a fault and add nothing.
	envHint = "every role row holds the NAME of an environment variable that holds the secret, " +
		"never the secret itself"
	typeHint   = "env names one environment variable per role; keyring keeps the secrets in the credential store"
	secretHint = "s system keyring · p unencrypted file (asks first) · x remove; typing is masked"
	// lockedHint is what the name of an existing entry says about itself. It replaces the hint that
	// describes a free choice, which is the opposite of what this field does.
	lockedHint = "read-only; delete this entry and create it again to rename it"
)

// typeLabel is the field that decides what a credential is: which variables it names, or that its secrets
// live in a store. It is shown and chosen, never assumed.
const typeLabel = "type"

// defaultWidth is the width assumed until the terminal reports its own.
const defaultWidth = 80

func (f field) withHint(hint string) field {
	f.hint = hint
	return f
}

func (f field) value() string {
	if f.kind == fieldChoice {
		if f.index < 0 || f.index >= len(f.choices) {
			return ""
		}
		return f.choices[f.index]
	}
	return strings.TrimSpace(f.input.Value())
}

// Model is the whole editor state.
type Model struct {
	store *config.Store
	cfg   *config.Config

	screen  screen
	section section
	cursor  int
	names   []string

	editing string
	fields  []field
	focus   int
	// confirmRole names the role whose stored secret the confirmation removes. Empty means the
	// confirmation is about the selected entry of the list.
	confirmRole string

	status string
	fail   string
	// busy names the store write in flight and probing the question in flight, if any. The editor keeps
	// working while either runs.
	busy    string
	probing string
	// width is what the terminal reported. Messages are wrapped into it instead of being cut off.
	width int
	// configExists distinguishes a loaded file from a new in-memory configuration. The default directory
	// is deliberately created only by the first successful save, and the editor must say so instead of
	// claiming that a file which does not exist was loaded.
	configExists bool

	// Credential store state. The editor never holds a secret: it holds where each role resolves from and
	// which stages the resolver checked, both of which name no value.
	secrets Secrets
	sources map[string]secret.Source
	checked map[string][]string
	// writes counts the store writes in flight. They are counted, not generation numbered: each one has
	// its own effect, so each outcome has to reach the user.
	writes int
	// probeID guards the display refresh, guardID the answer a waiting type change needs. They are apart
	// so an ordinary refresh cannot take that answer away.
	probeID     int
	guardID     int
	secretInput textinput.Model
	secretRole  string
	secretPlain bool

	// Connection test state. Raw responses never enter the model, only the class and a redacted message.
	tester     Tester
	redactor   *redact.Redactor
	testing    bool
	testName   string
	testClass  provider.Class
	testID     int
	cancelTest context.CancelFunc

	quitting bool
}

// New builds the editor over an existing store. A missing configuration file starts an empty one. The
// tester may be nil, in which case connection testing is unavailable; secrets may be nil, in which case the
// configuration stays editable and only the operations that would reach a store report why they cannot.
func New(store *config.Store, tester Tester, secrets Secrets, redactor *redact.Redactor) (*Model, error) {
	cfg, err := store.Load()
	configExists := true
	if err != nil {
		var notFound *config.NotFoundError
		if !asNotFound(err, &notFound) {
			return nil, err
		}
		cfg = config.New()
		configExists = false
	}
	if redactor == nil {
		redactor = &redact.Redactor{}
	}
	if secrets == nil {
		secrets = noSecrets{}
	}
	return &Model{
		store: store, cfg: cfg, tester: tester, redactor: redactor,
		secrets: secrets,
		sources: map[string]secret.Source{},
		checked: map[string][]string{},
		width:   defaultWidth, configExists: configExists,
	}, nil
}

func asNotFound(err error, target **config.NotFoundError) bool {
	nf, ok := err.(*config.NotFoundError)
	if ok {
		*target = nf
	}
	return ok
}

// Init satisfies tea.Model. The cursor is static, so the editor needs no start-up command and no timer.
func (m *Model) Init() tea.Cmd { return nil }

// Update handles one event. It is the whole editor logic and needs no terminal.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case testDoneMsg:
		m.finishTest(msg)
		return m, nil
	case sourcesMsg:
		return m, m.handleSources(msg)
	case placedMsg:
		return m, m.handlePlaced(msg)
	case writtenMsg:
		return m, m.handleWritten(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyMsg:
		switch m.screen {
		case screenMenu:
			return m, m.updateMenu(msg)
		case screenList:
			return m, m.updateList(msg)
		case screenForm:
			return m, m.updateForm(msg)
		case screenConfirm:
			return m, m.updateConfirm(msg)
		case screenPlaintextConfirm:
			return m, m.updatePlaintextConfirm(msg)
		case screenSecret:
			return m, m.updateSecret(msg)
		}
	}
	return m, nil
}

func (m *Model) updateMenu(key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "q", "esc", "ctrl+c":
		return m.quit()
	case "up", "k":
		m.cursor = wrap(m.cursor-1, int(sectionCount))
	case "down", "j":
		m.cursor = wrap(m.cursor+1, int(sectionCount))
	case "enter":
		return m.openSection(section(m.cursor))
	}
	return nil
}

func (m *Model) updateList(key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "esc", "q":
		if m.testing {
			// Cancelling the test returns to the editor with the configuration untouched.
			m.stopTest()
			m.status = "Connection test cancelled"
			return nil
		}
		m.screen, m.cursor = screenMenu, int(m.section)
		m.clearMessages()
	case "ctrl+c":
		return m.quit()
	case "up", "k":
		m.cursor = wrap(m.cursor-1, len(m.names))
	case "down", "j":
		m.cursor = wrap(m.cursor+1, len(m.names))
	case "n":
		if reason := m.newEntryBlocked(); reason != "" {
			m.status = ""
			m.fail = reason
			return nil
		}
		return m.openForm("")
	case "enter":
		if name, ok := m.selected(); ok {
			return m.openForm(name)
		}
	case "d":
		if _, ok := m.selected(); ok {
			m.screen = screenConfirm
			m.clearMessages()
		}
	case "t":
		return m.startTest()
	}
	return nil
}

// newEntryBlocked keeps a form with empty choice lists from turning an obvious missing prerequisite into
// a schema error. Existing entries remain editable even if a hand-written file is inconsistent; the core
// still owns validation in that case.
func (m *Model) newEntryBlocked() string {
	switch m.section {
	case sectionConnections:
		var missing []string
		if len(m.cfg.Services) == 0 {
			missing = append(missing, "a service")
		}
		if len(m.cfg.Credentials) == 0 {
			missing = append(missing, "a credential")
		}
		if len(missing) > 0 {
			return "Create " + strings.Join(missing, " and ") +
				" before adding a connection. Press esc to return to setup."
		}
	case sectionDefaults:
		if len(m.cfg.Connections) == 0 {
			return "Create a connection before choosing a default. Press esc to return to setup."
		}
	}
	return ""
}

func (m *Model) updateForm(key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "esc":
		// Cancelling discards the form; nothing was written.
		cmd := m.openSection(m.section)
		m.status = "Cancelled"
		return cmd
	case "ctrl+c":
		return m.quit()
	case "tab", "down":
		m.moveFocus(1)
		return nil
	case "shift+tab", "up":
		m.moveFocus(-1)
		return nil
	case "enter":
		return m.submit()
	}

	current := &m.fields[m.focus]
	switch current.kind {
	case fieldChoice:
		switch key.String() {
		case "left", "h":
			current.index = wrap(current.index-1, len(current.choices))
		case "right", "l":
			current.index = wrap(current.index+1, len(current.choices))
		default:
			return nil
		}
		if current.label == typeLabel {
			return m.credentialTypeChosen()
		}
		return nil
	case fieldSecret:
		// A secret row holds nothing to type into, so its keys are free for what a secret needs.
		return m.secretRowKey(current.label, key)
	}
	if current.readOnly {
		return nil
	}
	var cmd tea.Cmd
	current.input, cmd = current.input.Update(key)
	return cmd
}

func (m *Model) updateConfirm(key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "y":
		if role := m.confirmRole; role != "" {
			m.confirmRole = ""
			m.screen = screenForm
			return m.removeSecret(m.editing, role)
		}
		return m.delete()
	case "n", "esc":
		if m.confirmRole != "" {
			m.confirmRole = ""
			m.screen = screenForm
		} else {
			m.screen = screenList
		}
		m.status = "Cancelled"
	case "ctrl+c":
		return m.quit()
	}
	return nil
}

// startTest runs the connection test for the selected connection. The event loop keeps handling keys
// while it runs, so the editor never blocks.
func (m *Model) startTest() tea.Cmd {
	if m.section != sectionConnections {
		return nil
	}
	name, ok := m.selected()
	if !ok {
		return nil
	}
	if m.tester == nil {
		m.fail = "connection testing is unavailable"
		return nil
	}

	m.stopTest()
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTest = cancel
	m.testing = true
	m.testName = name
	m.testClass = ""
	m.clearMessages()

	id := m.testID
	tester := m.tester
	return func() tea.Msg {
		class, err := tester(ctx, name)
		msg := testDoneMsg{id: id, class: class}
		if err != nil {
			msg.err = connectionTestError(err)
		}
		return msg
	}
}

// connectionTestError turns a missing credential role into an editor action. Other errors stay intact for
// redaction, but receive context when they are displayed by finishTest.
func connectionTestError(err error) string {
	var missing *secret.MissingSecretError
	if !errors.As(err, &missing) {
		return err.Error()
	}
	if missing.Type == config.CredentialTypeKeyring {
		return fmt.Sprintf("credential %q is missing %s; open Credentials, edit it, select %s, and press s "+
			"(or p when the system credential store is unavailable)",
			missing.Credential, missing.Role, missing.Role)
	}
	return fmt.Sprintf("credential %q is missing %s; open Credentials and set the environment variable "+
		"named for that role", missing.Credential, missing.Role)
}

// finishTest accepts a result only while it is still the current one, so a cancelled test cannot report
// back later.
func (m *Model) finishTest(done testDoneMsg) {
	if !m.testing || done.id != m.testID {
		return
	}
	m.testing = false
	m.testID++
	if m.cancelTest != nil {
		m.cancelTest()
		m.cancelTest = nil
	}
	if done.err != "" {
		// Redaction happens before anything reaches the model, including an unexpected provider error.
		m.fail = "Connection test could not run: " + m.redactor.Apply(done.err)
		return
	}
	m.testClass = done.class
}

// quit ends the editor. A running test is cancelled first, so its context never outlives the editor.
func (m *Model) quit() tea.Cmd {
	m.stopTest()
	m.quitting = true
	return tea.Quit
}

// stopTest abandons a running test. Its result is ignored when it arrives.
func (m *Model) stopTest() {
	if m.cancelTest != nil {
		m.cancelTest()
		m.cancelTest = nil
	}
	if m.testing {
		m.testID++
	}
	m.testing = false
	m.testClass = ""
}

func (m *Model) openSection(s section) tea.Cmd {
	m.stopTest()
	m.testName = ""
	m.section = s
	m.screen = screenList
	m.names = m.entryNames(s)
	m.cursor = 0
	m.editing = ""
	m.confirmRole = ""
	m.clearMessages()
	if s != sectionCredentials {
		return nil
	}
	// The list says where each secret resolves from, and that answer comes from the resolver rather than
	// from anything this editor remembers.
	return m.refreshSources(m.keyringQueries())
}

func (m *Model) selected() (string, bool) {
	if m.cursor < 0 || m.cursor >= len(m.names) {
		return "", false
	}
	return m.names[m.cursor], true
}

func (m *Model) entryNames(s section) []string {
	var names []string
	switch s {
	case sectionServices:
		for name := range m.cfg.Services {
			names = append(names, name)
		}
	case sectionCredentials:
		for name := range m.cfg.Credentials {
			names = append(names, name)
		}
	case sectionConnections:
		for name := range m.cfg.Connections {
			names = append(names, name)
		}
	case sectionDefaults:
		for domain := range m.cfg.Defaults.Connections {
			names = append(names, domain)
		}
	}
	sort.Strings(names)
	return names
}

// openForm builds the form for a new entry, or for the named existing one.
func (m *Model) openForm(name string) tea.Cmd {
	m.editing = name
	m.screen = screenForm
	m.clearMessages()
	m.fields = m.buildFields(name)
	m.focus = m.firstEditable()
	m.applyFocus()
	if m.section == sectionCredentials && m.credentialType() == config.CredentialTypeKeyring {
		return m.refreshSources(m.editedQuery())
	}
	return nil
}

// credentialType is the type the credential form currently shows.
func (m *Model) credentialType() string { return m.fieldValue(typeLabel) }

// credentialTypeChosen switches the role rows to the type that is now selected.
//
// The same row means a different thing under each type: under env it holds the NAME of a variable, under
// keyring it stands for an entry in the credential store. Drawing them alike is what let a keyring
// credential look like an env credential and turn into one on the next save. A variable name that was
// already typed survives a switch back, so trying out both types costs nothing.
func (m *Model) credentialTypeChosen() tea.Cmd {
	keyring := m.credentialType() == config.CredentialTypeKeyring
	for i := range m.fields {
		f := &m.fields[i]
		if f.kind != fieldEnvName && f.kind != fieldSecret {
			continue
		}
		if keyring {
			// A secret row builds its hint from what the resolver reported, so it carries none itself.
			f.kind, f.hint = fieldSecret, ""
		} else {
			f.kind, f.hint = fieldEnvName, roleHint(f.label, f.roleLead)
		}
	}
	m.applyFocus()
	if keyring {
		return m.refreshSources(m.editedQuery())
	}
	return nil
}

func (m *Model) buildFields(name string) []field {
	key := textField("name", name, name != "").withHint(nameHint)
	if m.section == sectionDefaults {
		key.label, key.hint = "domain", domainHint
	}
	if key.readOnly {
		// A locked field must not go on describing a free choice: that hint would claim the opposite of
		// what the field does. It says that it is locked, and how to get the rename it refuses.
		key.hint = lockedHint
	}
	fields := []field{key}

	switch m.section {
	case sectionServices:
		s := m.cfg.Services[name]
		fields = append(fields,
			choiceField("provider", config.Providers(), s.Provider),
			textField("base url", s.BaseURL, false).withHint(baseURLHint),
		)
	case sectionCredentials:
		cred := m.cfg.Credentials[name]
		credType := cred.Type
		if credType == "" {
			// A new credential starts as keyring: that is the type this editor can complete on its own,
			// while env needs a variable exported in a shell the editor cannot reach.
			credType = config.CredentialTypeKeyring
		}
		fields = append(fields, choiceField(typeLabel, config.CredentialTypes(), credType).withHint(typeHint))
		for i, role := range config.SecretRoles() {
			fields = append(fields, roleField(role, cred.Values[role], credType, i == 0))
		}
	case sectionConnections:
		conn := m.cfg.Connections[name]
		fields = append(fields,
			choiceField("service", m.entryNames(sectionServices), conn.Service),
			choiceField("credential", m.entryNames(sectionCredentials), conn.Credential),
			textField("target", conn.Target, false),
		)
	case sectionDefaults:
		fields = append(fields,
			choiceField("connection", m.entryNames(sectionConnections), m.cfg.Defaults.Connections[name]),
		)
	}
	return fields
}

// submit saves the form, unless the change first has to be checked against the places that keep secrets.
func (m *Model) submit() tea.Cmd {
	m.trimFields()
	if cmd := m.guardTypeChange(); cmd != nil {
		return cmd
	}
	// The core owns every rule, including that a name must not be empty.
	return m.save(m.fields[0].value())
}

// save applies the form to a copy of the configuration and saves it. The editor keeps the change only when
// the store accepted it.
func (m *Model) save(name string) tea.Cmd {
	wasNewKeyring := m.section == sectionCredentials && m.editing == "" &&
		m.credentialType() == config.CredentialTypeKeyring
	candidate := m.cfg.Clone()
	if err := m.apply(candidate, name); err != nil {
		m.fail = m.redactor.Apply(err.Error())
		return nil
	}
	if err := m.store.Save(candidate); err != nil {
		m.fail = m.redactor.Apply(err.Error())
		return nil
	}
	m.cfg = candidate
	m.configExists = true
	if wasNewKeyring {
		cmd := m.openForm(name)
		for i := range m.fields {
			if m.fields[i].kind == fieldSecret {
				m.focus = i
				break
			}
		}
		m.applyFocus()
		m.status = "Credential saved. Add the BookStack token ID and token secret below: press s on each " +
			"role, or p if the system credential store is unavailable."
		return cmd
	}
	cmd := m.openSection(m.section)
	m.status = "Saved " + name
	return cmd
}

func (m *Model) apply(cfg *config.Config, name string) error {
	switch m.section {
	case sectionServices:
		return cfg.SetService(name, config.Service{
			Provider: m.fieldValue("provider"),
			BaseURL:  m.fieldValue("base url"),
			Options:  cfg.Services[name].Options,
		})
	case sectionCredentials:
		cred := config.Credential{Type: m.credentialType()}
		// Only an env credential names anything here. A keyring credential carries no values at all, so
		// this file cannot hold a secret even by accident; the core refuses one that does.
		if cred.Type == config.CredentialTypeEnv {
			cred.Values = map[string]string{}
			for _, role := range config.SecretRoles() {
				if v := m.fieldValue(role); v != "" {
					cred.Values[role] = v
				}
			}
		}
		return cfg.SetCredential(name, cred)
	case sectionConnections:
		return cfg.SetConnection(name, config.Connection{
			Service:    m.fieldValue("service"),
			Credential: m.fieldValue("credential"),
			Target:     m.fieldValue("target"),
		})
	case sectionDefaults:
		return cfg.SetDefault(name, m.fieldValue("connection"))
	}
	return nil
}

func (m *Model) remove(cfg *config.Config, name string) error {
	switch m.section {
	case sectionServices:
		return cfg.DeleteService(name)
	case sectionCredentials:
		return cfg.DeleteCredential(name)
	case sectionConnections:
		return cfg.DeleteConnection(name)
	case sectionDefaults:
		return cfg.DeleteDefault(name)
	}
	return nil
}

func (m *Model) delete() tea.Cmd {
	name, ok := m.selected()
	if !ok {
		m.screen = screenList
		return nil
	}

	candidate := m.cfg.Clone()
	if err := m.remove(candidate, name); err != nil {
		m.fail = m.redactor.Apply(err.Error())
		m.screen = screenList
		return nil
	}
	if err := m.store.Save(candidate); err != nil {
		m.fail = m.redactor.Apply(err.Error())
		m.screen = screenList
		return nil
	}
	m.cfg = candidate
	m.configExists = true
	cmd := m.openSection(m.section)
	m.status = "Deleted " + name
	return cmd
}

func (m *Model) fieldValue(label string) string {
	for _, f := range m.fields {
		if f.label == label {
			return f.value()
		}
	}
	return ""
}

// moveFocus steps to the next field that can be edited. A read-only field is stepped over rather than
// stopped on: stopping there would show a cursor on a field that swallows every key, which is the
// contradiction this is here to remove. The field stays drawn and readable, it just cannot be entered.
//
// The search runs at most once around the form, so a form made only of read-only fields keeps the focus
// where it is instead of spinning; nothing traps the user, because esc and enter are handled before this.
func (m *Model) moveFocus(by int) {
	m.trimFields()
	next := m.focus
	for i := 0; i < len(m.fields); i++ {
		next = wrap(next+by, len(m.fields))
		if !m.fields[next].readOnly {
			break
		}
	}
	m.focus = next
	m.applyFocus()
}

// firstEditable is where a form opens. A form whose first field is locked opens on the first field that
// takes input, so the user starts where typing has an effect.
func (m *Model) firstEditable() int {
	for i := range m.fields {
		if !m.fields[i].readOnly {
			return i
		}
	}
	return 0
}

// trimFields makes the shown text the text that will be stored. value() ignores spaces at both edges, so a
// pasted " https://wiki " is saved without them; without this the form would keep showing spaces that never
// reach the file. Trimming happens when the user leaves a field or saves, never while typing.
func (m *Model) trimFields() {
	for i := range m.fields {
		f := &m.fields[i]
		if f.kind == fieldChoice {
			continue
		}
		if trimmed := strings.TrimSpace(f.input.Value()); trimmed != f.input.Value() {
			f.input.SetValue(trimmed)
		}
	}
}

func (m *Model) applyFocus() {
	for i := range m.fields {
		if i == m.focus && m.fields[i].kind != fieldChoice && !m.fields[i].readOnly {
			m.fields[i].input.Focus()
			continue
		}
		m.fields[i].input.Blur()
	}
}

func (m *Model) clearMessages() { m.status, m.fail = "", "" }

func textField(label, value string, readOnly bool) field {
	in := textinput.New()
	in.SetValue(value)
	in.Prompt = ""
	// A static cursor is drawn on every frame the editor already paints. Blinking would need the blink
	// command threaded through Init, through every focus change and through the timer messages this event
	// loop drops, and a missed tick would leave the cursor invisible again.
	in.Cursor.SetMode(cursor.CursorStatic)
	return field{label: label, kind: fieldText, input: in, readOnly: readOnly}
}

// roleField is the row of one secret role. Under a credential of type env it holds the NAME of an
// environment variable, which the editor reads and writes; under keyring it stands for an entry in the
// credential store, which the editor can fill and remove but never read. The value is never read into the
// editor in either case.
//
// lead marks the first role row, the one that carries what all of them have in common.
func roleField(role, envName, credType string, lead bool) field {
	f := textField(role, envName, false)
	f.roleLead = lead
	f.kind, f.hint = fieldEnvName, roleHint(role, lead)
	if credType == config.CredentialTypeKeyring {
		// A secret row builds its hint from what the resolver reported, so it carries none itself.
		f.kind, f.hint = fieldSecret, ""
	}
	return f
}

// roleHint says what the provider-defined role means. Only the first row adds the explanation shared by
// every env row; repeating that part under the next role would read like a fault.
func roleHint(role string, lead bool) string {
	var parts []string
	if description := config.SecretRoleDescription(role); description != "" {
		parts = append(parts, description)
	}
	if lead {
		parts = append(parts, envHint)
	}
	return strings.Join(parts, "; ")
}

func choiceField(label string, choices []string, value string) field {
	f := field{label: label, kind: fieldChoice, choices: choices}
	for i, c := range choices {
		if c == value {
			f.index = i
		}
	}
	return f
}

func wrap(i, n int) int {
	if n <= 0 {
		return 0
	}
	return ((i % n) + n) % n
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	activeStyle = lipgloss.NewStyle().Bold(true)
	hintStyle   = lipgloss.NewStyle().Faint(true)
	failStyle   = lipgloss.NewStyle().Bold(true)
)

// View renders the current screen.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("Callbell setup") + "\n")
	path := "Config: " + m.store.Path()
	if !m.configExists {
		path += " (created on first save)"
	}
	b.WriteString(m.wrapped(hintStyle, path) + "\n\n")

	switch m.screen {
	case screenMenu:
		b.WriteString(m.wrapped(activeStyle, "Next: "+m.nextStep()) + "\n\n")
		for i := section(0); i < sectionCount; i++ {
			b.WriteString(m.row(int(i) == m.cursor, m.menuLine(i)) + "\n")
		}
		b.WriteString(m.hint("up/down move · enter open · q quit"))
	case screenList:
		b.WriteString(titleStyle.Render(m.section.title()) + "\n\n")
		if len(m.names) == 0 {
			b.WriteString(m.wrapped(hintStyle, m.emptyHelp()) + "\n")
		}
		for i, name := range m.names {
			b.WriteString(m.row(i == m.cursor, m.describe(name)) + "\n")
		}
		if m.section == sectionConnections {
			b.WriteString(m.testLine())
			b.WriteString(m.hint("n new · enter edit · d delete · t test · esc back"))
		} else {
			b.WriteString(m.hint("n new · enter edit · d delete · esc back"))
		}
	case screenForm:
		what := "New " + strings.ToLower(m.section.title())
		if m.editing != "" {
			what = "Edit " + m.editing
		}
		b.WriteString(titleStyle.Render(what) + "\n\n")
		for i, f := range m.fields {
			// A field and its hint are one block, and the blank line stands between the blocks. Without it
			// the hint is as close to the next field as to its own, and an indented line under a row of
			// rows reads as the introduction to what follows rather than as the note of what precedes.
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(line(i == m.focus, m.renderField(f, i == m.focus)) + "\n")
			if hint := m.fieldHint(f); hint != "" {
				b.WriteString(m.indented(hint) + "\n")
			}
		}
		keys := "tab move · left/right choose · enter save · esc cancel"
		if m.fields[m.focus].kind == fieldSecret {
			if m.editing == "" {
				keys = "enter save credential first · tab move · esc cancel"
			} else {
				keys = "s system keyring · p unencrypted file (asks first) · x remove · " + keys
			}
		}
		b.WriteString(m.hint(keys))
	case screenPlaintextConfirm:
		b.WriteString(titleStyle.Render("Store this secret unencrypted?") + "\n\n")
		b.WriteString(m.wrapped(failStyle,
			fmt.Sprintf("This does not use the system keyring. It writes %s.%s as readable text for your "+
				"user account into %s.", m.editing, m.secretRole, m.plaintextPath())) + "\n")
		b.WriteString(m.indented(
			"Choose no if you want the keyring. Unlock or configure the system keyring, return here, and press s.") +
			"\n")
		b.WriteString(m.hint("y continue to masked input · n/esc cancel without writing"))
	case screenSecret:
		where := "the credential store of this machine"
		if m.secretPlain {
			where = "the plaintext file " + m.plaintextPath()
		}
		b.WriteString(titleStyle.Render(fmt.Sprintf("Secret for %s.%s", m.editing, m.secretRole)) + "\n\n")
		b.WriteString("  " + m.secretInput.View() + "\n")
		b.WriteString(m.indented(
			"the value is masked while you type, is never shown back, and goes into "+where) + "\n")
		b.WriteString(m.hint("enter store · esc cancel"))
	case screenConfirm:
		if m.confirmRole != "" {
			b.WriteString(fmt.Sprintf("Remove the stored secret for %s.%s?\n", m.editing, m.confirmRole))
			b.WriteString(m.indented(
				"it is removed from the credential store and from the plaintext file; an environment "+
					"variable is not touched, because it belongs to your shell") + "\n")
		} else {
			name, _ := m.selected()
			b.WriteString(fmt.Sprintf("Delete %q?\n", name))
			if m.section == sectionCredentials &&
				m.cfg.Credentials[name].Type == config.CredentialTypeKeyring {
				b.WriteString(m.indented(
					"its stored secrets are not removed with it; remove them first with x on the role, "+
						"or later with 'callbell credential delete'") + "\n")
			}
		}
		b.WriteString(m.hint("y remove · n keep"))
	}

	for _, note := range []string{m.busy, m.probing} {
		if note != "" {
			b.WriteString("\n\n" + m.wrapped(hintStyle, note+" ... (the editor stays usable)"))
		}
	}
	if m.fail != "" {
		b.WriteString("\n\n" + m.wrapped(failStyle, "error: "+m.fail))
	} else if m.status != "" {
		b.WriteString("\n\n" + m.wrapped(hintStyle, m.status))
	}
	return b.String()
}

func (m *Model) menuLine(s section) string {
	detail := [...]string{
		"BookStack server URL",
		"token ID and token secret",
		"combine a service and credential",
		"optional connection for knowledge commands",
	}[s]
	return fmt.Sprintf("%d. %s (%d) - %s", int(s)+1, s.title(), len(m.entryNames(s)), detail)
}

func (m *Model) nextStep() string {
	switch {
	case len(m.cfg.Services) == 0:
		return "add a Service with your BookStack URL"
	case len(m.cfg.Credentials) == 0:
		return "add Credentials and store both parts of the BookStack API token"
	case len(m.cfg.Connections) == 0:
		return "add a Connection that combines the service and credentials"
	case len(m.cfg.Defaults.Connections) == 0:
		return "open Connections and press t to test; Defaults are optional"
	default:
		return "open Connections and press t to test BookStack"
	}
}

func (m *Model) emptyHelp() string {
	switch m.section {
	case sectionServices:
		return "No services yet. Press n to add the root URL of your BookStack server (without /api)."
	case sectionCredentials:
		return "No credentials yet. Press n to add the BookStack token ID and token secret."
	case sectionConnections:
		if reason := m.newEntryBlocked(); reason != "" {
			return reason
		}
		return "No connections yet. Press n to combine a service and credential."
	case sectionDefaults:
		if reason := m.newEntryBlocked(); reason != "" {
			return reason + " Defaults are optional."
		}
		return "No default yet. Press n to choose one, or leave this empty and select connections explicitly."
	}
	return "Nothing configured yet."
}

// wrapped fits a message into the terminal instead of leaving it to be cut off at the right edge.
//
// The messages of the cascade carry the way out at their end: the file to chmod, the command to run, the
// variable to export. A truncated line loses exactly the part the reader needs, and it is the longest
// messages, the ones that explain the most, that get truncated. Wrapping keeps all of it on screen and
// needs nothing but the width the terminal already reports.
func (m *Model) wrapped(style lipgloss.Style, text string) string {
	return style.Width(m.usable(0)).Render(text)
}

// usable is the room left for text after a prefix of n cells.
//
// A terminal that has not reported its width yet is assumed to be the usual one; a terminal that reported a
// width narrower than the prefix still gets one cell to wrap into, rather than being drawn at the assumed
// width and spilling. Falling back to the assumed width because the real one is small would be the very
// truncation this is here to avoid.
func (m *Model) usable(prefix int) int {
	width := m.width
	if width <= 0 {
		width = defaultWidth
	}
	if width-prefix < 1 {
		return 1
	}
	return width - prefix
}

// fit returns the prefix to draw and the room left for the text beside it, so that the two together never
// exceed the terminal. A terminal narrower than the prefix keeps one cell for the text and loses part of
// the prefix: an indent that pushes text past the right edge is worse than a missing indent.
func (m *Model) fit(prefix string) (string, int) {
	if m.width > 0 {
		if room := m.width - 1; room < len(prefix) {
			prefix = prefix[:max(room, 0)]
		}
	}
	return prefix, m.usable(len(prefix))
}

// indented draws a hint under the row it belongs to and keeps its continuation lines there too, so a hint
// that needs two lines still reads as one hint rather than as a stray sentence at the left margin.
func (m *Model) indented(text string) string {
	indent, width := m.fit("    ")
	lines := strings.Split(hintStyle.Width(width).Render(text), "\n")
	for i, l := range lines {
		lines[i] = indent + l
	}
	return strings.Join(lines, "\n")
}

// fieldHint is what one form row says about itself. A secret row adds the stages the resolver checked,
// which is the actionable half of a missing secret and names no value.
func (m *Model) fieldHint(f field) string {
	if f.kind == fieldSecret {
		if m.editing == "" {
			var parts []string
			if description := config.SecretRoleDescription(f.label); description != "" {
				parts = append(parts, description)
			}
			if f.roleLead {
				parts = append(parts,
					"save this credential with enter first; you will stay here to add both secrets")
			}
			return strings.Join(parts, "; ")
		}
		return m.secretRowHint(m.editing, f.label, f.roleLead)
	}
	return f.hint
}

// testLine keeps the stable class visible and adds the next useful interpretation for a human.
func (m *Model) testLine() string {
	switch {
	case m.testing:
		return "\n" + m.wrapped(hintStyle, fmt.Sprintf("testing %s ... (esc cancels)", m.testName))
	case m.testClass == provider.ClassOK:
		return "\n" + m.wrapped(activeStyle,
			fmt.Sprintf("%s: ok - BookStack accepted the connection", m.testName))
	case m.testClass != "":
		explanation := map[provider.Class]string{
			provider.ClassUnreachable:   "the server did not answer; check the base URL and network",
			provider.ClassTLS:           "the secure connection failed; check the server certificate and URL",
			provider.ClassAuth:          "BookStack rejected the token or its user lacks permission",
			provider.ClassRateLimited:   "BookStack is rate-limiting requests; wait and try again",
			provider.ClassProviderError: "BookStack returned an unusable response; check the root URL and API access",
		}[m.testClass]
		return "\n" + m.wrapped(failStyle,
			fmt.Sprintf("%s: %s - %s", m.testName, m.testClass, explanation))
	}
	return ""
}

// describe summarises one entry for the list. It never shows a secret value.
func (m *Model) describe(name string) string {
	switch m.section {
	case sectionServices:
		s := m.cfg.Services[name]
		return fmt.Sprintf("%s  %s  %s", name, s.Provider, s.BaseURL)
	case sectionCredentials:
		// The type decides what the credential is, so it is part of the line rather than something the
		// reader has to open the form to find out.
		cred := m.cfg.Credentials[name]
		parts := make([]string, 0, len(config.SecretRoles()))
		for _, role := range config.SecretRoles() {
			switch {
			case cred.Type == config.CredentialTypeKeyring:
				parts = append(parts, fmt.Sprintf("%s (%s)", role, m.storedSource(name, role)))
			case cred.Values[role] != "":
				parts = append(parts, fmt.Sprintf("%s=%s (%s)", role, cred.Values[role],
					m.envSource(cred.Values[role])))
			}
		}
		return strings.TrimSpace(fmt.Sprintf("%s  %s  %s", name, cred.Type, strings.Join(parts, "  ")))
	case sectionConnections:
		conn := m.cfg.Connections[name]
		return fmt.Sprintf("%s  %s / %s", name, conn.Service, conn.Credential)
	case sectionDefaults:
		return fmt.Sprintf("%s  %s", name, m.cfg.Defaults.Connections[name])
	}
	return name
}

// renderField draws one form row. The focused text field is drawn by the text input itself, so the cursor
// stands where it really stands, including in the middle of a url being corrected. Every other field is
// drawn plainly from its value.
func (m *Model) renderField(f field, focused bool) string {
	value := f.value()
	switch {
	case f.kind == fieldSecret:
		// A secret row shows where the role resolves from and nothing else: there is no value to draw,
		// and the resolver would not hand one out.
		value = "(" + m.storedSource(m.editing, f.label) + ")"
	case f.kind == fieldChoice && len(f.choices) == 0:
		value = "(nothing to choose)"
	case f.kind == fieldChoice:
		value = "< " + value + " >"
	case focused && !f.readOnly:
		value = f.input.View()
		if f.kind == fieldEnvName && f.value() != "" {
			value += " (" + m.envSource(f.value()) + ")"
		}
	case f.kind == fieldEnvName && value != "":
		value += " (" + m.envSource(value) + ")"
	case value == "":
		value = hintStyle.Render("(empty)")
	}
	return fmt.Sprintf("%-12s %s", f.label, value)
}

func line(active bool, text string) string {
	if active {
		return activeStyle.Render("> " + text)
	}
	return "  " + text
}

// row draws one line of a list. A line that does not fit is continued under its own entry instead of being
// cut off at the right edge: the list is where the source of every secret role stands, and that is the part
// that falls off first.
func (m *Model) row(active bool, text string) string {
	blank, width := m.fit("  ")
	marker := blank
	if active && len(blank) == 2 {
		marker = "> "
	}
	style := lipgloss.NewStyle()
	if active {
		style = activeStyle
	}
	lines := strings.Split(style.Width(width).Render(text), "\n")
	for i, l := range lines {
		prefix := blank
		if i == 0 {
			prefix = marker
		}
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// hint is the key line of a screen. Like every other prose line it is wrapped into the terminal instead of
// being cut off at its right edge.
func (m *Model) hint(text string) string { return "\n" + m.wrapped(hintStyle, text) }

// Run starts the editor on the given terminal streams.
func Run(store *config.Store, tester Tester, secrets Secrets, redactor *redact.Redactor, in, out *os.File) error {
	model, err := New(store, tester, secrets, redactor)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(model, tea.WithInput(in), tea.WithOutput(out)).Run()
	return err
}
