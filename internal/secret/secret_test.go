package secret

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

// Every stage gets its own canary, so a test can tell not only that a secret arrived but from where.
const (
	canaryEnv       = "canary-from-env-4f21a8"
	canaryStore     = "canary-from-store-9ab307"
	canaryPlaintext = "canary-from-file-2d5c14"
)

const (
	credName = "wiki-reader"
	role     = "token-id"
)

func keyringCred() config.Credential { return config.Credential{Type: config.CredentialTypeKeyring} }

func envCred(name string) config.Credential {
	return config.Credential{Type: config.CredentialTypeEnv, Values: map[string]string{role: name}}
}

// fixture builds a resolver over an environment that only knows what the test puts in it, an in-process
// store, and a fallback file in a temporary directory. Nothing here can reach the machine's own store.
func fixture(t *testing.T, env map[string]string) (*Resolver, *MemoryStore, *File, *redact.Redactor) {
	t.Helper()
	store := NewMemoryStore()
	file := NewFile(filepath.Join(t.TempDir(), FileName))
	red := &redact.Redactor{}
	lookup := func(name string) string { return env[name] }
	return NewWith(lookup, store, file, red), store, file, red
}

// The cascade: the first stage that delivers wins, and the delivering stage is reported.
func TestCascadeOrder(t *testing.T) {
	derived := DerivedEnvName(credName, role)

	t.Run("the environment variable wins over the store and the file", func(t *testing.T) {
		r, store, file, _ := fixture(t, map[string]string{derived: canaryEnv})
		if err := store.Set(StoreKey(credName, role), canaryStore); err != nil {
			t.Fatalf("Set() = %v", err)
		}
		if err := file.Set(credName, role, canaryPlaintext); err != nil {
			t.Fatalf("Set() = %v", err)
		}

		got, err := r.Resolve(credName, keyringCred(), role)

		if err != nil {
			t.Fatalf("Resolve() = %v", err)
		}
		if got.Secret != canaryEnv || got.Source != SourceEnv {
			t.Errorf("source = %q, want %q", got.Source, SourceEnv)
		}
	})

	t.Run("the store wins over the file", func(t *testing.T) {
		r, store, file, _ := fixture(t, nil)
		if err := store.Set(StoreKey(credName, role), canaryStore); err != nil {
			t.Fatalf("Set() = %v", err)
		}
		if err := file.Set(credName, role, canaryPlaintext); err != nil {
			t.Fatalf("Set() = %v", err)
		}

		got, err := r.Resolve(credName, keyringCred(), role)

		if err != nil {
			t.Fatalf("Resolve() = %v", err)
		}
		if got.Secret != canaryStore || got.Source != SourceStore {
			t.Errorf("source = %q, want %q", got.Source, SourceStore)
		}
		if len(got.Checked) != 1 || !strings.Contains(got.Checked[0], string(SourceEnv)) {
			t.Errorf("checked = %v, want the environment variable to be reported as tried", got.Checked)
		}
	})

	t.Run("the switched-on file delivers when nothing else does", func(t *testing.T) {
		r, _, file, _ := fixture(t, nil)
		if err := file.Set(credName, role, canaryPlaintext); err != nil {
			t.Fatalf("Set() = %v", err)
		}

		got, err := r.Resolve(credName, keyringCred(), role)

		if err != nil {
			t.Fatalf("Resolve() = %v", err)
		}
		if got.Secret != canaryPlaintext || got.Source != SourcePlaintext {
			t.Errorf("source = %q, want %q", got.Source, SourcePlaintext)
		}
	})
}

// A missing credential store is not a dead end: the named fallback takes over.
func TestUnavailableStoreFallsThrough(t *testing.T) {
	r, store, file, _ := fixture(t, nil)
	store.Fail(ErrUnavailable)
	if err := file.Set(credName, role, canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	got, err := r.Resolve(credName, keyringCred(), role)

	if err != nil {
		t.Fatalf("Resolve() = %v, want the fallback to take over", err)
	}
	if got.Source != SourcePlaintext {
		t.Errorf("source = %q, want %q", got.Source, SourcePlaintext)
	}
	if len(got.Checked) != 2 || !strings.Contains(got.Checked[1], "unavailable") {
		t.Errorf("checked = %v, want the unreachable store to be reported", got.Checked)
	}
}

// The fallback file is inert until it is switched on, and resolving never creates it.
func TestPlaintextNeedsTheSwitch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	// A file that holds an entry but was never switched on. Nothing but the switch separates it from a
	// working fallback, which is the point: the fallback is a decision, not a leftover file.
	inert := "version: 1\nallow_plaintext: false\ncredentials:\n  " + credName + ":\n    " + role + ": " + canaryPlaintext + "\n"
	if err := os.WriteFile(path, []byte(inert), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	r := NewWith(func(string) string { return "" }, NewMemoryStore(), NewFile(path), &redact.Redactor{})

	_, err := r.Resolve(credName, keyringCred(), role)

	var missing *MissingSecretError
	if !errors.As(err, &missing) {
		t.Fatalf("Resolve() = %v, want a *MissingSecretError", err)
	}
	if strings.Contains(err.Error(), canaryPlaintext) {
		t.Errorf("error = %q, want it to keep the secret out", err)
	}
	if !strings.Contains(err.Error(), "plaintext file (not enabled)") {
		t.Errorf("error = %q, want the fallback reported as not enabled", err)
	}
}

// Resolving alone never writes anything, least of all a plaintext file.
func TestResolveCreatesNoFile(t *testing.T) {
	dir := t.TempDir()
	r := NewWith(func(string) string { return "" }, NewMemoryStore(), NewFile(filepath.Join(dir, FileName)), nil)

	if _, err := r.Resolve(credName, keyringCred(), role); err == nil {
		t.Fatal("Resolve() = nil, want an error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the directory holds %d files, want none", len(entries))
	}
}

// The message names the configuration key and the stages that were tried, and never the configured text:
// that text may be a pasted token.
func TestMissingSecretMessage(t *testing.T) {
	const pasted = "Pasted7Token9Value2Canary4Kx8Qm1"

	t.Run("a credential of type env", func(t *testing.T) {
		r, _, _, _ := fixture(t, nil)

		_, err := r.Resolve("reader", envCred(pasted), role)

		var missing *MissingSecretError
		if !errors.As(err, &missing) {
			t.Fatalf("Resolve() = %v, want a *MissingSecretError", err)
		}
		if !strings.Contains(err.Error(), "credentials.reader.values.token-id") {
			t.Errorf("error = %q, want it to name the configuration key", err)
		}
		if strings.Contains(err.Error(), pasted) {
			t.Errorf("error = %q, want the configured text kept out", err)
		}
		// A credential of type env is described by its variable alone, so that is the only stage.
		want := "checked: environment variable (not set)"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
		if strings.Contains(err.Error(), string(SourceStore)) || strings.Contains(err.Error(), string(SourcePlaintext)) {
			t.Errorf("error = %q, want no stage beyond the variable", err)
		}
		if !strings.Contains(err.Error(), "set the environment variable that credentials.reader.values.token-id names") {
			t.Errorf("error = %q, want the way out named", err)
		}
	})

	t.Run("a credential of type keyring", func(t *testing.T) {
		r, _, _, _ := fixture(t, nil)

		_, err := r.Resolve(credName, keyringCred(), role)

		// A keyring credential has no values section, so the message must not send the user to one.
		if strings.Contains(err.Error(), ".values.") {
			t.Errorf("error = %q, want it to name a key that exists", err)
		}
		if !strings.Contains(err.Error(), "credentials.wiki-reader, secret role token-id") {
			t.Errorf("error = %q, want it to name the credential and the role", err)
		}
		want := "checked: environment variable (not set), credential store (no entry), plaintext file (not enabled)"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
		// The way out of a keyring credential is the command and the derived variable, both of which are
		// names rather than values.
		for _, want := range []string{"callbell credential set wiki-reader token-id", "CALLBELL_WIKI_READER_TOKEN_ID"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		}
	})

	t.Run("a role the credential does not name", func(t *testing.T) {
		r, _, _, _ := fixture(t, nil)

		_, err := r.Resolve("reader", envCred("SOME_VARIABLE"), "token-secret")

		if !strings.Contains(err.Error(), "environment variable (not named)") {
			t.Errorf("error = %q, want the unnamed variable reported", err)
		}
	})
}

// The environment variable of a credential that names none is derived, deterministically and documented.
func TestDerivedEnvName(t *testing.T) {
	tests := []struct {
		credential string
		role       string
		want       string
	}{
		{"wiki-reader", "token-id", "CALLBELL_WIKI_READER_TOKEN_ID"},
		{"wiki.audit", "token-secret", "CALLBELL_WIKI_AUDIT_TOKEN_SECRET"},
		{"Wiki_2", "token", "CALLBELL_WIKI_2_TOKEN"},
		// A name may start with a digit; the fixed prefix keeps the result a legal variable name.
		{"7wiki", "token", "CALLBELL_7WIKI_TOKEN"},
	}

	for _, tt := range tests {
		if got := DerivedEnvName(tt.credential, tt.role); got != tt.want {
			t.Errorf("DerivedEnvName(%q, %q) = %q, want %q", tt.credential, tt.role, got, tt.want)
		}
	}
}

// A credential of type env keeps naming its own variable, unchanged, and the derived name plays no part.
func TestEnvCredentialUsesItsOwnVariable(t *testing.T) {
	r, _, _, _ := fixture(t, map[string]string{
		"WIKI_TOKEN_ID":                      canaryEnv,
		DerivedEnvName("reader", "token-id"): "must-not-be-used",
	})

	got, err := r.Resolve("reader", envCred("WIKI_TOKEN_ID"), role)

	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if got.Secret != canaryEnv || got.Source != SourceEnv {
		t.Errorf("resolved %q from %q, want the named variable", got.Secret, got.Source)
	}
}

// Whatever the cascade delivers is known to the redactor before it leaves the resolver.
func TestDeliveredValuesAreRedacted(t *testing.T) {
	for _, tt := range []struct {
		name   string
		canary string
		setup  func(*Resolver, *MemoryStore, *File)
		env    map[string]string
	}{
		{"environment variable", canaryEnv, nil, map[string]string{DerivedEnvName(credName, role): canaryEnv}},
		{"credential store", canaryStore, func(_ *Resolver, s *MemoryStore, _ *File) {
			_ = s.Set(StoreKey(credName, role), canaryStore)
		}, nil},
		{"plaintext file", canaryPlaintext, func(_ *Resolver, _ *MemoryStore, f *File) {
			_ = f.Set(credName, role, canaryPlaintext)
		}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, store, file, red := fixture(t, tt.env)
			if tt.setup != nil {
				tt.setup(r, store, file)
			}

			if _, err := r.Resolve(credName, keyringCred(), role); err != nil {
				t.Fatalf("Resolve() = %v", err)
			}

			if got := red.Apply("token is " + tt.canary); strings.Contains(got, tt.canary) {
				t.Errorf("the redactor does not know the value: %q", got)
			}
		})
	}
}

// Status answers where a secret comes from without handing the value to the caller.
func TestStatus(t *testing.T) {
	r, store, _, _ := fixture(t, nil)
	if err := store.Set(StoreKey(credName, role), canaryStore); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	source, checked := r.Status(credName, keyringCred(), role)

	if source != SourceStore {
		t.Errorf("source = %q, want %q", source, SourceStore)
	}
	if len(checked) != 1 {
		t.Errorf("checked = %v, want the one stage that was tried first", checked)
	}

	missingSource, missingChecked := r.Status(credName, keyringCred(), "token-secret")
	if missingSource != SourceMissing || len(missingChecked) != 3 {
		t.Errorf("status = %q %v, want every stage reported as tried", missingSource, missingChecked)
	}
}

// Writing goes to the store; only SetPlaintext ever touches the file.
func TestSetAndDelete(t *testing.T) {
	r, store, file, red := fixture(t, nil)

	if err := r.Set(credName, role, canaryStore); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if got, err := store.Get(StoreKey(credName, role)); err != nil || got != canaryStore {
		t.Fatalf("store holds %q (%v), want the value", got, err)
	}
	if _, err := os.Stat(file.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the plaintext file exists after a plain Set: %v", err)
	}
	if got := red.Apply(canaryStore); strings.Contains(got, canaryStore) {
		t.Errorf("the redactor does not know a stored value: %q", got)
	}

	cleared, err := r.Delete(credName, role)
	if err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if len(cleared) != 1 || cleared[0] != SourceStore {
		t.Errorf("cleared = %v, want the credential store", cleared)
	}
	if _, err := r.Delete(credName, role); !errors.Is(err, ErrNoEntry) {
		t.Errorf("Delete() = %v, want ErrNoEntry", err)
	}
}

// A resolver without a store still resolves, because a platform without one must not be a dead end.
func TestResolverWithoutStore(t *testing.T) {
	env := map[string]string{DerivedEnvName(credName, role): canaryEnv}
	r := NewWith(func(name string) string { return env[name] }, nil, nil, nil)

	got, err := r.Resolve(credName, keyringCred(), role)

	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if got.Source != SourceEnv {
		t.Errorf("source = %q, want %q", got.Source, SourceEnv)
	}
	if _, err := r.Resolve(credName, keyringCred(), "unset"); err == nil {
		t.Error("Resolve() = nil, want an error when nothing delivers")
	}
}

// A credential of type env is described by the variable it names, and by nothing else. A store entry or a
// switched-on plaintext file under the same credential name must not stand in for a variable that a CI run
// did not set: that would turn a missing secret into a successful authentication with a developer's own
// identity.
func TestEnvCredentialNeverFallsThrough(t *testing.T) {
	r, store, file, _ := fixture(t, nil)
	if err := store.Set(StoreKey("reader", role), canaryStore); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if err := file.Set("reader", role, canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	value, err := r.Resolve("reader", envCred("WIKI_TOKEN_ID"), role)

	var missing *MissingSecretError
	if !errors.As(err, &missing) {
		t.Fatalf("Resolve() = %+v, %v; want a *MissingSecretError", value, err)
	}
	if value.Secret != "" {
		t.Errorf("a secret was delivered from %q", value.Source)
	}
	if len(missing.Checked) != 1 || !strings.Contains(missing.Checked[0], string(SourceEnv)) {
		t.Errorf("checked = %v, want only the environment variable", missing.Checked)
	}
	for _, canary := range []string{canaryStore, canaryPlaintext} {
		if strings.Contains(err.Error(), canary) {
			t.Errorf("error = %q, want it to carry no value", err)
		}
	}

	// The same store entry and the same file do deliver for a keyring credential, which is the type the
	// cascade exists for.
	if got, err := r.Resolve("reader", keyringCred(), role); err != nil || got.Source != SourceStore {
		t.Errorf("keyring credential resolved to %q (%v), want the credential store", got.Source, err)
	}
}

// The fallback holds secrets in clear text, so a file others can read is refused rather than used.
func TestPlaintextRefusesAWidenedMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not carry on Windows")
	}
	f := tempFile(t)
	if err := f.Set(credName, role, canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if err := os.Chmod(f.Path(), 0o644); err != nil {
		t.Fatalf("Chmod() = %v", err)
	}
	r := NewWith(func(string) string { return "" }, NewMemoryStore(), f, &redact.Redactor{})

	_, err := r.Resolve(credName, keyringCred(), role)

	var missing *MissingSecretError
	if !errors.As(err, &missing) {
		t.Fatalf("Resolve() = %v, want a *MissingSecretError", err)
	}
	if !strings.Contains(err.Error(), "plaintext file (readable by others)") {
		t.Errorf("error = %q, want the widened mode reported as the reason", err)
	}
	// The reading side names the fix as well, not only the stage that refused.
	if !strings.Contains(err.Error(), "chmod 600 "+f.Path()) {
		t.Errorf("error = %q, want the command to fix it", err)
	}
	if strings.Contains(err.Error(), canaryPlaintext) {
		t.Errorf("error = %q, want no line of the file quoted", err)
	}
	// The same state must be classifiable the same way here as on the writing and the deleting side.
	var reading *PermissionError
	if !errors.As(err, &reading) {
		t.Errorf("error = %v, want the permission problem to be reachable", err)
	}

	// Writing through the same file reports the fix instead of quietly repairing it.
	var perm *PermissionError
	if err := f.Set(credName, role, canaryPlaintext); !errors.As(err, &perm) {
		t.Fatalf("Set() = %v, want a *PermissionError", err)
	}
	if !strings.Contains(perm.Error(), "chmod 600 "+f.Path()) {
		t.Errorf("error = %q, want the command to fix it", perm)
	}
	if strings.Contains(perm.Error(), canaryPlaintext) {
		t.Errorf("error = %q, want no line of the file quoted", perm)
	}

	if err := os.Chmod(f.Path(), 0o600); err != nil {
		t.Fatalf("Chmod() = %v", err)
	}
	if got, err := r.Resolve(credName, keyringCred(), role); err != nil || got.Source != SourcePlaintext {
		t.Errorf("resolved %q (%v), want the fallback to work once it is private", got.Source, err)
	}
}

// An unrecognised store selector must not quietly mean the opposite of what was asked for.
func TestStoreSelector(t *testing.T) {
	tests := []struct {
		value   string
		wantErr bool
	}{
		{"", false},
		{"auto", false},
		{"none", false},
		{"off", true},
		{"disabled", true},
	}

	for _, tt := range tests {
		t.Run("selector "+tt.value, func(t *testing.T) {
			t.Setenv(StoreSelector, tt.value)

			store, err := SystemStore()

			if tt.wantErr {
				if err == nil {
					t.Fatalf("SystemStore() = %v, nil; want an error", store)
				}
				if !strings.Contains(err.Error(), StoreSelector) || !strings.Contains(err.Error(), "auto") {
					t.Errorf("error = %q, want the accepted values named", err)
				}
				if strings.Contains(err.Error(), tt.value) {
					t.Errorf("error = %q, want the written value not echoed", err)
				}
				if _, err := New(t.TempDir(), nil); err == nil {
					t.Error("New() = nil error, want the selector rejected there too")
				}
				return
			}
			if err != nil {
				t.Fatalf("SystemStore() = %v", err)
			}
			if store == nil {
				t.Fatal("SystemStore() = nil store")
			}
		})
	}
}

// The store selector switches the store off without reaching for it.
func TestStoreSelectorNone(t *testing.T) {
	t.Setenv(StoreSelector, "none")
	r, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	_, err = r.Resolve(credName, keyringCred(), role)

	var missing *MissingSecretError
	if !errors.As(err, &missing) {
		t.Fatalf("Resolve() = %v, want a *MissingSecretError", err)
	}
	if len(missing.Checked) != 3 || !strings.Contains(missing.Checked[1], "switched off") {
		t.Errorf("checked = %v, want the store reported as switched off", missing.Checked)
	}
}

// A delete that cannot clear every place says where the secret still is and how to get at it. The failure
// that kept an entry back decides the message, not whichever stage happened to fail first.
func TestDeleteReportsWhatItCouldNotClear(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not carry on Windows")
	}

	// widened prepares a fallback holding one entry that nobody but its owner may read, then widens it.
	widened := func(t *testing.T) *File {
		t.Helper()
		f := tempFile(t)
		if err := f.Set(credName, role, canaryPlaintext); err != nil {
			t.Fatalf("Set() = %v", err)
		}
		if err := os.Chmod(f.Path(), 0o644); err != nil {
			t.Fatalf("Chmod() = %v", err)
		}
		return f
	}
	// stillThere proves the entry survived the failed delete.
	stillThere := func(t *testing.T, f *File) {
		t.Helper()
		if err := os.Chmod(f.Path(), 0o600); err != nil {
			t.Fatalf("Chmod() = %v", err)
		}
		if got, err := f.Get(credName, role); err != nil || got != canaryPlaintext {
			t.Errorf("Get() = %q, %v; want the entry that could not be cleared", got, err)
		}
	}

	t.Run("a switched-off store does not hide the real blocker", func(t *testing.T) {
		f := widened(t)
		store := NewMemoryStore()
		store.Fail(ErrDisabled)
		r := NewWith(nil, store, f, nil)

		cleared, err := r.Delete(credName, role)

		var remaining *RemainingError
		if !errors.As(err, &remaining) {
			t.Fatalf("Delete() = %v, %v; want a *RemainingError", cleared, err)
		}
		if len(cleared) != 0 {
			t.Errorf("cleared = %v, want nothing", cleared)
		}
		if len(remaining.Remaining) != 1 || remaining.Remaining[0] != SourcePlaintext {
			t.Errorf("remaining = %v, want the plaintext file", remaining.Remaining)
		}
		if strings.Contains(err.Error(), string(SourceStore)) {
			t.Errorf("error = %q, want no word about the store the user switched off", err)
		}
		if !strings.Contains(err.Error(), "chmod 600 "+f.Path()) {
			t.Errorf("error = %q, want the same way out the reading side gives", err)
		}
		if strings.Contains(err.Error(), canaryPlaintext) {
			t.Errorf("error = %q, want no value in the message", err)
		}
		stillThere(t, f)
	})

	t.Run("a half-done delete is never a silent success", func(t *testing.T) {
		f := widened(t)
		store := NewMemoryStore()
		if err := store.Set(StoreKey(credName, role), canaryStore); err != nil {
			t.Fatalf("Set() = %v", err)
		}
		r := NewWith(nil, store, f, nil)

		cleared, err := r.Delete(credName, role)

		var remaining *RemainingError
		if !errors.As(err, &remaining) {
			t.Fatalf("Delete() = %v, %v; want a *RemainingError", cleared, err)
		}
		if len(cleared) != 1 || cleared[0] != SourceStore {
			t.Errorf("cleared = %v, want the credential store", cleared)
		}
		if len(remaining.Remaining) != 1 || remaining.Remaining[0] != SourcePlaintext {
			t.Errorf("remaining = %v, want the plaintext file", remaining.Remaining)
		}
		if !strings.Contains(err.Error(), "may still be stored in the plaintext file") {
			t.Errorf("error = %q, want the place that still holds it", err)
		}
		if !strings.Contains(err.Error(), "removed from the credential store") {
			t.Errorf("error = %q, want the part that did work", err)
		}
		// The blocker is classifiable, so every caller can treat it as the fixable problem it is.
		var perm *PermissionError
		if !errors.As(err, &perm) {
			t.Errorf("error = %v, want the permission problem to be reachable", err)
		}
		if _, err := store.Get(StoreKey(credName, role)); !errors.Is(err, ErrNoEntry) {
			t.Errorf("store still holds the entry: %v", err)
		}
		stillThere(t, f)
	})

	t.Run("an unreachable store keeps the delete from succeeding", func(t *testing.T) {
		store := NewMemoryStore()
		store.Fail(ErrUnavailable)
		r := NewWith(nil, store, tempFile(t), nil)

		cleared, err := r.Delete(credName, role)

		var remaining *RemainingError
		if !errors.As(err, &remaining) {
			t.Fatalf("Delete() = %v, %v; want a *RemainingError", cleared, err)
		}
		if len(remaining.Remaining) != 1 || remaining.Remaining[0] != SourceStore {
			t.Errorf("remaining = %v, want the credential store", remaining.Remaining)
		}
	})
}

// Every call into the platform store runs under a deadline, and a deadline that passes is the same class
// as a store that cannot be reached: the cascade goes on and the stage says what happened.
func TestStoreDeadline(t *testing.T) {
	t.Run("a call that answers returns its result", func(t *testing.T) {
		got, err := within(time.Minute, func() (string, error) { return "answer", nil })

		if err != nil || got != "answer" {
			t.Errorf("within() = %q, %v; want the result", got, err)
		}
	})

	t.Run("a call that never answers is reported, not waited for", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)

		start := time.Now()
		_, err := within(10*time.Millisecond, func() (string, error) {
			<-release
			return "too late", nil
		})

		if !errors.Is(err, ErrTimedOut) || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("within() = %v, want a timeout that counts as unavailable", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("within() waited %s, want it to give up at the deadline", elapsed)
		}
	})

	t.Run("the cascade goes on and names the stage", func(t *testing.T) {
		store := NewMemoryStore()
		store.Fail(fmt.Errorf("%w: %w", ErrUnavailable, ErrTimedOut))
		r, _, file, _ := fixture(t, nil)
		r = NewWith(nil, store, file, nil)
		if err := file.Set(credName, role, canaryPlaintext); err != nil {
			t.Fatalf("Set() = %v", err)
		}

		got, err := r.Resolve(credName, keyringCred(), role)

		if err != nil {
			t.Fatalf("Resolve() = %v, want the fallback to take over", err)
		}
		if got.Source != SourcePlaintext {
			t.Errorf("source = %q, want %q", got.Source, SourcePlaintext)
		}
		if len(got.Checked) != 2 || !strings.Contains(got.Checked[1], "timed out") {
			t.Errorf("checked = %v, want the timeout named", got.Checked)
		}
	})
}
