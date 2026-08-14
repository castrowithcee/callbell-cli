package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// newStoreModel builds an editor whose configuration, credential store and plaintext fallback all live in
// the test's own directory, over exactly the environment the test names.
func newStoreModel(t *testing.T) (*Model, *config.Store, string, *secret.Resolver, *secret.MemoryStore) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "callbell")
	path := filepath.Join(dir, "config.yaml")
	store := config.NewStore(path)

	secrets, mem := newResolver(t, dir, nil)
	m, err := New(store, nil, secrets, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return m, store, path, secrets, mem
}

// focusRole moves the form focus onto the row of one secret role.
func focusRole(t *testing.T, m *Model, role string) {
	t.Helper()
	for i := 0; i <= len(m.fields); i++ {
		if m.fields[m.focus].label == role {
			return
		}
		press(t, m, "tab")
	}
	t.Fatalf("the form has no row for role %q", role)
}

// setSecret types a secret into the masked prompt of one role, the way a person does.
func setSecret(t *testing.T, m *Model, role, value string, plaintext bool) {
	t.Helper()
	focusRole(t, m, role)
	key := "s"
	if plaintext {
		key = "p"
	}
	press(t, m, key)
	if plaintext {
		if m.screen != screenPlaintextConfirm {
			t.Fatalf("p did not open the plaintext confirmation: screen %v, error %q", m.screen, m.fail)
		}
		press(t, m, "y")
	}
	if m.screen != screenSecret {
		t.Fatalf("%q did not open the prompt: screen %v, error %q", key, m.screen, m.fail)
	}
	typeText(t, m, value)
	pump(t, m, "enter")
}

// editEntry opens the form of the named entry in the current section.
func editEntry(t *testing.T, m *Model, name string) {
	t.Helper()
	openSectionByName(t, m, sectionCredentials)
	for i := 0; i <= len(m.names); i++ {
		if got, _ := m.selected(); got == name {
			pump(t, m, "enter")
			return
		}
		press(t, m, "down")
	}
	t.Fatalf("the list has no entry %q", name)
}

// The whole point of the task, through key events alone: a person sets up a BookStack connection without
// leaving the editor, and nothing of the secret ends up in the configuration.
func TestKeyringSetupHappensEntirelyInTheEditor(t *testing.T) {
	const (
		canaryTokenID     = "canary-store-token-id-73f1"
		canaryTokenSecret = "canary-store-token-secret-a02c"
	)

	m, store, path, secrets, _ := newStoreModel(t)
	var rendered strings.Builder

	addService(t, m, "wiki", "https://wiki.example.invalid")
	addKeyringCredential(t, m, "reader")

	editEntry(t, m, "reader")
	setSecret(t, m, "token-id", canaryTokenID, false)
	setSecret(t, m, "token-secret", canaryTokenSecret, false)
	rendered.WriteString(m.View())
	if m.fail != "" {
		t.Fatalf("storing reported %q", m.fail)
	}

	press(t, m, "esc")
	addConnection(t, m, "wiki", "wiki", "reader")

	openSectionByName(t, m, sectionDefaults)
	press(t, m, "n")
	typeText(t, m, "knowledge")
	press(t, m, "tab")
	selectChoice(t, m, "wiki")
	pump(t, m, "enter")
	if m.fail != "" {
		t.Fatalf("the editor reported %q", m.fail)
	}

	// The configuration loads through the ordinary loader and carries no secret.
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	cred := saved.Credentials["reader"]
	if cred.Type != config.CredentialTypeKeyring {
		t.Errorf("credential type = %q, want keyring", cred.Type)
	}
	if len(cred.Values) != 0 {
		t.Errorf("a keyring credential carries values: %v", cred.Values)
	}
	if saved.Defaults.Connections["knowledge"] != "wiki" {
		t.Errorf("default = %q", saved.Defaults.Connections["knowledge"])
	}

	// Both roles resolve from the store the secrets were handed to.
	for _, role := range []string{"token-id", "token-secret"} {
		source, checked := secrets.Status("reader", cred, role)
		if source != secret.SourceStore {
			t.Errorf("%s resolves from %q (%v), want the credential store", role, source, checked)
		}
	}

	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Every screen the editor can show, plus the file it wrote.
	for _, s := range []section{sectionServices, sectionCredentials, sectionConnections, sectionDefaults} {
		openSectionByName(t, m, s)
		rendered.WriteString(m.View())
		press(t, m, "n")
		rendered.WriteString(m.View())
		press(t, m, "esc")
	}
	editEntry(t, m, "reader")
	rendered.WriteString(m.View())

	for _, canary := range []string{canaryTokenID, canaryTokenSecret} {
		if strings.Contains(rendered.String(), canary) {
			t.Errorf("a typed secret reached the screen:\n%s", rendered.String())
		}
		if strings.Contains(string(data), canary) {
			t.Errorf("a typed secret reached the configuration:\n%s", data)
		}
	}
	if !strings.Contains(rendered.String(), string(secret.SourceStore)) {
		t.Errorf("the editor does not say where the roles resolve from:\n%s", rendered.String())
	}
}

// B1: an existing keyring credential is shown as one, and saving it unchanged leaves it one. Before this,
// the form drew its roles as variable names, the save refused, and filling the fields to get rid of the
// refusal turned the credential into an env credential and orphaned its secrets.
func TestKeyringCredentialKeepsItsTypeOnAnUnchangedSave(t *testing.T) {
	_, store, path, secrets, _ := newStoreModel(t)

	cfg := config.New()
	mustNoError(t, cfg.SetService("wiki", config.Service{Provider: "bookstack", BaseURL: "https://wiki.example.invalid"}))
	mustNoError(t, cfg.SetCredential("reader", config.Credential{Type: config.CredentialTypeKeyring}))
	mustNoError(t, cfg.SetConnection("wiki", config.Connection{Service: "wiki", Credential: "reader"}))
	mustNoError(t, store.Save(cfg))
	mustNoError(t, secrets.Set("reader", "token-id", "canary-existing-id"))

	// The editor is started on that file, the way a user finds it.
	m, err := New(store, nil, secrets, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	openSectionByName(t, m, sectionCredentials)
	if view := m.View(); !strings.Contains(view, config.CredentialTypeKeyring) {
		t.Errorf("the list does not show the credential type:\n%s", view)
	}
	if view := m.View(); !strings.Contains(view, string(secret.SourceStore)) {
		t.Errorf("the list does not show where the roles resolve from:\n%s", view)
	}

	pump(t, m, "enter")
	if got := m.fieldValue(typeLabel); got != config.CredentialTypeKeyring {
		t.Errorf("the form shows type %q, want keyring", got)
	}
	for _, role := range config.SecretRoles() {
		f := m.fields[fieldIndex(t, m, role)]
		if f.kind != fieldSecret {
			t.Errorf("role %q is drawn as a variable name, not as a stored secret", role)
		}
	}
	if view := m.View(); strings.Contains(view, envHint) {
		t.Errorf("the keyring form offers variable names:\n%s", view)
	}

	// Saving without touching anything must go through and change nothing.
	pump(t, m, "enter")
	if m.fail != "" {
		t.Fatalf("an unchanged keyring credential does not save: %q", m.fail)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := saved.Credentials["reader"]; got.Type != config.CredentialTypeKeyring || len(got.Values) != 0 {
		t.Errorf("the credential turned into %+v", got)
	}
	if source, _ := secrets.Status("reader", saved.Credentials["reader"], "token-id"); source != secret.SourceStore {
		t.Errorf("the stored secret was orphaned: %q", source)
	}
}

func fieldIndex(t *testing.T, m *Model, label string) int {
	t.Helper()
	for i, f := range m.fields {
		if f.label == label {
			return i
		}
	}
	t.Fatalf("the form has no field %q", label)
	return 0
}

func mustNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

// B1, the other half: switching a keyring credential to env while a secret is stored would leave that
// secret behind with nothing that reads it. The editor refuses and names what to remove first.
func TestTypeChangeIsRefusedWhileASecretIsStored(t *testing.T) {
	_, store, path, secrets, _ := newStoreModel(t)

	cfg := config.New()
	mustNoError(t, cfg.SetCredential("reader", config.Credential{Type: config.CredentialTypeKeyring}))
	mustNoError(t, store.Save(cfg))
	mustNoError(t, secrets.Set("reader", "token-secret", "canary-orphan-candidate"))

	m, err := New(store, nil, secrets, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	editEntry(t, m, "reader")
	focusField(t, m, typeLabel)
	selectChoice(t, m, config.CredentialTypeEnv)
	pump(t, m, "enter")

	if !strings.Contains(m.fail, "token-secret") || !strings.Contains(m.fail, "still stored") {
		t.Fatalf("error = %q, want it to name the stored role", m.fail)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := saved.Credentials["reader"].Type; got != config.CredentialTypeKeyring {
		t.Errorf("the type changed to %q although a secret is stored", got)
	}
	if source, _ := secrets.Status("reader", saved.Credentials["reader"], "token-secret"); source != secret.SourceStore {
		t.Errorf("the stored secret is gone: %q", source)
	}

	// Removing the secret is what unblocks the change, and it happens in the editor: the type goes back to
	// keyring, where the role rows manage stored secrets again.
	focusField(t, m, typeLabel)
	selectChoice(t, m, config.CredentialTypeKeyring)
	focusRole(t, m, "token-secret")
	press(t, m, "x")
	if m.screen != screenConfirm {
		t.Fatalf("removing a stored secret is not confirmed: screen %v", m.screen)
	}
	pump(t, m, "y")
	if m.fail != "" {
		t.Fatalf("removing reported %q", m.fail)
	}
	focusField(t, m, typeLabel)
	selectChoice(t, m, config.CredentialTypeEnv)
	focusRole(t, m, "token-id")
	typeText(t, m, "WIKI_ID")
	pump(t, m, "enter")

	if m.fail != "" {
		t.Fatalf("the type change still fails: %q", m.fail)
	}
	saved, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := saved.Credentials["reader"]; got.Type != config.CredentialTypeEnv || got.Values["token-id"] != "WIKI_ID" {
		t.Errorf("credential = %+v, want an env credential naming WIKI_ID", got)
	}
}

func focusField(t *testing.T, m *Model, label string) {
	t.Helper()
	for i := 0; i <= len(m.fields); i++ {
		if m.fields[m.focus].label == label {
			return
		}
		press(t, m, "tab")
	}
	t.Fatalf("the form has no field %q", label)
}

// The typed value is masked, and it exists in no rendered screen and in no field of the model.
func TestTypedSecretIsMaskedAndLeavesTheModel(t *testing.T) {
	const canary = "canary-masked-1f4b7c"

	m, _, _, _, _ := newStoreModel(t)
	addKeyringCredential(t, m, "reader")
	editEntry(t, m, "reader")
	focusRole(t, m, "token-id")
	press(t, m, "s")
	typeText(t, m, canary)

	view := m.View()
	if strings.Contains(view, canary) {
		t.Errorf("the prompt shows the typed secret:\n%s", view)
	}
	if !strings.Contains(view, strings.Repeat("*", len(canary))) {
		t.Errorf("the prompt does not mask what was typed:\n%s", view)
	}
	if got := m.secretInput.EchoMode; got != 1 {
		t.Errorf("echo mode = %v, want the masked one", got)
	}

	pump(t, m, "enter")

	if got := m.secretInput.Value(); got != "" {
		t.Errorf("the model still holds the typed secret: %q", got)
	}
	for _, f := range m.fields {
		if strings.Contains(f.input.Value(), canary) {
			t.Errorf("a form field holds the typed secret: %q", f.input.Value())
		}
	}
	for _, s := range []string{m.status, m.fail, m.busy, m.probing, m.View()} {
		if strings.Contains(s, canary) {
			t.Errorf("the editor kept the typed secret: %q", s)
		}
	}
}

// Cancelling the prompt stores nothing and keeps nothing.
func TestCancellingThePromptStoresNothing(t *testing.T) {
	const canary = "canary-cancelled-8d31"

	m, _, _, secrets, _ := newStoreModel(t)
	addKeyringCredential(t, m, "reader")
	editEntry(t, m, "reader")
	focusRole(t, m, "token-id")
	press(t, m, "s")
	typeText(t, m, canary)
	press(t, m, "esc")

	if m.screen != screenForm {
		t.Errorf("screen = %v, want to be back in the form", m.screen)
	}
	if got := m.secretInput.Value(); got != "" {
		t.Errorf("the cancelled value is still in the model: %q", got)
	}
	source, _ := secrets.Status("reader", config.Credential{Type: config.CredentialTypeKeyring}, "token-id")
	if source != secret.SourceMissing {
		t.Errorf("the cancelled secret was stored anyway: %q", source)
	}
}

// A machine without a running secret service names the keyring as the primary fix. Plaintext remains an
// explicitly confirmed way out, never an automatic or equivalent recommendation.
func TestPlaintextIsTheNamedWayOutWithoutAStore(t *testing.T) {
	const canary = "canary-plaintext-2c9a"

	m, _, _, secrets, mem := newStoreModel(t)
	mem.Fail(secret.ErrUnavailable)

	addKeyringCredential(t, m, "reader")
	editEntry(t, m, "reader")
	setSecret(t, m, "token-id", canary, false)

	for _, want := range []string{"unlock or configure", "retry s", "deliberately accept an unencrypted file"} {
		if !strings.Contains(m.fail, want) {
			t.Fatalf("error = %q, want it to contain %q", m.fail, want)
		}
	}
	if !strings.Contains(m.fail, secret.FileName) {
		t.Errorf("error = %q, want it to name the file", m.fail)
	}
	if !strings.Contains(m.fail, secret.DerivedEnvName("reader", "token-id")) {
		t.Errorf("error = %q, want it to name the variable", m.fail)
	}
	if strings.Contains(m.View(), canary) {
		t.Errorf("the failed write shows the secret:\n%s", m.View())
	}

	// The fallback is reached by asking for it, never by falling back silently.
	setSecret(t, m, "token-id", canary, true)
	if m.fail != "" {
		t.Fatalf("storing in the plaintext file reported %q", m.fail)
	}
	source, checked := secrets.Status("reader", config.Credential{Type: config.CredentialTypeKeyring}, "token-id")
	if source != secret.SourcePlaintext {
		t.Errorf("the role resolves from %q (%v), want the plaintext file", source, checked)
	}
	if view := m.View(); !strings.Contains(view, string(secret.SourcePlaintext)) {
		t.Errorf("the form does not say the fallback delivers:\n%s", view)
	}
}

func TestPlaintextNeedsAWarningAndConfirmationBeforeInput(t *testing.T) {
	m, _, path, _, _ := newStoreModel(t)
	addKeyringCredential(t, m, "reader")
	editEntry(t, m, "reader")
	focusRole(t, m, "token-id")

	press(t, m, "p")
	if m.screen != screenPlaintextConfirm {
		t.Fatalf("screen = %v, want plaintext confirmation", m.screen)
	}
	view := strings.Join(strings.Fields(m.View()), " ")
	for _, want := range []string{
		"does not use the system keyring",
		"readable text",
		"press s",
		"cancel without writing",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("confirmation does not contain %q:\n%s", want, m.View())
		}
	}
	press(t, m, "n")
	if m.screen != screenForm || m.status != "Cancelled; nothing was written" {
		t.Errorf("cancel left screen %v with status %q", m.screen, m.status)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), secret.FileName)); !os.IsNotExist(err) {
		t.Errorf("pressing p and cancelling created a plaintext file: %v", err)
	}

	press(t, m, "p", "y")
	if m.screen != screenSecret || !m.secretPlain {
		t.Errorf("confirmed plaintext did not open its masked prompt: screen %v plaintext %v",
			m.screen, m.secretPlain)
	}
}

// Removing a stored secret clears every place that kept one, and it is confirmed first.
func TestRemovingAStoredSecret(t *testing.T) {
	m, _, _, secrets, _ := newStoreModel(t)
	addKeyringCredential(t, m, "reader")
	editEntry(t, m, "reader")
	setSecret(t, m, "token-id", "canary-removed-6a12", false)

	focusRole(t, m, "token-id")
	press(t, m, "x")
	pump(t, m, "n")
	if source, _ := secrets.Status("reader", config.Credential{Type: config.CredentialTypeKeyring}, "token-id"); source != secret.SourceStore {
		t.Fatalf("the secret was removed although the answer was no: %q", source)
	}

	press(t, m, "x")
	pump(t, m, "y")
	if m.fail != "" {
		t.Fatalf("removing reported %q", m.fail)
	}
	source, _ := secrets.Status("reader", config.Credential{Type: config.CredentialTypeKeyring}, "token-id")
	if source != secret.SourceMissing {
		t.Errorf("the secret survived the removal: %q", source)
	}
	if view := m.View(); !strings.Contains(view, string(secret.SourceMissing)) {
		t.Errorf("the form still claims a source:\n%s", view)
	}
}

// A secret cannot be stored under a name that was never saved: the entry would sit in the store with
// nothing pointing at it.
func TestSecretsNeedASavedCredential(t *testing.T) {
	m, _, _, _, _ := newStoreModel(t)
	openSectionByName(t, m, sectionCredentials)
	press(t, m, "n")
	typeText(t, m, "reader")
	focusRole(t, m, "token-id")
	press(t, m, "s")

	if m.screen == screenSecret {
		t.Fatal("the prompt opened for a credential that does not exist yet")
	}
	if !strings.Contains(m.fail, "save the credential first") {
		t.Errorf("error = %q", m.fail)
	}
}

// blockingSecrets is a credential store that answers only when the test lets it, the way a half-started
// keyring daemon behaves. Nothing here touches the store of the machine.
type blockingSecrets struct {
	writes chan struct{}
	probes chan struct{}
	guards chan struct{}
}

func newBlockingSecrets() blockingSecrets {
	return blockingSecrets{
		writes: make(chan struct{}), probes: make(chan struct{}), guards: make(chan struct{}),
	}
}

func (b blockingSecrets) Status(string, config.Credential, string) (secret.Source, []string) {
	<-b.probes
	return secret.SourceMissing, []string{"credential store (no entry)"}
}
func (b blockingSecrets) Stored(string, string) secret.Placement {
	<-b.guards
	return secret.Placement{}
}
func (b blockingSecrets) Set(string, string, string) error {
	<-b.writes
	return nil
}
func (b blockingSecrets) SetPlaintext(string, string, string) error { return nil }
func (b blockingSecrets) Delete(string, string) ([]secret.Source, error) {
	<-b.writes
	return []secret.Source{secret.SourceStore}, nil
}
func (b blockingSecrets) Lookup(string) bool      { return false }
func (b blockingSecrets) Plaintext() *secret.File { return nil }

// A store that takes its time must not freeze the editor: it has thirty seconds to answer, and the event
// loop keeps working meanwhile.
func TestSlowStoreDoesNotBlockTheEditor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "callbell")
	store := config.NewStore(filepath.Join(dir, "config.yaml"))
	slow := newBlockingSecrets()
	// Only the write is slow here; the question where a role resolves from is answered at once.
	close(slow.probes)

	m, err := New(store, nil, slow, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	// press drops the commands, so nothing waits on the store while the configuration is built.
	addService(t, m, "wiki", "https://wiki.example.invalid")
	openSectionByName(t, m, sectionCredentials)
	press(t, m, "n")
	typeText(t, m, "reader")
	press(t, m, "tab")
	selectChoice(t, m, config.CredentialTypeKeyring)
	press(t, m, "enter")
	if m.fail != "" {
		t.Fatalf("creating the credential reported %q", m.fail)
	}

	openSectionByName(t, m, sectionCredentials)
	press(t, m, "enter")
	focusRole(t, m, "token-id")
	press(t, m, "s")
	typeText(t, m, "canary-slow-4b7d")
	_, cmd := m.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("submitting the secret produced no command")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	if m.busy == "" {
		t.Error("the editor does not report the write it is waiting for")
	}
	if !strings.Contains(m.View(), "storing the secret") {
		t.Errorf("view = %q, want the running write", m.View())
	}

	// The event loop stays responsive while the store is thinking.
	press(t, m, "esc")
	openSectionByName(t, m, sectionServices)
	press(t, m, "down", "up")
	if m.screen != screenList || m.section != sectionServices {
		t.Fatalf("the editor stopped reacting: screen %v section %v", m.screen, m.section)
	}

	close(slow.writes)
	select {
	case msg := <-done:
		m.Update(msg)
	case <-time.After(2 * time.Second):
		t.Fatal("the write never finished")
	}
	if m.fail != "" {
		t.Errorf("the finished write reported %q", m.fail)
	}
	if !strings.HasPrefix(m.status, "Stored") {
		t.Errorf("status = %q, want the confirmation of the write", m.status)
	}
}

// The same holds for the question where a secret resolves from: it is asked in a command, so a store that
// does not answer leaves the editor usable instead of freezing the list it was opened from.
func TestSlowStatusQueryDoesNotBlockTheEditor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "callbell")
	store := config.NewStore(filepath.Join(dir, "config.yaml"))
	slow := newBlockingSecrets()

	m, err := New(store, nil, slow, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	openSectionByName(t, m, sectionCredentials)
	press(t, m, "n")
	typeText(t, m, "reader")
	press(t, m, "tab")
	selectChoice(t, m, config.CredentialTypeKeyring)
	press(t, m, "enter")

	// Opening the list asks the store where every keyring role resolves from.
	m.screen, m.cursor = screenMenu, int(sectionCredentials)
	_, cmd := m.Update(keyMsg("enter"))
	if cmd == nil {
		t.Fatal("opening the credentials list asked the store nothing")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	if m.probing == "" {
		t.Error("the editor does not report the query it is waiting for")
	}
	if view := m.View(); !strings.Contains(view, sourcePending) {
		t.Errorf("view = %q, want the pending rows", view)
	}

	press(t, m, "down", "up")
	openSectionByName(t, m, sectionServices)
	if m.screen != screenList || m.section != sectionServices {
		t.Fatalf("the editor stopped reacting: screen %v section %v", m.screen, m.section)
	}

	close(slow.probes)
	select {
	case msg := <-done:
		m.Update(msg)
	case <-time.After(2 * time.Second):
		t.Fatal("the query never finished")
	}
}

// B3: the messages of the cascade carry the way out at their end, so the editor must show all of it. It
// used to be cut off at the right edge of the terminal.
func TestLongMessagesStayReadable(t *testing.T) {
	m, _, _, _, mem := newStoreModel(t)
	mem.Fail(secret.ErrUnavailable)

	addKeyringCredential(t, m, "reader")
	editEntry(t, m, "reader")
	setSecret(t, m, "token-id", "canary-wrapped-9e21", false)
	if len(m.fail) <= 100 {
		t.Fatalf("the message is short enough to fit anyway: %q", m.fail)
	}
	tail := secret.DerivedEnvName("reader", "token-id")

	// Every screen that carries the message, at the widths a terminal is really used at. Narrower than
	// this the title and the label column of a form do not fit either, and neither is wrapped; that is
	// what TestWrappedTextUsesTheReportedWidth covers, and it is deliberately not claimed here.
	for _, width := range []int{30, 40, 60, 80, 100} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		// Leaving a screen clears its message, so the failure is produced again at every width.
		setSecret(t, m, "token-id", "canary-wrapped-9e21", false)
		views := map[string]string{"form": m.View()}
		press(t, m, "esc")
		views["list"] = m.View()
		press(t, m, "esc")
		views["menu"] = m.View()
		editEntry(t, m, "reader")

		for name, view := range views {
			for _, line := range strings.Split(view, "\n") {
				if lipgloss.Width(line) > width {
					t.Errorf("the %s at width %d has a line of %d cells:\n%s",
						name, width, lipgloss.Width(line), line)
				}
			}
		}
		// The end of the message is the part that says what to do, so it has to survive.
		if !strings.Contains(views["form"], tail) {
			t.Errorf("the end of the message is missing at width %d:\n%s", width, views["form"])
		}
	}
}

// The editor works without a resolver: the configuration stays editable, and only what needs a store says
// why it cannot.
func TestWithoutAResolver(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "callbell")
	store := config.NewStore(filepath.Join(dir, "config.yaml"))
	m, err := New(store, nil, nil, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	addKeyringCredential(t, m, "reader")
	editEntry(t, m, "reader")
	setSecret(t, m, "token-id", "canary-no-resolver", false)

	if !strings.Contains(m.fail, ErrNoResolver.Error()) {
		t.Errorf("error = %q, want %q", m.fail, ErrNoResolver)
	}
}

// storedCredential writes a configuration holding one keyring credential, the starting point of every
// guard test.
func storedCredential(t *testing.T, env map[string]string) (
	string, *config.Store, string, *secret.Resolver, *secret.MemoryStore,
) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "callbell")
	path := filepath.Join(dir, "config.yaml")
	store := config.NewStore(path)

	cfg := config.New()
	mustNoError(t, cfg.SetCredential("reader", config.Credential{Type: config.CredentialTypeKeyring}))
	mustNoError(t, store.Save(cfg))

	secrets, mem := newResolver(t, dir, env)
	return dir, store, path, secrets, mem
}

// attemptTypeChange switches the credential to env and saves. Both variable names are filled in, so a
// refusal can only come from the guard and never from validation finding an empty credential.
func attemptTypeChange(t *testing.T, m *Model) {
	t.Helper()
	editEntry(t, m, "reader")
	focusField(t, m, typeLabel)
	selectChoice(t, m, config.CredentialTypeEnv)
	focusRole(t, m, "token-id")
	typeText(t, m, "WIKI_ID")
	focusRole(t, m, "token-secret")
	typeText(t, m, "WIKI_SECRET")
	pump(t, m, "enter")
}

// D1 and D2: the guard must ask what lies somewhere, not what the cascade would deliver. Each case here
// makes the cascade report "nothing", while a secret is in fact stored or the place cannot be asked at all.
func TestTypeChangeGuardAsksWhatIsStoredNotWhatDelivers(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		arrange func(t *testing.T, dir string, secrets *secret.Resolver, mem *secret.MemoryStore) Secrets
		wantIn  []string
		wantEnv bool // the type change is expected to go through
	}{
		{
			name: "a set variable does not make the store empty",
			// This is the very variable the editor recommends as a way out, so following that advice must
			// not switch the protection off.
			env: map[string]string{secret.DerivedEnvName("reader", "token-secret"): "shadow"},
			arrange: func(t *testing.T, _ string, secrets *secret.Resolver, _ *secret.MemoryStore) Secrets {
				mustNoError(t, secrets.Set("reader", "token-secret", "canary-shadowed"))
				return secrets
			},
			wantIn: []string{"still stored", "token-secret", string(secret.SourceStore)},
		},
		{
			name: "a switched-off store is not an empty store",
			arrange: func(t *testing.T, dir string, _ *secret.Resolver, _ *secret.MemoryStore) Secrets {
				// The same state CALLBELL_CREDENTIAL_STORE=none produces.
				return secret.NewWith(nil, nil, secret.NewFile(filepath.Join(dir, secret.FileName)), nil)
			},
			wantIn: []string{"cannot tell", "switched off", string(secret.SourceStore)},
		},
		{
			name: "an unreachable store is not an empty store",
			arrange: func(_ *testing.T, _ string, secrets *secret.Resolver, mem *secret.MemoryStore) Secrets {
				mem.Fail(secret.ErrUnavailable)
				return secrets
			},
			wantIn: []string{"cannot tell", "unavailable", string(secret.SourceStore)},
		},
		{
			name: "a fallback file that cannot be read is not an empty file",
			arrange: func(t *testing.T, dir string, secrets *secret.Resolver, _ *secret.MemoryStore) Secrets {
				mustNoError(t, secrets.SetPlaintext("reader", "token-id", "canary-too-open"))
				if err := os.Chmod(filepath.Join(dir, secret.FileName), 0o644); err != nil {
					t.Fatalf("chmod: %v", err)
				}
				return secrets
			},
			wantIn: []string{"cannot tell", "chmod 600", string(secret.SourcePlaintext)},
		},
		{
			name: "what was found and what could not be asked are said together",
			arrange: func(t *testing.T, _ string, secrets *secret.Resolver, mem *secret.MemoryStore) Secrets {
				mustNoError(t, secrets.SetPlaintext("reader", "token-id", "canary-both-1a7f"))
				mem.Fail(secret.ErrUnavailable)
				return secrets
			},
			// One message, both halves: clearing the file and trying again would otherwise be the only way
			// to learn that the store could not be asked either.
			wantIn: []string{
				"still stored", string(secret.SourcePlaintext),
				"cannot tell", string(secret.SourceStore), "unavailable",
			},
		},
		{
			name: "nothing anywhere lets the change through",
			arrange: func(_ *testing.T, _ string, secrets *secret.Resolver, _ *secret.MemoryStore) Secrets {
				return secrets
			},
			wantEnv: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, store, path, secrets, mem := storedCredential(t, tt.env)
			injected := tt.arrange(t, dir, secrets, mem)

			m, err := New(store, nil, injected, nil)
			if err != nil {
				t.Fatalf("New() = %v", err)
			}
			attemptTypeChange(t, m)

			saved, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			got := saved.Credentials["reader"].Type

			if tt.wantEnv {
				if m.fail != "" {
					t.Fatalf("the change was refused although nothing is stored: %q", m.fail)
				}
				if got != config.CredentialTypeEnv {
					t.Errorf("type = %q, want env", got)
				}
				return
			}
			if m.fail == "" {
				t.Fatalf("the type change went through silently, type is now %q", got)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(m.fail, want) {
					t.Errorf("error = %q, want it to contain %q", m.fail, want)
				}
			}
			if got != config.CredentialTypeKeyring {
				t.Errorf("type = %q, want it to stay keyring", got)
			}
		})
	}
}

// scriptedSecrets holds each write until the test releases it, so two writes really overlap.
type scriptedSecrets struct {
	gate map[string]chan struct{}
	err  map[string]error
}

func (s scriptedSecrets) Status(string, config.Credential, string) (secret.Source, []string) {
	return secret.SourceMissing, []string{"credential store (no entry)"}
}
func (s scriptedSecrets) Stored(string, string) secret.Placement { return secret.Placement{} }
func (s scriptedSecrets) Set(_, role, _ string) error {
	<-s.gate[role]
	return s.err[role]
}
func (s scriptedSecrets) SetPlaintext(_, role, _ string) error {
	<-s.gate[role]
	return s.err[role]
}
func (s scriptedSecrets) Delete(string, string) ([]secret.Source, error) {
	return nil, secret.ErrNoEntry
}
func (s scriptedSecrets) Lookup(string) bool      { return false }
func (s scriptedSecrets) Plaintext() *secret.File { return nil }

// D3: a second write must not swallow the outcome of the first. The two are really in flight together here,
// and the one that finishes second is the one that failed.
func TestEveryWriteOutcomeReachesTheUser(t *testing.T) {
	scripted := scriptedSecrets{
		gate: map[string]chan struct{}{
			"token-id":     make(chan struct{}),
			"token-secret": make(chan struct{}),
		},
		err: map[string]error{"token-id": secret.ErrUnavailable},
	}

	store := config.NewStore(filepath.Join(t.TempDir(), "callbell", "config.yaml"))
	m, err := New(store, nil, scripted, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	addKeyringCredential(t, m, "reader")
	editEntry(t, m, "reader")

	start := func(role string) chan tea.Msg {
		t.Helper()
		focusRole(t, m, role)
		press(t, m, "s")
		typeText(t, m, "canary-"+role)
		_, cmd := m.Update(keyMsg("enter"))
		if cmd == nil {
			t.Fatalf("storing %s produced no command", role)
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		return done
	}

	failing := start("token-id")
	succeeding := start("token-secret")
	if m.writes != 2 {
		t.Fatalf("writes in flight = %d, want 2", m.writes)
	}

	deliver := func(done chan tea.Msg) {
		t.Helper()
		select {
		case msg := <-done:
			m.Update(msg)
		case <-time.After(2 * time.Second):
			t.Fatal("a write never finished")
		}
	}

	// The successful one comes back first, the failing one second.
	close(scripted.gate["token-secret"])
	deliver(succeeding)
	if !strings.HasPrefix(m.status, "Stored") {
		t.Errorf("status = %q, want the confirmation of the finished write", m.status)
	}

	close(scripted.gate["token-id"])
	deliver(failing)

	if m.fail == "" {
		t.Fatalf("the failed write was swallowed, the editor shows %q", m.status)
	}
	if !strings.Contains(m.fail, secret.DerivedEnvName("reader", "token-id")) {
		t.Errorf("error = %q, want it to be about token-id", m.fail)
	}
	if m.busy != "" {
		t.Errorf("busy = %q, want it cleared once both writes are done", m.busy)
	}
}

// D4: the answer a waiting type change needs must survive an ordinary display refresh. An enter that
// neither saves nor complains is the worst outcome of all.
func TestAWaitingTypeChangeSurvivesADisplayRefresh(t *testing.T) {
	slow := newBlockingSecrets()
	close(slow.probes) // only the guard is slow here
	close(slow.writes)

	dir := filepath.Join(t.TempDir(), "callbell")
	path := filepath.Join(dir, "config.yaml")
	store := config.NewStore(path)
	cfg := config.New()
	mustNoError(t, cfg.SetCredential("reader", config.Credential{Type: config.CredentialTypeKeyring}))
	mustNoError(t, store.Save(cfg))

	m, err := New(store, nil, slow, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	editEntry(t, m, "reader")
	focusField(t, m, typeLabel)
	selectChoice(t, m, config.CredentialTypeEnv)
	focusRole(t, m, "token-id")
	typeText(t, m, "WIKI_ID")
	_, guard := m.Update(keyMsg("enter"))
	if guard == nil {
		t.Fatal("the type change asked nothing")
	}
	answered := make(chan tea.Msg, 1)
	go func() { answered <- guard() }()

	// A write that was already on its way finishes and refreshes the rows, which is exactly what used to
	// discard the answer the save is waiting for.
	m.Update(writtenMsg{credential: "reader", role: "token-secret", done: "Stored reader.token-secret"})

	close(slow.guards)
	select {
	case msg := <-answered:
		m.Update(msg)
	case <-time.After(2 * time.Second):
		t.Fatal("the guard never answered")
	}

	if m.fail != "" {
		t.Fatalf("the save was refused: %q", m.fail)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := saved.Credentials["reader"].Type; got != config.CredentialTypeEnv {
		t.Errorf("type = %q, want the waiting save to have happened", got)
	}
}

// A2: after a write or delete that failed, the rows are asked again. That is when a stale source is most
// misleading, because a delete may have cleared one place and left another.
func TestRowsAreAskedAgainAfterAFailedWrite(t *testing.T) {
	m, _, _, _, mem := newStoreModel(t)
	addKeyringCredential(t, m, "reader")
	editEntry(t, m, "reader")
	setSecret(t, m, "token-id", "canary-a2-4c81", true)

	key := secret.StoreKey("reader", "token-id")
	if got := m.sources[key]; got != secret.SourcePlaintext {
		t.Fatalf("source = %q, want the plaintext file", got)
	}

	// The store stops answering, so the delete can clear the file but not the store.
	mem.Fail(secret.ErrUnavailable)
	focusRole(t, m, "token-id")
	press(t, m, "x")
	pump(t, m, "y")

	if m.fail == "" {
		t.Fatal("a delete that could not clear every place reported success")
	}
	if got := m.sources[key]; got == secret.SourcePlaintext {
		t.Errorf("source = %q, want the row to be asked again after the failure", got)
	}
}

// A6: the text the editor wraps uses the width the terminal reported instead of falling back to the assumed
// 80 as soon as the prefixes no longer fit.
//
// This is a claim about the wrapping helpers and about the texts measured here, and that is the whole claim.
// Two things are outside it. The title and the label column of a form row are drawn at a fixed width and are
// not wrapped, so a whole view at these widths does overflow; where a whole view holds is measured in
// TestLongMessagesStayReadable. And below six columns the helpers themselves can draw one cell too many,
// because the wrapping underneath them keeps a word together rather than splitting it. Neither is worth
// chasing: a terminal that narrow shows nothing usable anyway.
func TestWrappedTextUsesTheReportedWidth(t *testing.T) {
	m, _, _, _, _ := newStoreModel(t)
	const long = "a message whose actionable part sits at its very end: export CALLBELL_READER_TOKEN_ID"

	for _, width := range []int{1, 2, 3, 4, 5, 8, 20} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		parts := map[string]string{
			"wrapped":  m.wrapped(hintStyle, long),
			"indented": m.indented(long),
			"row":      m.row(true, long),
		}
		for name, text := range parts {
			for _, l := range strings.Split(text, "\n") {
				if lipgloss.Width(l) > width {
					t.Errorf("%s at width %d drew %d cells: %q", name, width, lipgloss.Width(l), l)
				}
			}
		}
	}
}

// switchOffFallback turns the plaintext fallback off while leaving its entries on disk, the state its own
// header suggests to whoever wants it to stop delivering.
func switchOffFallback(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, secret.FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	off := strings.Replace(string(data), "allow_plaintext: true", "allow_plaintext: false", 1)
	if err := os.WriteFile(path, []byte(off), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The way out the guard names has to lead out. A secret it reports as stored must be removable with the
// very key it points at, and the type change must go through afterwards.
func TestTheGuardsWayOutReallyLeadsOut(t *testing.T) {
	dir, store, path, secrets, _ := storedCredential(t, nil)
	mustNoError(t, secrets.SetPlaintext("reader", "token-id", "canary-inert-9f2c"))
	// A fallback that no longer delivers still holds its entry, and that is what the guard reports.
	switchOffFallback(t, dir)

	m, err := New(store, nil, secrets, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	attemptTypeChange(t, m)
	if !strings.Contains(m.fail, "still stored") {
		t.Fatalf("error = %q, want the stored secret reported", m.fail)
	}

	// Follow the message: back to keyring, remove the secret on the role it names.
	focusField(t, m, typeLabel)
	selectChoice(t, m, config.CredentialTypeKeyring)
	focusRole(t, m, "token-id")
	press(t, m, "x")
	pump(t, m, "y")
	if m.fail != "" {
		t.Fatalf("the way out failed: %q", m.fail)
	}
	if holds, err := secrets.Plaintext().Holds("reader", "token-id"); err != nil || holds {
		t.Fatalf("Holds() = %v, %v; want the secret really gone", holds, err)
	}

	// And now the change the guard was blocking goes through.
	attemptTypeChange(t, m)
	if m.fail != "" {
		t.Fatalf("the change is still refused: %q", m.fail)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := saved.Credentials["reader"].Type; got != config.CredentialTypeEnv {
		t.Errorf("type = %q, want env once nothing is stored", got)
	}
}

// A-N4: after ctrl+c nothing is written any more, not even by an answer that was already on its way.
func TestQuittingDropsAWaitingTypeChange(t *testing.T) {
	slow := newBlockingSecrets()
	close(slow.probes)
	close(slow.writes)

	dir := filepath.Join(t.TempDir(), "callbell")
	path := filepath.Join(dir, "config.yaml")
	store := config.NewStore(path)
	cfg := config.New()
	mustNoError(t, cfg.SetCredential("reader", config.Credential{Type: config.CredentialTypeKeyring}))
	mustNoError(t, store.Save(cfg))

	m, err := New(store, nil, slow, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	editEntry(t, m, "reader")
	focusField(t, m, typeLabel)
	selectChoice(t, m, config.CredentialTypeEnv)
	focusRole(t, m, "token-id")
	typeText(t, m, "WIKI_ID")
	_, guard := m.Update(keyMsg("enter"))
	if guard == nil {
		t.Fatal("the type change asked nothing")
	}
	answered := make(chan tea.Msg, 1)
	go func() { answered <- guard() }()

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.quitting {
		t.Fatal("ctrl+c did not end the editor")
	}

	close(slow.guards)
	select {
	case msg := <-answered:
		m.Update(msg)
	case <-time.After(2 * time.Second):
		t.Fatal("the guard never answered")
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := saved.Credentials["reader"].Type; got != config.CredentialTypeKeyring {
		t.Errorf("type = %q, want nothing written after ctrl+c", got)
	}
}
