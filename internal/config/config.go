// Package config loads and validates the callbell configuration and resolves the connection a command
// should use. It never reads, stores, or reports secret values: a credential only says where its secrets
// come from, either by naming environment variables or by pointing at the system credential store.
// Resolving a secret from that description is the job of package secret.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"
)

// Version is the only configuration schema version this build understands.
const Version = 1

// Config is the whole configuration file.
//
// The model is provider -> service -> connection -> credential: a service describes a technical API
// endpoint, a credential only the source of a secret, and a connection the selectable access context. That
// separation is what allows several instances of one provider, and several keys for one instance.
type Config struct {
	Version     int                   `yaml:"version"`
	Services    map[string]Service    `yaml:"services,omitempty"`
	Credentials map[string]Credential `yaml:"credentials,omitempty"`
	Connections map[string]Connection `yaml:"connections,omitempty"`
	Defaults    Defaults              `yaml:"defaults"`
	providers   ProviderCatalog       `yaml:"-"`
}

// Service is a technical API endpoint of one provider.
type Service struct {
	Provider string            `yaml:"provider"`
	BaseURL  string            `yaml:"base_url"`
	Options  map[string]string `yaml:"options,omitempty"`
}

// Credential names the source of the secrets a provider needs, never a secret itself.
//
// Type "env" maps every provider-defined secret role to the name of an environment variable in Values.
// That is the unchanged path for CI and headless use. Type "keyring" names nothing at all: its secrets
// live in the system credential store, and Values must stay empty, so this file cannot hold a secret even
// by accident.
//
// Provider is optional and names the system these secrets belong to. Secret roles are provider-defined, so
// without it nothing says whether a credential holds a bot token or a wiki token pair, and an editor can
// only offer the roles of every compiled provider at once. A credential written before this field, or by
// hand without it, stays valid and keeps that behaviour.
type Credential struct {
	Provider string            `yaml:"provider,omitempty"`
	Type     string            `yaml:"type"`
	Values   map[string]string `yaml:"values,omitempty"`
}

// Connection binds exactly one service to exactly one credential. Target is an optional provider-specific
// scope inside that service.
//
// Description is optional prose the user maintains. A name like "personal" or "crm-internal" is a stable
// selector, not an explanation, so this one line says what the route is for and lets a reader tell two
// routes of one provider apart. Discovery publishes it verbatim and nothing is ever sent to a provider,
// which is why it is configuration, never a secret and never personal data.
type Connection struct {
	Service     string `yaml:"service"`
	Credential  string `yaml:"credential"`
	Target      string `yaml:"target,omitempty"`
	Description string `yaml:"description,omitempty"`
}

// Defaults holds the connection chosen for a domain when no connection is given explicitly.
type Defaults struct {
	Connections map[string]string `yaml:"connections,omitempty"`
}

// The supported credential types.
const (
	// CredentialTypeEnv resolves from the environment variables the credential names.
	CredentialTypeEnv = "env"
	// CredentialTypeKeyring resolves from the system credential store, and from the plaintext fallback
	// beside this file when that fallback was switched on. Both are overridden by a derived environment
	// variable, so the same credential still works in a container.
	CredentialTypeKeyring = "keyring"
)

// CredentialTypes lists the supported credential types, in the order the documentation shows them.
func CredentialTypes() []string { return []string{CredentialTypeEnv, CredentialTypeKeyring} }

// NotFoundError reports that no configuration file exists at the resolved path.
type NotFoundError struct{ Path string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("no configuration file at %s", e.Path)
}

// InvalidError reports that a configuration file exists but does not satisfy the schema.
type InvalidError struct {
	Path string
	Err  error
}

func (e *InvalidError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }

func (e *InvalidError) Unwrap() error { return e.Err }

// Path returns the configuration file to use. An explicit path wins, then the file named by
// CALLBELL_CONFIG, then config.yaml inside the directory named by CALLBELL_CLI_HOME, then
// ~/.callbell/cli/config.yaml.
func Path(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if p := os.Getenv("CALLBELL_CONFIG"); p != "" {
		return p, nil
	}
	if dir := os.Getenv("CALLBELL_CLI_HOME"); dir != "" {
		return filepath.Join(dir, "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine the user home directory: %w", err)
	}
	return filepath.Join(home, ".callbell", "cli", "config.yaml"), nil
}

// Load reads and validates the configuration at path.
func Load(path string, providers ...ProviderCatalog) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &NotFoundError{Path: path}
		}
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	defer f.Close()

	cfg, err := Decode(f, providers...)
	if err != nil {
		return nil, &InvalidError{Path: path, Err: err}
	}
	return cfg, nil
}

// Decode reads a configuration from r. Unknown keys and duplicate keys are errors.
func Decode(r io.Reader, providers ...ProviderCatalog) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var cfg Config
	cfg.providers = providerCatalog(providers)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the configuration file is empty")
		}
		return nil, redactDecodeError(err)
	}
	// A second document would silently shadow the first one.
	if err := dec.Decode(new(Config)); !errors.Is(err, io.EOF) {
		return nil, errors.New("the configuration file must contain exactly one document")
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// A decoded configuration is directly editable, and an absent section round-trips to the same model
	// as an empty one.
	cfg.ensure()
	cfg.normalize()
	return &cfg, nil
}

// yamlLine matches the position the YAML library puts in front of a message it can locate.
var yamlLine = regexp.MustCompile(`^line (\d+): `)

// yamlProblems names the kinds of problem the library reports, keyed by a fragment of its message that
// carries no text from the file. The first match wins.
var yamlProblems = []struct{ match, problem string }{
	{" not found in type ", "unknown key"},
	{" already defined at line ", "duplicate key"},
	{"cannot unmarshal ", "a value does not have the type its key requires"},
	{"cannot decode ", "a value has an explicit type tag it does not satisfy"},
	{"unknown anchor ", "a reference to an anchor that is not defined"},
	{"value contains itself", "an anchor that refers to itself"},
	{"invalid map key", "an unusable key"},
}

// redactDecodeError replaces a decoder error with one that names the place and the kind of problem but
// never the text that caused it. The library quotes the offending key or value, and a user can type a
// secret in either place, so the whole message has to be rebuilt rather than filtered.
//
// The kinds are matched on message fragments; an unrecognized message keeps its position and loses its
// explanation. That is the deliberate trade against quoting the file. Extend the table when the library
// gains a message worth naming.
func redactDecodeError(err error) error {
	messages := []string{strings.TrimPrefix(err.Error(), "yaml: ")}
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		messages = typeErr.Errors
	}

	problems := make([]string, 0, len(messages))
	seen := make(map[string]bool, len(messages))
	for _, message := range messages {
		where := ""
		if at := yamlLine.FindStringSubmatch(message); at != nil {
			where = at[0]
			message = message[len(at[0]):]
		}
		problem := where + "the file does not parse as YAML"
		for _, known := range yamlProblems {
			if strings.Contains(message, known.match) {
				problem = where + known.problem
				break
			}
		}
		if !seen[problem] {
			seen[problem] = true
			problems = append(problems, problem)
		}
	}
	return errors.New(strings.Join(problems, "; "))
}

// Validate reports every schema and reference problem at once. Messages name configuration keys and
// environment variable names only, never secret values.
func (c *Config) Validate() error {
	providers := c.providerCatalog()
	var problems []error
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}
	// checkName reports an unusable name under its own key, so several bad names are all visible at once.
	checkName := func(section, kind, name string) {
		err := validateName(name)
		switch {
		case err == nil:
		case name == "":
			report("%s: a %s %v", section, kind, err)
		default:
			report("%s.%s: a %s %v", section, name, kind, err)
		}
	}

	if c.Version != Version {
		report("version: got %d, want %d", c.Version, Version)
	}

	for _, name := range sortedKeys(c.Services) {
		s := c.Services[name]
		checkName("services", "service name", name)
		if s.Provider == "" {
			report("services.%s: provider must not be empty", name)
		} else if _, ok := providers.ProviderMetadata(s.Provider); !ok {
			report("services.%s: unknown provider %q, known providers are %s",
				name, s.Provider, strings.Join(c.Providers(), ", "))
		}
		if err := validateBaseURL(s.BaseURL); err != nil {
			report("services.%s.base_url: %v", name, err)
		}
	}

	for _, name := range sortedKeys(c.Credentials) {
		cred := c.Credentials[name]
		checkName("credentials", "credential name", name)
		if cred.Provider != "" {
			if _, ok := providers.ProviderMetadata(cred.Provider); !ok {
				report("credentials.%s: unknown provider %q, known providers are %s",
					name, cred.Provider, strings.Join(c.Providers(), ", "))
			} else if roles := c.SecretRolesOf(cred.Provider); len(roles) > 0 {
				// A role the named provider does not define would never be read. Naming the provider is
				// what makes that visible here instead of at the first failing call.
				for _, role := range sortedKeys(cred.Values) {
					if !contains(roles, role) {
						report("credentials.%s.values.%s: %s does not define this secret role, it uses %s",
							name, role, cred.Provider, strings.Join(roles, ", "))
					}
				}
			}
		}
		switch cred.Type {
		case CredentialTypeEnv:
			if len(cred.Values) == 0 {
				report("credentials.%s.values: at least one secret role is required", name)
			}
			for _, role := range sortedKeys(cred.Values) {
				if role == "" {
					report("credentials.%s.values: a secret role must not be empty", name)
				}
				if err := validateEnvName(cred.Values[role]); err != nil {
					report("credentials.%s.values.%s: %v", name, role, err)
				}
			}
		case CredentialTypeKeyring:
			// The message never quotes what was written there: a value under a keyring credential is
			// most likely the secret itself, pasted into the file.
			if len(cred.Values) > 0 {
				report("credentials.%s.values: %s", name, keyringValuesRule)
			}
		default:
			report("credentials.%s: type must be one of %s, got %q",
				name, strings.Join(CredentialTypes(), ", "), cred.Type)
		}
	}

	for _, name := range sortedKeys(c.Connections) {
		conn := c.Connections[name]
		checkName("connections", "connection name", name)
		service, ok := c.Services[conn.Service]
		if !ok {
			report("connections.%s.service: unknown service %q", name, conn.Service)
		}
		cred, credOK := c.Credentials[conn.Credential]
		if !credOK {
			report("connections.%s.credential: unknown credential %q", name, conn.Credential)
		}
		// Only an env credential can be incomplete in the file. A keyring credential names no roles
		// here; which ones it must supply follows from the provider, and whether they are supplied is a
		// question about the credential store, not about this file.
		if ok && credOK && cred.Type == CredentialTypeEnv {
			for _, role := range c.ProviderSecretRoles(service.Provider) {
				if cred.Values[role] == "" {
					report("connections.%s: provider %q requires the secret role %q in credential %q",
						name, service.Provider, role, conn.Credential)
				}
			}
		}
		if err := validateDescription(conn.Description); err != nil {
			report("connections.%s.description: %v", name, err)
		}
		if ok {
			metadata, _ := providers.ProviderMetadata(service.Provider)
			if metadata.Target.Required && strings.TrimSpace(conn.Target) == "" {
				report("connections.%s.target: provider %q requires %s", name, service.Provider,
					metadata.Target.Label)
			}
		}
	}

	for _, domain := range sortedKeys(c.Defaults.Connections) {
		conn := c.Defaults.Connections[domain]
		checkName("defaults.connections", "domain", domain)
		if _, ok := c.Connections[conn]; !ok {
			report("defaults.connections.%s: unknown connection %q", domain, conn)
		}
	}

	return errors.Join(problems...)
}

func providerCatalog(providers []ProviderCatalog) ProviderCatalog {
	if len(providers) > 0 && providers[0] != nil {
		return providers[0]
	}
	return emptyProviderCatalog{}
}

func (c *Config) providerCatalog() ProviderCatalog {
	if c.providers != nil {
		return c.providers
	}
	return emptyProviderCatalog{}
}

// Providers returns the registered provider IDs in deterministic order.
func (c *Config) Providers() []string {
	metadata := c.providerCatalog().ProviderMetadataAll()
	providers := make([]string, len(metadata))
	for i := range metadata {
		providers[i] = metadata[i].ID
	}
	return providers
}

// ProviderMetadata returns one registered provider's configuration contract.
func (c *Config) ProviderMetadata(provider string) (ProviderMetadata, bool) {
	return c.providerCatalog().ProviderMetadata(provider)
}

// ProviderSecretRoles returns the roles one provider requires, in declaration order.
func (c *Config) ProviderSecretRoles(provider string) []string {
	metadata, _ := c.ProviderMetadata(provider)
	roles := make([]string, len(metadata.SecretRoles))
	for i := range metadata.SecretRoles {
		roles[i] = metadata.SecretRoles[i].Name
	}
	return roles
}

// SecretRoles returns all registered roles in deterministic order.
func (c *Config) SecretRoles() []string {
	roles := map[string]bool{}
	for _, metadata := range c.providerCatalog().ProviderMetadataAll() {
		for _, role := range metadata.SecretRoles {
			roles[role.Name] = true
		}
	}
	return sortedKeys(roles)
}

// SecretRolesOf returns the secret roles of one provider, sorted. An unknown provider has none.
func (c *Config) SecretRolesOf(provider string) []string {
	metadata, ok := c.providerCatalog().ProviderMetadata(provider)
	if !ok {
		return nil
	}
	roles := make([]string, 0, len(metadata.SecretRoles))
	for _, role := range metadata.SecretRoles {
		roles = append(roles, role.Name)
	}
	sort.Strings(roles)
	return roles
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// SecretRoleDescription returns the provider-authored help for a role.
func (c *Config) SecretRoleDescription(role string) string {
	for _, metadata := range c.providerCatalog().ProviderMetadataAll() {
		for _, candidate := range metadata.SecretRoles {
			if candidate.Name == role {
				return candidate.Description
			}
		}
	}
	return ""
}

func validateBaseURL(raw string) error {
	if raw == "" {
		return errors.New("must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	// http stays allowed so a local test server can be configured; providers enforce transport security.
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must use scheme http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("must contain a host")
	}
	return nil
}

// envNameRule states what a credential value must look like.
//
// The message never quotes the offending input. A user who pastes a secret into a credential field instead
// of the variable name would otherwise see that secret echoed in the editor and in `config validate`, and
// the redactor cannot catch it: it only knows values that were resolved from an environment variable.
const envNameRule = "must be the name of an environment variable, written with letters, digits and " +
	"underscores and not starting with a digit, never the secret value itself"

// keyringValuesRule states why a keyring credential carries no values. Like envNameRule it never quotes
// the input: whatever stands under a keyring credential is most likely the secret itself.
const keyringValuesRule = "must be absent for a keyring credential, whose secrets live in the credential " +
	"store; set them with 'callbell credential set'"

// maxDescriptionLength bounds a connection description. It labels a route for the person choosing one, it
// is not documentation, and discovery repeats it on every describe, so it stays short enough to read at a
// glance.
const maxDescriptionLength = 200

// descriptionRule states what a connection description must look like. Like the other rules it never
// quotes the input: a description is free text, and free text is exactly where a secret gets pasted by
// accident.
const descriptionRule = "a connection description must be a single line of at most 200 characters; " +
	"discovery publishes it, so it must never carry a secret or personal data"

// validateDescription refuses a description that is not one short line. An empty description is a missing
// one and needs no rule.
func validateDescription(text string) error {
	if strings.ContainsAny(text, "\n\r") {
		return errors.New("must not contain a line break: " + descriptionRule)
	}
	if length := utf8.RuneCountInString(normalizeDescription(text)); length > maxDescriptionLength {
		return fmt.Errorf("is %d characters long: %s", length, descriptionRule)
	}
	return nil
}

// normalizeDescription drops the blanks at the edges and collapses the runs between words, so the same
// sentence typed with different spacing is stored, measured and published as one text. A line break is
// deliberately not collapsed: validateDescription refuses it, because a description is one line.
func normalizeDescription(text string) string {
	return strings.Join(strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == '\t' }), " ")
}

// nameRule states the character set of every service, credential, connection and domain name. These names
// are configuration keys and `--connection` arguments, so they stay free of quoting and shell surprises.
const nameRule = "must consist of letters, digits, '-', '_' or '.' and must start and end with a letter or a digit"

func validateEnvName(name string) error {
	if name == "" {
		return errors.New("must name an environment variable")
	}
	for i, r := range name {
		digit := r >= '0' && r <= '9'
		letter := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		if letter || r == '_' || (digit && i > 0) {
			continue
		}
		return errors.New(envNameRule)
	}
	return nil
}

func validateName(name string) error {
	if name == "" {
		return errors.New("must not be empty")
	}
	for _, r := range name {
		if !isNameRune(r) {
			return errors.New(nameRule)
		}
	}
	// Every allowed rune is one ASCII byte, so the first and last byte are the first and last rune.
	if !isAlphanumeric(rune(name[0])) || !isAlphanumeric(rune(name[len(name)-1])) {
		return errors.New(nameRule)
	}
	return nil
}

func isNameRune(r rune) bool {
	return isAlphanumeric(r) || r == '-' || r == '_' || r == '.'
}

func isAlphanumeric(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
