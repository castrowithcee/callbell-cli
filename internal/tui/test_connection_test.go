package tui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/provider"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// newTestableModel builds an editor with one configured connection and an injected tester.
func newTestableModel(t *testing.T, tester Tester, red *redact.Redactor) *Model {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "callbell")
	store := config.NewStore(filepath.Join(dir, "config.yaml"))
	secrets, _ := newResolver(t, dir, nil)

	m, err := New(store, tester, secrets, red)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	addService(t, m, "wiki", "https://wiki.example.invalid")
	addCredential(t, m, "reader", "WIKI_ID", "WIKI_SECRET")
	addConnection(t, m, "wiki", "wiki", "reader")
	openSectionByName(t, m, sectionConnections)
	return m
}

// runTest presses the test key and delivers the resulting command's message, the way the event loop does.
func runTest(t *testing.T, m *Model) {
	t.Helper()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if cmd == nil {
		t.Fatal("pressing t produced no command")
	}
	m.Update(cmd())
}

// Every stable class reaches the editor unchanged.
func TestConnectionTestClasses(t *testing.T) {
	classes := map[provider.Class]string{
		provider.ClassOK:            "accepted the connection",
		provider.ClassUnreachable:   "check the base URL and network",
		provider.ClassTLS:           "check the server certificate and URL",
		provider.ClassAuth:          "rejected the token or its user lacks permission",
		provider.ClassRateLimited:   "wait and try again",
		provider.ClassProviderError: "check the root URL and API access",
	}

	for want, explanation := range classes {
		t.Run(string(want), func(t *testing.T) {
			m := newTestableModel(t, func(context.Context, string) (provider.Class, error) {
				return want, nil
			}, nil)

			runTest(t, m)

			if m.testing {
				t.Error("the editor still reports a running test")
			}
			if m.testClass != want {
				t.Errorf("class = %q, want %q", m.testClass, want)
			}
			view := m.View()
			words := strings.Join(strings.Fields(view), " ")
			if !strings.Contains(view, string(want)) {
				t.Errorf("view = %q, want it to show %q", view, want)
			}
			if !strings.Contains(view, "wiki") {
				t.Errorf("view = %q, want it to name the connection", view)
			}
			if !strings.Contains(words, explanation) {
				t.Errorf("view = %q, want it to explain %q", view, explanation)
			}
		})
	}
}

// The tester receives the connection the cursor is on.
func TestConnectionTestUsesTheSelectedConnection(t *testing.T) {
	var got string
	m := newTestableModel(t, func(_ context.Context, name string) (provider.Class, error) {
		got = name
		return provider.ClassOK, nil
	}, nil)
	addConnection(t, m, "wiki-audit", "wiki", "reader")
	openSectionByName(t, m, sectionConnections)
	press(t, m, "down")

	runTest(t, m)

	if got != "wiki-audit" {
		t.Errorf("tested %q, want wiki-audit", got)
	}
}

// A slow test does not block the editor: keys keep working while it runs.
func TestConnectionTestDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	m := newTestableModel(t, func(ctx context.Context, _ string) (provider.Class, error) {
		select {
		case <-release:
			return provider.ClassOK, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}, nil)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if cmd == nil {
		t.Fatal("pressing t produced no command")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	if !m.testing {
		t.Fatal("the editor does not report a running test")
	}
	if !strings.Contains(m.View(), "testing wiki") {
		t.Errorf("view = %q, want the running state", m.View())
	}

	// The event loop stays responsive while the test is in flight.
	press(t, m, "down", "up")
	openSectionByName(t, m, sectionServices)
	if m.screen != screenList || m.section != sectionServices {
		t.Errorf("the editor stopped reacting: screen %v section %v", m.screen, m.section)
	}

	close(release)
	select {
	case msg := <-done:
		m.Update(msg)
	case <-time.After(2 * time.Second):
		t.Fatal("the test never finished")
	}
}

// Cancelling returns to the editor, keeps the configuration, and ignores the late result.
func TestConnectionTestCancel(t *testing.T) {
	release := make(chan struct{})
	m := newTestableModel(t, func(ctx context.Context, _ string) (provider.Class, error) {
		select {
		case <-release:
			return provider.ClassOK, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}, nil)
	before := len(m.cfg.Connections)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	press(t, m, "esc")

	if m.testing {
		t.Error("the test is still marked as running")
	}
	if m.status != "Connection test cancelled" {
		t.Errorf("status = %q", m.status)
	}
	if m.screen != screenList {
		t.Errorf("screen = %v, want to stay in the editor", m.screen)
	}
	if len(m.cfg.Connections) != before {
		t.Error("the configuration changed while a test was cancelled")
	}

	// The abandoned result must not appear afterwards.
	close(release)
	select {
	case msg := <-done:
		m.Update(msg)
	case <-time.After(2 * time.Second):
		t.Fatal("the test never returned")
	}
	if m.testClass != "" {
		t.Errorf("class = %q, want the cancelled result to be ignored", m.testClass)
	}
}

// An unexpected provider error is shown redacted, never raw.
func TestConnectionTestRedactsUnexpectedErrors(t *testing.T) {
	const canary = "s3cr3t-canary-9f3a1c"
	red := &redact.Redactor{}
	red.Add(canary)

	m := newTestableModel(t, func(context.Context, string) (provider.Class, error) {
		return "", errors.New("provider rejected Authorization: Token id:" + canary)
	}, red)

	runTest(t, m)

	if strings.Contains(m.fail, canary) {
		t.Errorf("the model holds the secret: %q", m.fail)
	}
	if !strings.Contains(m.fail, redact.Marker) {
		t.Errorf("error = %q, want the redaction marker", m.fail)
	}
	if strings.Contains(m.View(), canary) {
		t.Errorf("the screen shows the secret:\n%s", m.View())
	}
	if m.testClass != "" {
		t.Errorf("class = %q, want none after a failure", m.testClass)
	}
}

func TestConnectionTestExplainsAMissingKeyringSecretInEditorTerms(t *testing.T) {
	m := newTestableModel(t, func(context.Context, string) (provider.Class, error) {
		return "", &secret.MissingSecretError{
			Credential: "wiki-reader",
			Role:       "token-secret",
			Type:       config.CredentialTypeKeyring,
		}
	}, nil)

	runTest(t, m)

	view := m.View()
	words := strings.Join(strings.Fields(view), " ")
	for _, want := range []string{
		"Connection test could not run",
		"open Credentials",
		"select token-secret",
		"press s",
		"or p",
	} {
		if !strings.Contains(words, want) {
			t.Errorf("missing-secret result does not contain %q:\n%s", want, view)
		}
	}
}

// Testing is only offered where it makes sense, and never crashes without a tester or an entry.
func TestConnectionTestGuards(t *testing.T) {
	t.Run("other sections ignore the key", func(t *testing.T) {
		m := newTestableModel(t, func(context.Context, string) (provider.Class, error) {
			t.Error("the tester should not run here")
			return provider.ClassOK, nil
		}, nil)
		openSectionByName(t, m, sectionServices)

		if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")}); cmd != nil {
			t.Error("a command was produced outside the connections section")
		}
	})

	t.Run("an empty list produces no command", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "callbell")
		store := config.NewStore(filepath.Join(dir, "config.yaml"))
		secrets, _ := newResolver(t, dir, nil)
		m, err := New(store, func(context.Context, string) (provider.Class, error) {
			return provider.ClassOK, nil
		}, secrets, nil)
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		openSectionByName(t, m, sectionConnections)

		if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")}); cmd != nil {
			t.Error("a command was produced for an empty list")
		}
	})

	t.Run("without a tester the editor says so", func(t *testing.T) {
		m := newTestableModel(t, nil, nil)

		if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")}); cmd != nil {
			t.Error("a command was produced without a tester")
		}
		if !strings.Contains(m.fail, "unavailable") {
			t.Errorf("error = %q", m.fail)
		}
	})
}

// Quitting cancels a running test, so its context never outlives the editor.
func TestQuitCancelsARunningTest(t *testing.T) {
	started := make(chan context.Context, 1)
	m := newTestableModel(t, func(ctx context.Context, _ string) (provider.Class, error) {
		started <- ctx
		<-ctx.Done()
		return "", ctx.Err()
	}, nil)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	go cmd()

	var ctx context.Context
	select {
	case ctx = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the tester never started")
	}
	if ctx.Err() != nil {
		t.Fatal("the context was already cancelled")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Error("quitting left the test context open")
	}
	if m.testing {
		t.Error("the editor still reports a running test")
	}
}

// Leaving the connections section clears the result rather than carrying it around.
func TestConnectionTestResultDoesNotLeakBetweenScreens(t *testing.T) {
	m := newTestableModel(t, func(context.Context, string) (provider.Class, error) {
		return provider.ClassAuth, nil
	}, nil)
	runTest(t, m)

	openSectionByName(t, m, sectionServices)

	if m.testClass != "" || strings.Contains(m.View(), string(provider.ClassAuth)) {
		t.Errorf("the result survived the section change: %q\n%s", m.testClass, m.View())
	}
}
