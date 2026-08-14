// Package tui is a keyboard-driven editor for the local configuration. It is an interface on the shared
// configuration core: it holds no schema knowledge, no provider knowledge, and no validation of its own,
// and it saves only through the validating, atomic store.
//
// Secret values are never entered, held, or shown here. A credential names environment variables; the
// editor shows only whether a named variable is set.
package tui

import (
	"context"
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
)

type fieldKind int

const (
	fieldText fieldKind = iota
	fieldChoice
	fieldEnvName
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
	// so an entry is changed in place or deleted and created again.
	readOnly bool
}

// Field hints. They name the shape of an entry, never a rule the core owns: the core states the exact
// character set when it refuses a value, and repeating it here would let the two drift apart.
const (
	nameHint    = "a key you choose, without spaces; connections and --connection refer to it"
	domainHint  = "a key you choose, without spaces; commands look up this default by it"
	baseURLHint = "the root url of the instance, with scheme http or https"
	envHint     = "the NAME of an environment variable that holds the secret, never the secret itself"
)

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

	status string
	fail   string

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
// tester may be nil, in which case connection testing is unavailable.
func New(store *config.Store, tester Tester, redactor *redact.Redactor) (*Model, error) {
	cfg, err := store.Load()
	if err != nil {
		var notFound *config.NotFoundError
		if !asNotFound(err, &notFound) {
			return nil, err
		}
		cfg = config.New()
	}
	if redactor == nil {
		redactor = &redact.Redactor{}
	}
	return &Model{
		store: store, cfg: cfg, tester: tester, redactor: redactor,
		status: "Loaded " + store.Path(),
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
	if done, ok := msg.(testDoneMsg); ok {
		m.finishTest(done)
		return m, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	switch m.screen {
	case screenMenu:
		return m, m.updateMenu(key)
	case screenList:
		return m, m.updateList(key)
	case screenForm:
		return m, m.updateForm(key)
	case screenConfirm:
		return m, m.updateConfirm(key)
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
		m.openSection(section(m.cursor))
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
		m.openForm("")
	case "enter":
		if name, ok := m.selected(); ok {
			m.openForm(name)
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

func (m *Model) updateForm(key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "esc":
		// Cancelling discards the form; nothing was written.
		m.openSection(m.section)
		m.status = "Cancelled"
		return nil
	case "ctrl+c":
		return m.quit()
	case "tab", "down":
		m.moveFocus(1)
		return nil
	case "shift+tab", "up":
		m.moveFocus(-1)
		return nil
	case "enter":
		m.submit()
		return nil
	}

	current := &m.fields[m.focus]
	if current.kind == fieldChoice {
		switch key.String() {
		case "left", "h":
			current.index = wrap(current.index-1, len(current.choices))
		case "right", "l":
			current.index = wrap(current.index+1, len(current.choices))
		}
		return nil
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
		m.delete()
	case "n", "esc":
		m.screen = screenList
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
			msg.err = err.Error()
		}
		return msg
	}
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
		m.fail = m.redactor.Apply(done.err)
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

func (m *Model) openSection(s section) {
	m.stopTest()
	m.testName = ""
	m.section = s
	m.screen = screenList
	m.names = m.entryNames(s)
	m.cursor = 0
	m.clearMessages()
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
func (m *Model) openForm(name string) {
	m.editing = name
	m.screen = screenForm
	m.focus = 0
	m.clearMessages()
	m.fields = m.buildFields(name)
	m.applyFocus()
}

func (m *Model) buildFields(name string) []field {
	fields := []field{textField("name", name, name != "").withHint(nameHint)}
	if m.section == sectionDefaults {
		fields[0].label = "domain"
		fields[0].hint = domainHint
	}

	switch m.section {
	case sectionServices:
		s := m.cfg.Services[name]
		fields = append(fields,
			choiceField("provider", config.Providers(), s.Provider),
			textField("base url", s.BaseURL, false).withHint(baseURLHint),
		)
	case sectionCredentials:
		cred := m.cfg.Credentials[name]
		for _, role := range config.SecretRoles() {
			fields = append(fields, envField(role, cred.Values[role]))
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

// submit applies the form to a copy of the configuration and saves it. The editor keeps the change only
// when the store accepted it.
func (m *Model) submit() {
	m.trimFields()
	// The core owns every rule, including that a name must not be empty.
	name := m.fields[0].value()

	candidate := m.cfg.Clone()
	if err := m.apply(candidate, name); err != nil {
		m.fail = m.redactor.Apply(err.Error())
		return
	}
	if err := m.store.Save(candidate); err != nil {
		m.fail = m.redactor.Apply(err.Error())
		return
	}
	m.cfg = candidate
	m.openSection(m.section)
	m.status = "Saved " + name
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
		values := map[string]string{}
		for _, role := range config.SecretRoles() {
			if v := m.fieldValue(role); v != "" {
				values[role] = v
			}
		}
		return cfg.SetCredential(name, config.Credential{Type: config.CredentialTypeEnv, Values: values})
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

func (m *Model) delete() {
	name, ok := m.selected()
	if !ok {
		m.screen = screenList
		return
	}

	candidate := m.cfg.Clone()
	if err := m.remove(candidate, name); err != nil {
		m.fail = m.redactor.Apply(err.Error())
		m.screen = screenList
		return
	}
	if err := m.store.Save(candidate); err != nil {
		m.fail = m.redactor.Apply(err.Error())
		m.screen = screenList
		return
	}
	m.cfg = candidate
	m.openSection(m.section)
	m.status = "Deleted " + name
}

func (m *Model) fieldValue(label string) string {
	for _, f := range m.fields {
		if f.label == label {
			return f.value()
		}
	}
	return ""
}

func (m *Model) moveFocus(by int) {
	m.trimFields()
	m.focus = wrap(m.focus+by, len(m.fields))
	m.applyFocus()
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

// envField holds the NAME of an environment variable. The value is never read into the editor.
func envField(role, envName string) field {
	f := textField(role, envName, false)
	f.kind = fieldEnvName
	f.hint = envHint
	return f
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
	b.WriteString(titleStyle.Render("callbell configuration") + "\n\n")

	switch m.screen {
	case screenMenu:
		for i := section(0); i < sectionCount; i++ {
			b.WriteString(line(int(i) == m.cursor, i.title()) + "\n")
		}
		b.WriteString(hint("up/down move · enter open · q quit"))
	case screenList:
		b.WriteString(titleStyle.Render(m.section.title()) + "\n\n")
		if len(m.names) == 0 {
			b.WriteString(hintStyle.Render("nothing configured yet") + "\n")
		}
		for i, name := range m.names {
			b.WriteString(line(i == m.cursor, m.describe(name)) + "\n")
		}
		if m.section == sectionConnections {
			b.WriteString(m.testLine())
			b.WriteString(hint("n new · enter edit · d delete · t test · esc back"))
		} else {
			b.WriteString(hint("n new · enter edit · d delete · esc back"))
		}
	case screenForm:
		what := "New " + strings.ToLower(m.section.title())
		if m.editing != "" {
			what = "Edit " + m.editing
		}
		b.WriteString(titleStyle.Render(what) + "\n\n")
		for i, f := range m.fields {
			b.WriteString(line(i == m.focus, m.renderField(f, i == m.focus)) + "\n")
			if f.hint != "" {
				b.WriteString("    " + hintStyle.Render(f.hint) + "\n")
			}
		}
		b.WriteString(hint("tab move · left/right choose · enter save · esc cancel"))
	case screenConfirm:
		name, _ := m.selected()
		b.WriteString(fmt.Sprintf("Delete %q?\n", name))
		b.WriteString(hint("y delete · n keep"))
	}

	if m.fail != "" {
		b.WriteString("\n\n" + failStyle.Render("error: "+m.fail))
	} else if m.status != "" {
		b.WriteString("\n\n" + hintStyle.Render(m.status))
	}
	return b.String()
}

// testLine reports what the connection test is doing or found. It shows the stable class only.
func (m *Model) testLine() string {
	switch {
	case m.testing:
		return "\n" + hintStyle.Render(fmt.Sprintf("testing %s ... (esc cancels)", m.testName))
	case m.testClass == provider.ClassOK:
		return "\n" + activeStyle.Render(fmt.Sprintf("%s: ok", m.testName))
	case m.testClass != "":
		return "\n" + failStyle.Render(fmt.Sprintf("%s: %s", m.testName, m.testClass))
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
		cred := m.cfg.Credentials[name]
		parts := make([]string, 0, len(cred.Values))
		for _, role := range config.SecretRoles() {
			if env, ok := cred.Values[role]; ok {
				parts = append(parts, fmt.Sprintf("%s=%s %s", role, env, envState(env)))
			}
		}
		return name + "  " + strings.Join(parts, "  ")
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
	case f.kind == fieldChoice && len(f.choices) == 0:
		value = "(nothing to choose)"
	case f.kind == fieldChoice:
		value = "< " + value + " >"
	case focused && !f.readOnly:
		value = f.input.View()
		if f.kind == fieldEnvName && f.value() != "" {
			value += " " + envState(f.value())
		}
	case f.kind == fieldEnvName && value != "":
		value += " " + envState(value)
	case value == "":
		value = hintStyle.Render("(empty)")
	}
	return fmt.Sprintf("%-12s %s", f.label, value)
}

// envState reports only whether a variable is set. The value never leaves the environment.
func envState(name string) string {
	if _, ok := os.LookupEnv(name); ok {
		return "(set)"
	}
	return "(not set)"
}

func line(active bool, text string) string {
	if active {
		return activeStyle.Render("> " + text)
	}
	return "  " + text
}

func hint(text string) string { return "\n" + hintStyle.Render(text) }

// Run starts the editor on the given terminal streams.
func Run(store *config.Store, tester Tester, redactor *redact.Redactor, in *os.File, out *os.File) error {
	model, err := New(store, tester, redactor)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(model, tea.WithInput(in), tea.WithOutput(out)).Run()
	return err
}
