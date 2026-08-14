// Package config loads and validates the callbell configuration and resolves the connection a command
// should use. It never reads, stores, or reports secret values: a credential only names the environment
// variables that carry them.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
}

// Service is a technical API endpoint of one provider.
type Service struct {
	Provider string            `yaml:"provider"`
	BaseURL  string            `yaml:"base_url"`
	Options  map[string]string `yaml:"options,omitempty"`
}

// Credential names the source of the secrets a provider needs. Only the type "env" exists; Values maps a
// provider-defined secret role to the name of an environment variable, never to its value.
type Credential struct {
	Type   string            `yaml:"type"`
	Values map[string]string `yaml:"values"`
}

// Connection binds exactly one service to exactly one credential. Target is an optional provider-specific
// scope inside that service.
type Connection struct {
	Service    string `yaml:"service"`
	Credential string `yaml:"credential"`
	Target     string `yaml:"target,omitempty"`
}

// Defaults holds the connection chosen for a domain when no connection is given explicitly.
type Defaults struct {
	Connections map[string]string `yaml:"connections,omitempty"`
}

// CredentialTypeEnv is the only supported credential type.
const CredentialTypeEnv = "env"

// providerSecretRoles lists the secret roles every provider requires. Roles are provider-defined: a
// provider authenticating with a single bearer token needs one role, BookStack needs two.
//
// callbell-dev: the table lives here because config validation is the only consumer today; it moves to the
// provider registry once providers register themselves. Only providers that actually exist belong here, so
// a configuration that validates can also run.
var providerSecretRoles = map[string][]string{
	"bookstack": {"token-id", "token-secret"},
}

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

// Path returns the configuration file to use. An explicit path wins, then CALLBELL_CONFIG, then
// <user config dir>/callbell/config.yaml.
func Path(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if p := os.Getenv("CALLBELL_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine the user configuration directory: %w", err)
	}
	return filepath.Join(dir, "callbell", "config.yaml"), nil
}

// Load reads and validates the configuration at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &NotFoundError{Path: path}
		}
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	defer f.Close()

	cfg, err := Decode(f)
	if err != nil {
		return nil, &InvalidError{Path: path, Err: err}
	}
	return cfg, nil
}

// Decode reads a configuration from r. Unknown keys and duplicate keys are errors.
func Decode(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the configuration file is empty")
		}
		return nil, err
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
	return &cfg, nil
}

// Validate reports every schema and reference problem at once. Messages name configuration keys and
// environment variable names only, never secret values.
func (c *Config) Validate() error {
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
		} else if _, ok := providerSecretRoles[s.Provider]; !ok {
			report("services.%s: unknown provider %q, known providers are %s",
				name, s.Provider, strings.Join(sortedKeys(providerSecretRoles), ", "))
		}
		if err := validateBaseURL(s.BaseURL); err != nil {
			report("services.%s.base_url: %v", name, err)
		}
	}

	for _, name := range sortedKeys(c.Credentials) {
		cred := c.Credentials[name]
		checkName("credentials", "credential name", name)
		if cred.Type != CredentialTypeEnv {
			report("credentials.%s: type must be %q, got %q", name, CredentialTypeEnv, cred.Type)
		}
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
		if ok && credOK {
			for _, role := range providerSecretRoles[service.Provider] {
				if cred.Values[role] == "" {
					report("connections.%s: provider %q requires the secret role %q in credential %q",
						name, service.Provider, role, conn.Credential)
				}
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
