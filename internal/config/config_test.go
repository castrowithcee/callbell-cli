package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validFixture = "testdata/valid.yaml"

// minimal is a valid configuration used as the base for the invalid variants below.
const minimal = `
version: 1
services:
  wiki:
    provider: bookstack
    base_url: https://wiki.example.invalid
credentials:
  reader:
    type: env
    values:
      token-id: WIKI_TOKEN_ID
      token-secret: WIKI_TOKEN_SECRET
connections:
  wiki:
    service: wiki
    credential: reader
defaults:
  connections:
    knowledge: wiki
`

func TestLoadValidFixture(t *testing.T) {
	cfg, err := Load(validFixture, testProviders)
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	if cfg.Version != Version {
		t.Errorf("version = %d, want %d", cfg.Version, Version)
	}
	// Two services of the same provider.
	if got := cfg.Services["wiki-primary"].Provider; got != "bookstack" {
		t.Errorf("wiki-primary provider = %q, want bookstack", got)
	}
	if got := cfg.Services["wiki-archive"].Provider; got != "bookstack" {
		t.Errorf("wiki-archive provider = %q, want bookstack", got)
	}
	// Two connections with different credentials on the same service.
	wiki, audit := cfg.Connections["wiki"], cfg.Connections["wiki-audit"]
	if wiki.Service != audit.Service {
		t.Errorf("wiki and wiki-audit use different services: %q and %q", wiki.Service, audit.Service)
	}
	if wiki.Credential == audit.Credential {
		t.Errorf("wiki and wiki-audit share credential %q, want different ones", wiki.Credential)
	}
	// One credential reused across two services.
	if cfg.Connections["archive"].Credential != wiki.Credential {
		t.Error("archive should reuse the wiki-reader credential")
	}
	if got := cfg.Services["wiki-archive"].Options["page_size"]; got != "50" {
		t.Errorf("wiki-archive page_size = %q, want 50", got)
	}
	// A description is optional: the fixture maintains one for archive and none for wiki, and both are
	// valid configurations of the same schema version.
	if got := cfg.Connections["archive"].Description; got != "read-only copy of the wiki, safe for bulk reads" {
		t.Errorf("archive description = %q", got)
	}
	if got := wiki.Description; got != "" {
		t.Errorf("wiki description = %q, want the empty string for a connection without one", got)
	}
}

// The connection description is optional prose the user maintains and discovery publishes. It arrives in
// one normalized form, it stays one short line, and a refusal never quotes what was written.
func TestConnectionDescriptionIsOneShortNormalizedLine(t *testing.T) {
	const canary = "config-description-canary-8c17"
	withDescription := func(lines string) string {
		return strings.Replace(minimal, "    credential: reader\n", "    credential: reader\n"+lines, 1)
	}

	t.Run("an absent description is the empty string", func(t *testing.T) {
		cfg, err := Decode(strings.NewReader(minimal), testProviders)
		if err != nil {
			t.Fatalf("Decode() = %v", err)
		}
		if got := cfg.Connections["wiki"].Description; got != "" {
			t.Errorf("description = %q, want the empty string", got)
		}
	})

	t.Run("blanks at the edges and between words are normalized", func(t *testing.T) {
		cfg, err := Decode(strings.NewReader(
			withDescription("    description: \"  the live   team wiki \"\n")), testProviders)
		if err != nil {
			t.Fatalf("Decode() = %v", err)
		}
		if got, want := cfg.Connections["wiki"].Description, "the live team wiki"; got != want {
			t.Errorf("description = %q, want %q", got, want)
		}
	})

	t.Run("a description of exactly the limit is accepted", func(t *testing.T) {
		cfg, err := Decode(strings.NewReader(
			withDescription("    description: "+strings.Repeat("a", maxDescriptionLength)+"\n")), testProviders)
		if err != nil {
			t.Fatalf("Decode() = %v", err)
		}
		if got := len(cfg.Connections["wiki"].Description); got != maxDescriptionLength {
			t.Errorf("description length = %d, want %d", got, maxDescriptionLength)
		}
	})

	refusals := []struct {
		name, lines, want string
	}{
		{
			name:  "a line break",
			lines: "    description: |\n      " + canary + "\n      second line\n",
			want:  "must not contain a line break",
		},
		{
			name:  "one character too many",
			lines: "    description: " + canary + strings.Repeat("b", maxDescriptionLength+1-len(canary)) + "\n",
			want:  "is 201 characters long",
		},
	}
	for _, tt := range refusals {
		t.Run(tt.name+" is refused", func(t *testing.T) {
			_, err := Decode(strings.NewReader(withDescription(tt.lines)), testProviders)
			if err == nil {
				t.Fatal("Decode() = nil error")
			}
			message := err.Error()
			if !strings.Contains(message, "connections.wiki.description: ") || !strings.Contains(message, tt.want) {
				t.Errorf("error = %q, want it to name the key and %q", message, tt.want)
			}
			// A description is free text, which is exactly where a secret gets pasted by accident.
			if strings.Contains(message, canary) {
				t.Errorf("error = %q, want it not to quote the description", message)
			}
		})
	}
}

// The shipped example must stay valid, otherwise the documentation promises a file that does not load.
func TestShippedExampleIsValid(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "examples", "config.yaml"), testProviders); err != nil {
		t.Fatalf("examples/config.yaml is invalid: %v", err)
	}
}

func TestDecodeRejects(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantIn  string
		replace [2]string // optional single replacement applied to the minimal config
	}{
		{
			name:    "version zero",
			replace: [2]string{"version: 1", "version: 0"},
			wantIn:  "version: got 0, want 1",
		},
		{
			name:    "future version",
			replace: [2]string{"version: 1", "version: 2"},
			wantIn:  "version: got 2, want 1",
		},
		{
			name:    "unknown top level key",
			replace: [2]string{"version: 1", "version: 1\nproviders: {}"},
			wantIn:  "line 3: unknown key",
		},
		{
			name:    "unknown service key",
			replace: [2]string{"provider: bookstack", "provider: bookstack\n    api_key: nope"},
			wantIn:  "line 6: unknown key",
		},
		{
			name:    "unknown provider",
			replace: [2]string{"provider: bookstack", "provider: confluence"},
			wantIn:  `unknown provider "confluence"`,
		},
		{
			name:    "missing base url",
			replace: [2]string{"base_url: https://wiki.example.invalid", "base_url: \"\""},
			wantIn:  "base_url: must not be empty",
		},
		{
			name:    "base url without scheme",
			replace: [2]string{"base_url: https://wiki.example.invalid", "base_url: wiki.example.invalid"},
			wantIn:  "must use scheme http or https",
		},
		{
			name:    "base url without host",
			replace: [2]string{"base_url: https://wiki.example.invalid", "base_url: https:///books"},
			wantIn:  "must contain a host",
		},
		{
			name:    "unknown credential type",
			replace: [2]string{"type: env", "type: vault"},
			wantIn:  `type must be one of env, keyring, got "vault"`,
		},
		{
			// A keyring credential that carries values is the accident this type exists to prevent:
			// whatever stands there is most likely the secret itself.
			name:    "keyring credential with values",
			replace: [2]string{"type: env", "type: keyring"},
			wantIn:  "credentials.reader.values: must be absent for a keyring credential",
		},
		{
			name:    "invalid environment variable name",
			replace: [2]string{"token-id: WIKI_TOKEN_ID", "token-id: wiki token id"},
			wantIn:  "credentials.reader.values.token-id: must be the name of an environment variable",
		},
		{
			name:    "unknown service reference",
			replace: [2]string{"service: wiki", "service: absent"},
			wantIn:  `connections.wiki.service: unknown service "absent"`,
		},
		{
			name:    "unknown credential reference",
			replace: [2]string{"credential: reader", "credential: absent"},
			wantIn:  `connections.wiki.credential: unknown credential "absent"`,
		},
		{
			name:    "unknown default connection",
			replace: [2]string{"knowledge: wiki", "knowledge: absent"},
			wantIn:  `defaults.connections.knowledge: unknown connection "absent"`,
		},
		{
			name:    "missing secret role for provider",
			replace: [2]string{"      token-secret: WIKI_TOKEN_SECRET\n", ""},
			wantIn:  `requires the secret role "token-secret"`,
		},
		{
			name:    "duplicate default for one domain",
			replace: [2]string{"    knowledge: wiki", "    knowledge: wiki\n    knowledge: wiki"},
			wantIn:  "line 20: duplicate key",
		},
		{
			name:   "empty document",
			yaml:   "",
			wantIn: "empty",
		},
		{
			name:   "two documents",
			yaml:   minimal + "\n---\n" + minimal,
			wantIn: "exactly one document",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.yaml
			if tt.replace[0] != "" {
				in = strings.Replace(minimal, tt.replace[0], tt.replace[1], 1)
				if in == minimal {
					t.Fatalf("replacement %q did not apply; the fixture drifted", tt.replace[0])
				}
			}

			_, err := Decode(strings.NewReader(in), testProviders)

			if err == nil {
				t.Fatal("Decode() = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantIn)
			}
		})
	}
}

// A configuration file may hold a secret in a value or in a key, and the YAML library quotes the text it
// choked on. No decoder error may carry that text out of the package.
func TestADecodeErrorNeverQuotesTheFile(t *testing.T) {
	const canary = "S3CRET-CANARY-9f2a"

	tests := []struct {
		name    string
		replace [2]string
		wantIn  string
	}{
		// The two triggers proven by an independent check of a built binary.
		{
			name:    "alias to an undefined anchor",
			replace: [2]string{"token-id: WIKI_TOKEN_ID", "token-id: *" + canary},
			wantIn:  "anchor that is not defined",
		},
		{
			name:    "explicit tag the value does not satisfy",
			replace: [2]string{"token-id: WIKI_TOKEN_ID", "token-id: !!int " + canary},
			wantIn:  "explicit type tag",
		},
		{
			name:    "explicit tag on a whole credential",
			replace: [2]string{"    type: env", "    type: !!bool " + canary},
			wantIn:  "explicit type tag",
		},
		// A key can be a secret too, and an unknown key is quoted just like a value.
		{
			name:    "unknown key",
			replace: [2]string{"version: 1", "version: 1\n" + canary + ": {}"},
			wantIn:  "unknown key",
		},
		{
			name:    "duplicate key",
			replace: [2]string{"    knowledge: wiki", "    " + canary + ": wiki\n    " + canary + ": wiki"},
			wantIn:  "duplicate key",
		},
		// A plain syntax error has to keep enough diagnosis to find the place.
		{
			name:    "mapping value in the wrong context",
			replace: [2]string{"base_url: https://wiki.example.invalid", canary + ": a: b"},
			wantIn:  "line 6",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := strings.Replace(minimal, tt.replace[0], tt.replace[1], 1)
			if in == minimal {
				t.Fatalf("replacement %q did not apply; the fixture drifted", tt.replace[0])
			}

			_, err := Decode(strings.NewReader(in), testProviders)

			if err == nil {
				t.Fatal("Decode() = nil, want an error")
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("error quotes the file: %q", err)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantIn)
			}
		})
	}
}

// Validate reports every problem at once rather than stopping at the first one.
func TestValidateReportsAllProblems(t *testing.T) {
	in := strings.NewReplacer(
		"version: 1", "version: 3",
		"service: wiki", "service: absent",
		"knowledge: wiki", "knowledge: absent",
	).Replace(minimal)

	_, err := Decode(strings.NewReader(in), testProviders)

	if err == nil {
		t.Fatal("Decode() = nil, want an error")
	}
	for _, want := range []string{"version: got 3", "unknown service", "unknown connection"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%s", want, err)
		}
	}
}

func TestPathPriority(t *testing.T) {
	home := t.TempDir()
	cliHome := t.TempDir()

	// setPathEnv pins every input of Path so no value leaks in from the caller's environment.
	setPathEnv := func(t *testing.T, configFile, cliHomeDir string) {
		t.Helper()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
		t.Setenv("CALLBELL_CONFIG", configFile)
		t.Setenv("CALLBELL_CLI_HOME", cliHomeDir)
	}

	t.Run("explicit flag wins", func(t *testing.T) {
		setPathEnv(t, "/from/env.yaml", cliHome)

		got, err := Path("/from/flag.yaml")

		if err != nil || got != "/from/flag.yaml" {
			t.Errorf("Path() = %q, %v, want /from/flag.yaml", got, err)
		}
	})

	t.Run("CALLBELL_CONFIG wins over CALLBELL_CLI_HOME", func(t *testing.T) {
		setPathEnv(t, "/from/env.yaml", cliHome)

		got, err := Path("")

		if err != nil || got != "/from/env.yaml" {
			t.Errorf("Path() = %q, %v, want /from/env.yaml", got, err)
		}
	})

	t.Run("CALLBELL_CLI_HOME wins over the default location", func(t *testing.T) {
		setPathEnv(t, "", cliHome)

		got, err := Path("")

		want := filepath.Join(cliHome, "config.yaml")
		if err != nil || got != want {
			t.Errorf("Path() = %q, %v, want %q", got, err, want)
		}
	})

	t.Run("default location", func(t *testing.T) {
		setPathEnv(t, "", "")

		got, err := Path("")

		want := filepath.Join(home, ".callbell", "cli", "config.yaml")
		if err != nil || got != want {
			t.Errorf("Path() = %q, %v, want %q", got, err, want)
		}
	})
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")

	_, err := Load(path, testProviders)

	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Load() = %v, want a *NotFoundError", err)
	}
	if notFound.Path != path {
		t.Errorf("path = %q, want %q", notFound.Path, path)
	}
}

func TestResolve(t *testing.T) {
	cfg, err := Load(validFixture, testProviders)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	t.Run("explicit connection wins over the default", func(t *testing.T) {
		got, err := cfg.Resolve("wiki-audit", "knowledge")
		if err != nil {
			t.Fatalf("Resolve() = %v", err)
		}
		if got.Name != "wiki-audit" || got.Credential != "wiki-auditor" {
			t.Errorf("resolved %+v, want the wiki-audit connection", got)
		}
		if got.Secrets.Type != CredentialTypeEnv || got.Secrets.Values["token-id"] != "CALLBELL_WIKI_AUDITOR_TOKEN_ID" {
			t.Errorf("secrets = %+v, want the auditor variables", got.Secrets)
		}
	})

	t.Run("domain default is used", func(t *testing.T) {
		got, err := cfg.Resolve("", "knowledge")
		if err != nil {
			t.Fatalf("Resolve() = %v", err)
		}
		if got.Name != "wiki" || got.BaseURL != "https://wiki.example.invalid" {
			t.Errorf("resolved %+v, want the wiki connection", got)
		}
	})

	t.Run("target is carried through", func(t *testing.T) {
		got, err := cfg.Resolve("archive", "knowledge")
		if err != nil {
			t.Fatalf("Resolve() = %v", err)
		}
		if got.Target != "books" {
			t.Errorf("target = %q, want books", got.Target)
		}
	})

	t.Run("unknown connection", func(t *testing.T) {
		_, err := cfg.Resolve("absent", "knowledge")

		var sel *SelectionError
		if !errors.As(err, &sel) {
			t.Fatalf("Resolve() = %v, want a *SelectionError", err)
		}
		if !strings.Contains(err.Error(), `unknown connection "absent"`) {
			t.Errorf("error = %q", err)
		}
	})

	t.Run("no default for the domain", func(t *testing.T) {
		_, err := cfg.Resolve("", "tickets")

		var sel *SelectionError
		if !errors.As(err, &sel) {
			t.Fatalf("Resolve() = %v, want a *SelectionError", err)
		}
		if !strings.Contains(err.Error(), "defaults.connections.tickets") {
			t.Errorf("error = %q, want it to name the missing default", err)
		}
	})
}

// Configuration only names environment variables. Even with the referenced variables set to canary
// values, neither successful resolution nor any error may reveal them.
func TestNoSecretValueLeaks(t *testing.T) {
	const canary = "s3cr3t-canary-9f3a1c"
	t.Setenv("CALLBELL_WIKI_READER_TOKEN_ID", canary)
	t.Setenv("CALLBELL_WIKI_READER_TOKEN_SECRET", canary)
	t.Setenv("WIKI_TOKEN_ID", canary)
	t.Setenv("WIKI_TOKEN_SECRET", canary)

	cfg, err := Load(validFixture, testProviders)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	resolved, err := cfg.Resolve("wiki", "knowledge")
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	for role, name := range resolved.Secrets.Values {
		if strings.Contains(name, canary) {
			t.Errorf("resolved role %q carries the secret value instead of the variable name", role)
		}
	}

	// Every error path that mentions credentials.
	broken := []string{
		strings.Replace(minimal, "type: env", "type: keyring", 1),
		strings.Replace(minimal, "      token-secret: WIKI_TOKEN_SECRET\n", "", 1),
		strings.Replace(minimal, "credential: reader", "credential: absent", 1),
	}
	for _, in := range broken {
		_, err := Decode(strings.NewReader(in), testProviders)
		if err == nil {
			t.Fatal("Decode() = nil, want an error")
		}
		if strings.Contains(err.Error(), canary) {
			t.Errorf("error leaks the secret value: %s", err)
		}
	}

	if _, err := cfg.Resolve("absent", "knowledge"); err == nil || strings.Contains(err.Error(), canary) {
		t.Errorf("resolution error leaks the secret value: %v", err)
	}
}

// A user who pastes a secret into a credential field instead of the variable name must not see it echoed
// back. The redactor cannot help here: nothing ever resolved that value, so it is not registered.
func TestPastedSecretIsNeverQuotedBack(t *testing.T) {
	const canary = "canary-pasted-secret-4e91b7"
	// Handwritten configuration with the canary where an environment variable name belongs.
	pasted := `version: 1
services:
  wiki:
    provider: bookstack
    base_url: https://wiki.example.invalid
credentials:
  reader:
    type: env
    values:
      token-id: ` + canary + `
      token-secret: WIKI_TOKEN_SECRET
connections:
  wiki:
    service: wiki
    credential: reader
defaults:
  connections:
    knowledge: wiki
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(pasted), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Load(path, testProviders)

	if err == nil {
		t.Fatal("Load() = nil, want an error")
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("the message quotes the pasted secret: %s", err)
	}
	// The role and the rule are what the user needs instead.
	for _, want := range []string{"credentials.reader.values.token-id", "name of an environment variable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%s", want, err)
		}
	}
}

// Names end up as configuration keys and as --connection arguments, so they follow one fixed rule.
func TestNamesFollowTheNameRule(t *testing.T) {
	rejected := []struct {
		name    string
		replace [2]string
	}{
		{"service name with a space", [2]string{"  wiki:\n    provider", "  \"wiki primary\":\n    provider"}},
		{"credential name with a space", [2]string{"  reader:\n    type", "  \"reader one\":\n    type"}},
		{"connection name with a space", [2]string{"  wiki:\n    service", "  \"wiki audit\":\n    service"}},
		{"domain with a space", [2]string{"    knowledge: wiki", "    \"knowledge base\": wiki"}},
		{"name with a slash", [2]string{"  wiki:\n    service", "  wiki/audit:\n    service"}},
		{"name ending in a separator", [2]string{"  wiki:\n    service", "  wiki-:\n    service"}},
	}

	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			in := strings.Replace(minimal, tt.replace[0], tt.replace[1], 1)
			if in == minimal {
				t.Fatalf("replacement %q did not apply; the fixture drifted", tt.replace[0])
			}

			_, err := Decode(strings.NewReader(in), testProviders)

			if err == nil {
				t.Fatal("Decode() = nil, want an error")
			}
			// The rejection has to state what is allowed, otherwise the user has to guess.
			if !strings.Contains(err.Error(), nameRule) {
				t.Errorf("error = %q, want it to state the name rule", err)
			}
		})
	}

	// The names the documentation and the examples use stay valid.
	for _, name := range []string{"wiki-primary", "wiki-audit", "knowledge", "wiki_reader", "v1.0", "a"} {
		if err := validateName(name); err != nil {
			t.Errorf("validateName(%q) = %v, want nil", name, err)
		}
	}
	if err := validateName(""); err == nil {
		t.Error("validateName(\"\") = nil, want an error")
	}
}

// A credential may name the system its secrets belong to. The field is optional, an unknown provider is
// refused, and a role the named provider does not define would never be read, so it is refused too.
func TestCredentialProviderIsOptionalAndChecked(t *testing.T) {
	withProvider := func(line string) string {
		return strings.Replace(minimal, "  reader:\n", "  reader:\n"+line, 1)
	}

	t.Run("an absent provider stays valid", func(t *testing.T) {
		cfg, err := Decode(strings.NewReader(minimal), testProviders)
		if err != nil {
			t.Fatalf("Decode() = %v", err)
		}
		if got := cfg.Credentials["reader"].Provider; got != "" {
			t.Errorf("provider = %q, want the empty string", got)
		}
	})

	t.Run("a named provider round-trips", func(t *testing.T) {
		cfg, err := Decode(strings.NewReader(withProvider("    provider: bookstack\n")), testProviders)
		if err != nil {
			t.Fatalf("Decode() = %v", err)
		}
		if got := cfg.Credentials["reader"].Provider; got != "bookstack" {
			t.Errorf("provider = %q, want bookstack", got)
		}
	})

	t.Run("an unknown provider is refused", func(t *testing.T) {
		_, err := Decode(strings.NewReader(withProvider("    provider: bookstck\n")), testProviders)
		if err == nil || !strings.Contains(err.Error(), `credentials.reader: unknown provider "bookstck"`) {
			t.Errorf("Decode() = %v, want it to name the unknown provider", err)
		}
	})

	t.Run("a role the provider does not define is refused", func(t *testing.T) {
		_, err := Decode(strings.NewReader(strings.Replace(withProvider("    provider: bookstack\n"),
			"      token-id: WIKI_TOKEN_ID\n", "      bot-token: WIKI_BOT_TOKEN\n", 1)), testProviders)
		if err == nil || !strings.Contains(err.Error(),
			"credentials.reader.values.bot-token: bookstack does not define this secret role") {
			t.Errorf("Decode() = %v, want it to name the role and the provider", err)
		}
	})
}

// A connection whose credential belongs to another provider cannot work: the secret roles it holds are
// the other provider's. The file says so instead of letting the first call look like a rejected token.
func TestAConnectionMustNotMixProviders(t *testing.T) {
	mixed := strings.Replace(minimal, "  reader:\n", "  reader:\n    provider: telegram\n", 1)
	mixed = strings.Replace(mixed, "      token-id: WIKI_TOKEN_ID\n      token-secret: WIKI_TOKEN_SECRET\n",
		"      bot-token: WIKI_BOT_TOKEN\n", 1)
	_, err := Decode(strings.NewReader(mixed), testProviders)
	if err == nil || !strings.Contains(err.Error(),
		`connections.wiki: service "wiki" belongs to provider "bookstack", credential "reader" to "telegram"`) {
		t.Errorf("Decode() = %v, want it to name both sides of the mismatch", err)
	}

	// A credential that names no provider is not a mismatch: nothing places it anywhere yet.
	if _, err := Decode(strings.NewReader(minimal), testProviders); err != nil {
		t.Errorf("Decode() = %v, want a credential without a provider to stay valid", err)
	}
}
