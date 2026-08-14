package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	yaml "go.yaml.in/yaml/v3"
)

// File modes for the configuration. The file names environment variables rather than secrets, but it
// describes how to reach internal systems, so it stays private to the user.
const (
	fileMode = 0o600
	dirMode  = 0o700
)

// InUseError reports an entry that cannot be deleted because something still references it.
type InUseError struct {
	Kind string
	Name string
	By   []string
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("%s %q is still used by %v", e.Kind, e.Name, e.By)
}

// NotThereError reports an entry that does not exist.
type NotThereError struct {
	Kind string
	Name string
}

func (e *NotThereError) Error() string {
	return fmt.Sprintf("%s %q does not exist", e.Kind, e.Name)
}

// SecretRoles returns every secret role a known provider can require, sorted. A user interface asks for
// them here instead of knowing anything about providers itself.
func SecretRoles() []string {
	seen := map[string]bool{}
	for _, roles := range providerSecretRoles {
		for _, role := range roles {
			seen[role] = true
		}
	}
	return sortedKeys(seen)
}

// Providers returns the provider names a service may use, sorted.
func Providers() []string { return sortedKeys(providerSecretRoles) }

// Clone returns a deep copy, so a caller can try a change and discard it without touching the original.
func (c *Config) Clone() *Config {
	out := New()
	out.Version = c.Version
	for name, s := range c.Services {
		copied := s
		copied.Options = cloneStrings(s.Options)
		out.Services[name] = copied
	}
	for name, cred := range c.Credentials {
		copied := cred
		copied.Values = cloneStrings(cred.Values)
		out.Credentials[name] = copied
	}
	for name, conn := range c.Connections {
		out.Connections[name] = conn
	}
	if c.Defaults.Connections != nil {
		out.Defaults.Connections = cloneStrings(c.Defaults.Connections)
	}
	return out
}

func cloneStrings(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// New returns an empty configuration of the current schema version.
func New() *Config {
	return &Config{
		Version:     Version,
		Services:    map[string]Service{},
		Credentials: map[string]Credential{},
		Connections: map[string]Connection{},
		Defaults:    Defaults{Connections: map[string]string{}},
	}
}

// Store reads and writes one configuration file. A save either replaces the file completely or leaves it
// exactly as it was.
type Store struct {
	path string
	// marshal is a seam so a test can simulate an encoding failure.
	marshal func(any) ([]byte, error)
}

// NewStore returns a store for the configuration file at path.
func NewStore(path string) *Store {
	return &Store{path: path, marshal: yaml.Marshal}
}

// Path returns the file this store writes.
func (s *Store) Path() string { return s.path }

// Load reads the configuration through the same loader the CLI uses.
func (s *Store) Load() (*Config, error) { return Load(s.path) }

// Save validates the configuration, encodes it, checks that the encoded form loads back, and only then
// replaces the target file. Any failure before the replacement leaves the target byte-identical.
func (s *Store) Save(cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return &InvalidError{Path: s.path, Err: err}
	}

	data, err := s.marshal(cfg)
	if err != nil {
		return &InvalidError{Path: s.path, Err: fmt.Errorf("the configuration could not be encoded: %w", err)}
	}
	// What is written must load back, so an encoding defect can never reach the target file.
	if _, err := Decode(bytes.NewReader(data)); err != nil {
		return &InvalidError{Path: s.path, Err: fmt.Errorf("the encoded configuration is not loadable: %w", err)}
	}

	return s.replace(data)
}

// replace writes data next to the target and moves it into place. Both steps happen inside the target
// directory, so the move is atomic on the supported platforms.
func (s *Store) replace(data []byte) error {
	// A configuration kept in a dotfiles directory is often reached through a symlink. Renaming onto the
	// link would replace the link itself and orphan the real file, so the real file is the target.
	target := s.path
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}

	dir := filepath.Dir(target)
	// MkdirAll only applies the mode to directories it creates. A directory the user already had keeps
	// the permissions the user chose; changing them would be a side effect nobody asked for.
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".callbell-config-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", target, err)
	}
	name := tmp.Name()
	moved := false
	defer func() {
		if !moved {
			_ = os.Remove(name)
		}
	}()

	if err := writeAndSync(tmp, data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write %s: %w", name, err)
	}
	// CreateTemp already uses 0600; make it explicit so an umask cannot widen it.
	if err := os.Chmod(name, fileMode); err != nil {
		return fmt.Errorf("cannot set the permissions of %s: %w", name, err)
	}
	if err := os.Rename(name, target); err != nil {
		return fmt.Errorf("cannot replace %s: %w", target, err)
	}
	moved = true
	return nil
}

func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// SetService creates or replaces a service.
func (c *Config) SetService(name string, s Service) error {
	if name == "" {
		return errors.New("a service name must not be empty")
	}
	c.ensure()
	c.Services[name] = s
	return nil
}

// DeleteService removes a service that no connection uses.
func (c *Config) DeleteService(name string) error {
	if _, ok := c.Services[name]; !ok {
		return &NotThereError{Kind: "service", Name: name}
	}
	var by []string
	for _, conn := range sortedKeys(c.Connections) {
		if c.Connections[conn].Service == name {
			by = append(by, conn)
		}
	}
	if len(by) > 0 {
		return &InUseError{Kind: "service", Name: name, By: by}
	}
	delete(c.Services, name)
	return nil
}

// SetCredential creates or replaces a credential. Values name environment variables; a caller that passes
// a secret value instead of a name is rejected by validation, not stored.
func (c *Config) SetCredential(name string, cred Credential) error {
	if name == "" {
		return errors.New("a credential name must not be empty")
	}
	c.ensure()
	c.Credentials[name] = cred
	return nil
}

// DeleteCredential removes a credential that no connection uses.
func (c *Config) DeleteCredential(name string) error {
	if _, ok := c.Credentials[name]; !ok {
		return &NotThereError{Kind: "credential", Name: name}
	}
	var by []string
	for _, conn := range sortedKeys(c.Connections) {
		if c.Connections[conn].Credential == name {
			by = append(by, conn)
		}
	}
	if len(by) > 0 {
		return &InUseError{Kind: "credential", Name: name, By: by}
	}
	delete(c.Credentials, name)
	return nil
}

// SetConnection creates or replaces a connection.
func (c *Config) SetConnection(name string, conn Connection) error {
	if name == "" {
		return errors.New("a connection name must not be empty")
	}
	c.ensure()
	c.Connections[name] = conn
	return nil
}

// DeleteConnection removes a connection that no default points at.
func (c *Config) DeleteConnection(name string) error {
	if _, ok := c.Connections[name]; !ok {
		return &NotThereError{Kind: "connection", Name: name}
	}
	var by []string
	for _, domain := range sortedKeys(c.Defaults.Connections) {
		if c.Defaults.Connections[domain] == name {
			by = append(by, domain)
		}
	}
	if len(by) > 0 {
		return &InUseError{Kind: "connection", Name: name, By: by}
	}
	delete(c.Connections, name)
	return nil
}

// SetDefault points a domain at a connection.
func (c *Config) SetDefault(domain, connection string) error {
	if domain == "" {
		return errors.New("a domain must not be empty")
	}
	c.ensure()
	c.Defaults.Connections[domain] = connection
	return nil
}

// DeleteDefault removes the default of a domain.
func (c *Config) DeleteDefault(domain string) error {
	if _, ok := c.Defaults.Connections[domain]; !ok {
		return &NotThereError{Kind: "default", Name: domain}
	}
	delete(c.Defaults.Connections, domain)
	return nil
}

// ensure makes the maps usable on a configuration that was decoded without them.
func (c *Config) ensure() {
	if c.Services == nil {
		c.Services = map[string]Service{}
	}
	if c.Credentials == nil {
		c.Credentials = map[string]Credential{}
	}
	if c.Connections == nil {
		c.Connections = map[string]Connection{}
	}
	if c.Defaults.Connections == nil {
		c.Defaults.Connections = map[string]string{}
	}
}
