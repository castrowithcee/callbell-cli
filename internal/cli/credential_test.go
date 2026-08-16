package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

// Canaries for the two stages a command can write to. Neither may ever reach a stream.
const (
	canaryStored    = "canary-stored-token-7c31d9"
	canaryPlaintext = "canary-plaintext-token-4a08fe"
	canaryEnv       = "canary-env-token-2f76bb"
)

const keyringConfig = `
version: 1
services:
  wiki:
    provider: bookstack
    base_url: https://wiki.example.invalid
credentials:
  vault-reader:
    type: keyring
  env-reader:
    type: env
    values:
      token-id: TEST_ENV_TOKEN_ID
      token-secret: TEST_ENV_TOKEN_SECRET
connections:
  wiki:
    service: wiki
    credential: vault-reader
defaults:
  connections:
    knowledge: wiki
`

// testOptionsIn returns options whose credential resolver reads the process environment, an in-process
// store, and a fallback file in dir. No test may reach the credential store of the machine it runs on.
func testOptionsIn(t *testing.T, dir string, store *secret.MemoryStore) *Options {
	t.Helper()
	if store == nil {
		store = secret.NewMemoryStore()
	}
	red := &redact.Redactor{}
	return &Options{
		Redactor: red,
		Secrets:  secret.NewWith(os.Getenv, store, secret.NewFile(filepath.Join(dir, secret.FileName)), red),
	}
}

func testOptions(t *testing.T, store *secret.MemoryStore) *Options {
	t.Helper()
	return testOptionsIn(t, t.TempDir(), store)
}

// keyringFixture writes a configuration with one keyring credential and one env credential, and returns
// the directory holding it. The fallback file, if one is ever written, lands in the same directory.
func keyringFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(keyringConfig), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

func configIn(dir string) string { return filepath.Join(dir, "config.yaml") }

// runWithInput drives the command surface with a secret on standard input.
func runWithInput(t *testing.T, opts *Options, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(opts, defaultRegistry())
	cmd.SetIn(strings.NewReader(stdin))
	code := run(cmd, opts, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// filesIn returns every regular file in dir, keyed by name.
func filesIn(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() = %v", err)
	}
	files := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile() = %v", err)
		}
		files[e.Name()] = string(data)
	}
	return files
}

// The stored secret goes into the credential store, never into the configuration and never into a file.
func TestCredentialSet(t *testing.T) {
	dir := keyringFixture(t)
	store := secret.NewMemoryStore()
	opts := testOptionsIn(t, dir, store)

	code, stdout, stderr := runWithInput(t, opts, canaryStored+"\n",
		"credential", "set", "vault-reader", "token-id", "--config", configIn(dir))

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("stdout = %q, stderr = %q, want a silent success", stdout, stderr)
	}
	got, err := store.Get(secret.StoreKey("vault-reader", "token-id"))
	if err != nil || got != canaryStored {
		t.Errorf("store holds %q (%v), want the secret", got, err)
	}
	for name, data := range filesIn(t, dir) {
		if name != "config.yaml" {
			t.Errorf("the command created %s, want only the configuration to exist", name)
		}
		if strings.Contains(data, canaryStored) {
			t.Errorf("the canary reached %s", name)
		}
	}
}

// The plaintext fallback never comes into existence on its own. Proven on disk, not in the code path.
func TestCredentialSetNeedsThePlaintextSwitch(t *testing.T) {
	dir := keyringFixture(t)
	store := secret.NewMemoryStore()
	store.Fail(secret.ErrUnavailable)
	opts := testOptionsIn(t, dir, store)

	code, stdout, stderr := runWithInput(t, opts, canaryPlaintext,
		"credential", "set", "vault-reader", "token-id", "--config", configIn(dir))

	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "--plaintext") {
		t.Errorf("stderr = %q, want the named way out", stderr)
	}
	if strings.Contains(stderr, canaryPlaintext) {
		t.Errorf("stderr = %q, want the secret kept out", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, secret.FileName)); !os.IsNotExist(err) {
		t.Fatalf("the plaintext file exists without the switch: %v", err)
	}
	if len(filesIn(t, dir)) != 1 {
		t.Errorf("files = %v, want only the configuration", filesIn(t, dir))
	}
}

// With the switch the fallback is written, once, with the switch recorded in it and mode 0600.
func TestCredentialSetPlaintext(t *testing.T) {
	dir := keyringFixture(t)
	store := secret.NewMemoryStore()
	store.Fail(secret.ErrUnavailable)
	opts := testOptionsIn(t, dir, store)

	code, stdout, stderr := runWithInput(t, opts, canaryPlaintext,
		"credential", "set", "vault-reader", "token-id", "--plaintext", "--config", configIn(dir))

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("stdout = %q, stderr = %q, want a silent success", stdout, stderr)
	}

	path := filepath.Join(dir, secret.FileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}
	if !strings.Contains(string(data), "allow_plaintext: true") {
		t.Errorf("fallback = %q, want the switch recorded", data)
	}
	// The one file that is allowed to hold the secret is this one, and only because it was asked for.
	if !strings.Contains(string(data), canaryPlaintext) {
		t.Errorf("fallback = %q, want the secret stored", data)
	}
	cfg, err := os.ReadFile(configIn(dir))
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}
	if strings.Contains(string(cfg), canaryPlaintext) {
		t.Error("the canary reached the configuration file")
	}
}

// A variable that overrides the store must be named the moment it starts overriding.
func TestCredentialSetWarnsAboutAShadowingVariable(t *testing.T) {
	dir := keyringFixture(t)
	env := secret.DerivedEnvName("vault-reader", "token-id")
	t.Setenv(env, canaryEnv)
	opts := testOptionsIn(t, dir, secret.NewMemoryStore())

	code, stdout, stderr := runWithInput(t, opts, canaryStored,
		"credential", "set", "vault-reader", "token-id", "--config", configIn(dir))

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, env) || !strings.Contains(stderr, "overrides") {
		t.Errorf("stderr = %q, want the shadowing variable named", stderr)
	}
	for _, canary := range []string{canaryEnv, canaryStored} {
		if strings.Contains(stderr, canary) {
			t.Errorf("stderr = %q, want the values kept out", stderr)
		}
	}
}

func TestCredentialSetRejections(t *testing.T) {
	dir := keyringFixture(t)

	tests := []struct {
		name   string
		args   []string
		stdin  string
		wantIn string
	}{
		{"an env credential stores nothing", []string{"env-reader", "token-id"}, canaryStored, "has type \"env\""},
		{"an unknown credential", []string{"absent", "token-id"}, canaryStored, `credential "absent" does not exist`},
		{"an unknown role", []string{"vault-reader", "token"}, canaryStored, "unknown secret role"},
		{"an empty input", []string{"vault-reader", "token-id"}, "\n", "no secret"},
		{"a missing argument", []string{"vault-reader"}, canaryStored, "expected a credential name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testOptionsIn(t, dir, secret.NewMemoryStore())
			args := append([]string{"credential", "set"}, tt.args...)
			args = append(args, "--config", configIn(dir))

			code, stdout, stderr := runWithInput(t, opts, tt.stdin, args...)

			if code != exitUsage {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, tt.wantIn) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantIn)
			}
			if strings.Contains(stderr, canaryStored) {
				t.Errorf("stderr = %q, want the secret kept out", stderr)
			}
		})
	}
}

// Deleting clears the store and the fallback, and says so when there was nothing to clear.
func TestCredentialDelete(t *testing.T) {
	dir := keyringFixture(t)
	store := secret.NewMemoryStore()
	opts := testOptionsIn(t, dir, store)

	if code, _, stderr := runWithInput(t, opts, canaryStored,
		"credential", "set", "vault-reader", "token-id", "--config", configIn(dir)); code != exitOK {
		t.Fatalf("set: exit code = %d (stderr: %s)", code, stderr)
	}
	if code, _, stderr := runWithInput(t, opts, canaryPlaintext,
		"credential", "set", "vault-reader", "token-secret", "--plaintext", "--config", configIn(dir)); code != exitOK {
		t.Fatalf("set --plaintext: exit code = %d (stderr: %s)", code, stderr)
	}

	code, stdout, stderr := runWithInput(t, opts, "",
		"credential", "delete", "vault-reader", "token-id", "--config", configIn(dir))

	if code != exitOK || stdout != "" || stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want a silent success", code, stdout, stderr)
	}
	if _, err := store.Get(secret.StoreKey("vault-reader", "token-id")); err == nil {
		t.Error("the store still holds the entry")
	}

	if code, _, _ := runWithInput(t, opts, "",
		"credential", "delete", "vault-reader", "token-id", "--config", configIn(dir)); code != exitUsage {
		t.Errorf("deleting nothing: exit code = %d, want %d", code, exitUsage)
	}

	// The last plaintext entry takes the file with it.
	if code, _, stderr := runWithInput(t, opts, "",
		"credential", "delete", "vault-reader", "token-secret", "--config", configIn(dir)); code != exitOK {
		t.Fatalf("delete: exit code = %d (stderr: %s)", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, secret.FileName)); !os.IsNotExist(err) {
		t.Errorf("the emptied fallback file was kept: %v", err)
	}
}

// A delete that never consulted the credential store says so. Staying silent would let the run read as
// "the secret is gone everywhere", while the entry in the real store lives on.
func TestCredentialDeleteNamesTheStoreItSkipped(t *testing.T) {
	dir := keyringFixture(t)
	red := &redact.Redactor{}
	// A nil store is what SystemStore returns for CALLBELL_CREDENTIAL_STORE=none: switched off, never
	// consulted, and holding nothing this run was going to touch.
	opts := &Options{
		Redactor: red,
		Secrets:  secret.NewWith(os.Getenv, nil, secret.NewFile(filepath.Join(dir, secret.FileName)), red),
	}

	if code, _, stderr := runWithInput(t, opts, canaryPlaintext,
		"credential", "set", "vault-reader", "token-id", "--plaintext", "--config", configIn(dir)); code != exitOK {
		t.Fatalf("set --plaintext: exit code = %d (stderr: %s)", code, stderr)
	}

	code, stdout, stderr := runWithInput(t, opts, "",
		"credential", "delete", "vault-reader", "token-id", "--config", configIn(dir))

	if code != exitOK || stdout != "" {
		t.Errorf("exit %d, stdout %q; want a success with an empty stdout", code, stdout)
	}
	if !strings.Contains(stderr, secret.StoreSelector) || !strings.Contains(stderr, "may still hold") {
		t.Errorf("stderr = %q, want it to name %s and what may still be stored", stderr, secret.StoreSelector)
	}
	if strings.Contains(stderr, canaryPlaintext) {
		t.Error("the warning carries the secret")
	}

	// A resolver with a working store clears it and therefore warns about nothing.
	plain := testOptionsIn(t, keyringFixture(t), nil)
	if code, _, stderr := runWithInput(t, plain, canaryStored,
		"credential", "set", "vault-reader", "token-id", "--config", configIn(dir)); code != exitOK {
		t.Fatalf("set: exit code = %d (stderr: %s)", code, stderr)
	}
	if code, _, stderr := runWithInput(t, plain, "",
		"credential", "delete", "vault-reader", "token-id", "--config", configIn(dir)); code != exitOK || stderr != "" {
		t.Errorf("exit %d, stderr %q; want a silent success when the store was cleared", code, stderr)
	}
}

// The source of every secret is visible, and the value of none of them is.
func TestConfigValidateSecrets(t *testing.T) {
	dir := keyringFixture(t)
	store := secret.NewMemoryStore()
	if err := store.Set(secret.StoreKey("vault-reader", "token-secret"), canaryStored); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	t.Setenv(secret.DerivedEnvName("vault-reader", "token-id"), canaryEnv)
	opts := testOptionsIn(t, dir, store)

	code, stdout, stderr := runWithInput(t, opts, "",
		"config", "validate", "--secrets", "--agent", "--config", configIn(dir))

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	want := "connection|credential|role|source|checked\n" +
		"wiki|vault-reader|token-id|environment variable|\n" +
		"wiki|vault-reader|token-secret|credential store|environment variable (not set)\n"
	if stdout != want {
		t.Errorf("stdout = %q,\nwant %q", stdout, want)
	}
	for _, canary := range []string{canaryEnv, canaryStored} {
		if strings.Contains(stdout, canary) || strings.Contains(stderr, canary) {
			t.Errorf("a canary reached the output:\nstdout: %s\nstderr: %s", stdout, stderr)
		}
	}
}

// A secret no stage delivers is reported as missing, with the stages that were tried.
func TestConfigValidateSecretsReportsMissing(t *testing.T) {
	dir := keyringFixture(t)
	opts := testOptionsIn(t, dir, secret.NewMemoryStore())

	code, stdout, _ := runWithInput(t, opts, "",
		"config", "validate", "--secrets", "--agent", "--config", configIn(dir))

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "|missing|environment variable (not set), credential store (no entry), plaintext file (not enabled)") {
		t.Errorf("stdout = %q, want every stage reported as tried", stdout)
	}
}

// Without the flag, validate stays the silent local file check it documents.
func TestConfigValidateStaysSilentWithoutTheFlag(t *testing.T) {
	dir := keyringFixture(t)
	opts := testOptionsIn(t, dir, secret.NewMemoryStore())

	code, stdout, stderr := runWithInput(t, opts, "", "config", "validate", "--config", configIn(dir))

	if code != exitOK || stdout != "" || stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want a silent success", code, stdout, stderr)
	}
}

// The source report is a check, not a listing: it must never be cut short by the record limit, because
// the rows it silently dropped would read as an all-clear.
func TestConfigValidateSecretsIsNotTruncated(t *testing.T) {
	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	dir := t.TempDir()

	const connections = 30 // two roles each, well past the default limit of 50 records
	var b strings.Builder
	b.WriteString("version: 1\nservices:\n")
	for i := 0; i < connections; i++ {
		fmt.Fprintf(&b, "  s%02d:\n    provider: bookstack\n    base_url: https://wiki%02d.example.invalid\n", i, i)
	}
	b.WriteString("credentials:\n")
	for i := 0; i < connections; i++ {
		fmt.Fprintf(&b, "  c%02d:\n    type: keyring\n", i)
	}
	b.WriteString("connections:\n")
	for i := 0; i < connections; i++ {
		fmt.Fprintf(&b, "  k%02d:\n    service: s%02d\n    credential: c%02d\n", i, i, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	opts := testOptionsIn(t, dir, secret.NewMemoryStore())

	code, stdout, stderr := runWithInput(t, opts, "",
		"config", "validate", "--secrets", "--agent", "--config", filepath.Join(dir, "config.yaml"))

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	// One header line plus one row per connection and role.
	if got, want := strings.Count(stdout, "\n"), 1+2*connections; got != want {
		t.Errorf("lines = %d, want %d; the report was cut short", got, want)
	}
	if !strings.Contains(stdout, "k29|c29|token-secret|") {
		t.Errorf("stdout = %q, want the last connection reported", stdout)
	}
}

// An unrecognised store selector is refused instead of quietly meaning its opposite. This is the one test
// that lets the command build its own resolver, which is safe because the selector fails before any store
// is reached.
func TestUnknownStoreSelectorIsRejected(t *testing.T) {
	dir := keyringFixture(t)
	t.Setenv(secret.StoreSelector, "off")
	opts := &Options{Redactor: &redact.Redactor{}}

	code, stdout, stderr := runWithInput(t, opts, "",
		"config", "validate", "--secrets", "--config", configIn(dir))

	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, secret.StoreSelector) {
		t.Errorf("stderr = %q, want the selector named", stderr)
	}
}

// A delete that leaves a copy behind is a usage error naming the file and the fix, and it carries the same
// error code as every other run-in with that file, so an agent branching on the code sees one state.
func TestCredentialDeleteReportsARetainedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not carry on Windows")
	}
	dir := keyringFixture(t)
	store := secret.NewMemoryStore()
	opts := testOptionsIn(t, dir, store)
	fallback := filepath.Join(dir, secret.FileName)

	if code, _, stderr := runWithInput(t, opts, canaryStored,
		"credential", "set", "vault-reader", "token-id", "--config", configIn(dir)); code != exitOK {
		t.Fatalf("set: exit %d (stderr %q)", code, stderr)
	}
	if code, _, stderr := runWithInput(t, opts, canaryPlaintext,
		"credential", "set", "vault-reader", "token-id", "--plaintext", "--config", configIn(dir)); code != exitOK {
		t.Fatalf("set --plaintext: exit %d (stderr %q)", code, stderr)
	}
	if err := os.Chmod(fallback, 0o644); err != nil {
		t.Fatalf("Chmod() = %v", err)
	}

	code, stdout, stderr := runWithInput(t, opts, "",
		"credential", "delete", "vault-reader", "token-id", "--config", configIn(dir))

	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.HasPrefix(stderr, "callbell: config-invalid: ") {
		t.Errorf("stderr = %q, want the same code the file gets everywhere else", stderr)
	}
	for _, want := range []string{"may still be stored in the plaintext file", "chmod 600 " + fallback} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
	for _, canary := range []string{canaryStored, canaryPlaintext} {
		if strings.Contains(stderr, canary) {
			t.Errorf("stderr = %q, want no value in the message", stderr)
		}
	}
	// The copy really is still there, and the fix really is the one named.
	if err := os.Chmod(fallback, 0o600); err != nil {
		t.Fatalf("Chmod() = %v", err)
	}
	if code, _, stderr := runWithInput(t, opts, "",
		"credential", "delete", "vault-reader", "token-id", "--config", configIn(dir)); code != exitOK {
		t.Fatalf("delete after the fix: exit %d (stderr %q)", code, stderr)
	}
	if _, err := os.Stat(fallback); !os.IsNotExist(err) {
		t.Errorf("the fallback survived the successful delete: %v", err)
	}
}

// Writing into a fallback others can read is refused with the same code, not with a runtime error.
func TestCredentialSetPlaintextRefusesAWidenedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not carry on Windows")
	}
	dir := keyringFixture(t)
	opts := testOptionsIn(t, dir, secret.NewMemoryStore())
	fallback := filepath.Join(dir, secret.FileName)

	if code, _, stderr := runWithInput(t, opts, canaryPlaintext,
		"credential", "set", "vault-reader", "token-id", "--plaintext", "--config", configIn(dir)); code != exitOK {
		t.Fatalf("set --plaintext: exit %d (stderr %q)", code, stderr)
	}
	if err := os.Chmod(fallback, 0o644); err != nil {
		t.Fatalf("Chmod() = %v", err)
	}

	code, _, stderr := runWithInput(t, opts, canaryPlaintext,
		"credential", "set", "vault-reader", "token-secret", "--plaintext", "--config", configIn(dir))

	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if !strings.HasPrefix(stderr, "callbell: config-invalid: ") {
		t.Errorf("stderr = %q, want the config-invalid code", stderr)
	}
	if !strings.Contains(stderr, "chmod 600 "+fallback) {
		t.Errorf("stderr = %q, want the fix named", stderr)
	}
}

// A secret that comes out of the credential store must not reach any stream either.
//
// The other two stages of the cascade are covered elsewhere: a secret from an environment variable in
// TestKnowledgeNoCanaryInOutput, and a secret written straight into the configuration in the acceptance
// run. This is the third stage, and it is the one that cannot be driven through a subprocess: the store of
// the machine is off limits, so the run happens in this process with the in-process store the resolver
// takes through its Store interface.
//
// The setup proves the stage really delivered rather than being skipped: the mock refuses any request that
// does not carry exactly the stored token, and one endpoint answers with the credential echoed back, the
// way a hostile or careless provider does.
func TestStoredSecretNeverReachesTheOutput(t *testing.T) {
	const (
		storedID     = "canary-store-id-2d84f1"
		storedSecret = "canary-store-secret-9e05ba"
	)

	t.Setenv("CALLBELL_CONFIG", "")
	t.Setenv("CALLBELL_CLI_HOME", "")
	// Nothing may shadow the store, or the run would prove the wrong stage.
	t.Setenv(secret.DerivedEnvName("vault-reader", "token-id"), "")
	t.Setenv(secret.DerivedEnvName("vault-reader", "token-secret"), "")

	auth := "Token " + storedID + ":" + storedSecret
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != auth {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"No authorization token found on the request"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total": 1,
			"data": []map[string]any{{"id": 1, "book_id": 7, "chapter_id": 0, "name": "Vault Runbook",
				"slug": "vault", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z"}},
		})
	})
	mux.HandleFunc("/api/pages/1", func(w http.ResponseWriter, r *http.Request) {
		// The echo: whatever the provider says about the credential must still not be published.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"failed for ` + r.Header.Get("Authorization") + `"}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`
version: 1
services:
  wiki:
    provider: bookstack
    base_url: %s
credentials:
  vault-reader:
    type: keyring
connections:
  wiki:
    service: wiki
    credential: vault-reader
defaults:
  connections:
    knowledge: wiki
`, server.URL)), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	store := secret.NewMemoryStore()
	for role, value := range map[string]string{"token-id": storedID, "token-secret": storedSecret} {
		if err := store.Set(secret.StoreKey("vault-reader", role), value); err != nil {
			t.Fatalf("Set() = %v", err)
		}
	}

	// Every call gets its own options, and therefore its own redactor, the way every call of the binary is
	// its own process. A run must not be covered by what an earlier run happened to register.
	call := func(t *testing.T, args ...string) (int, string, string) {
		t.Helper()
		return runWithInput(t, testOptionsIn(t, dir, store), "", append(args, "--config", cfg)...)
	}

	// The stage really is the one under test, and it really does deliver.
	code, stdout, stderr := call(t, "config", "validate", "--secrets", "--agent")
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	want := "connection|credential|role|source|checked\n" +
		"wiki|vault-reader|token-id|credential store|environment variable (not set)\n" +
		"wiki|vault-reader|token-secret|credential store|environment variable (not set)\n"
	if stdout != want {
		t.Fatalf("stdout = %q,\nwant %q", stdout, want)
	}

	var seen strings.Builder
	commands := [][]string{
		{"config", "validate"},
		{"config", "validate", "--secrets"},
		{"config", "validate", "--secrets", "--agent"},
		{"config", "validate", "--secrets", "--output", "json"},
		{"capabilities"},
		{"capabilities", "--agent"},
		{"describe", "knowledge.pages.list"},
		{"knowledge", "pages", "list"},
		{"knowledge", "pages", "list", "--agent"},
		{"knowledge", "pages", "list", "--output", "json"},
		{"knowledge", "pages", "list", "--connection", "wiki"},
		{"knowledge", "pages", "get", "1"},
		{"knowledge", "pages", "get", "1", "--output", "json"},
		{"credential", "delete", "vault-reader", "token-id"},
	}
	for _, args := range commands {
		_, stdout, stderr := call(t, args...)
		seen.WriteString(stdout)
		seen.WriteString(stderr)
		for _, canary := range []string{storedID, storedSecret} {
			if strings.Contains(stdout, canary) {
				t.Errorf("%v: the canary reached stdout: %q", args, stdout)
			}
			if strings.Contains(stderr, canary) {
				t.Errorf("%v: the canary reached stderr: %q", args, stderr)
			}
		}
	}

	// The listing worked, so the mock accepted the stored token, and the echo really was echoed and
	// really was caught.
	if !strings.Contains(seen.String(), "Vault Runbook") {
		t.Errorf("output = %q, want the page the stored token unlocks", seen.String())
	}
	if !strings.Contains(seen.String(), "[redacted]") {
		t.Errorf("output = %q, want the redaction marker where the provider echoed the credential", seen.String())
	}

	// Nothing the runs wrote beside the configuration may carry the secret either.
	for name, data := range filesIn(t, dir) {
		for _, canary := range []string{storedID, storedSecret} {
			if strings.Contains(data, canary) {
				t.Errorf("the canary reached the file %s", name)
			}
		}
	}
}

// A limit the user set themselves is refused rather than accepted and ignored.
func TestConfigValidateSecretsRejectsAnExplicitLimit(t *testing.T) {
	dir := keyringFixture(t)
	opts := testOptionsIn(t, dir, secret.NewMemoryStore())

	code, stdout, stderr := runWithInput(t, opts, "",
		"config", "validate", "--secrets", "--limit", "10", "--config", configIn(dir))

	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitUsage, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "--limit does not apply") {
		t.Errorf("stderr = %q, want the refusal to name the flag", stderr)
	}

	// The default limit stays without effect, so the ordinary call is unaffected.
	if code, _, stderr := runWithInput(t, opts, "",
		"config", "validate", "--secrets", "--agent", "--config", configIn(dir)); code != exitOK {
		t.Errorf("plain call: exit %d (stderr %q)", code, stderr)
	}
}
