package config

import "fmt"

// Resolved is the fully dereferenced access context of one connection. EnvNames maps a provider-defined
// secret role to the environment variable that carries the secret; the value itself is never resolved here.
type Resolved struct {
	Name       string
	Provider   string
	BaseURL    string
	Options    map[string]string
	Target     string
	Service    string
	Credential string
	EnvNames   map[string]string
}

// SelectionError reports that no single connection could be determined for a domain. It is a usage
// problem, not a runtime failure.
type SelectionError struct {
	Domain string
	Name   string // set when an explicitly requested connection does not exist
}

func (e *SelectionError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("unknown connection %q", e.Name)
	}
	return fmt.Sprintf("no connection selected for domain %q: pass --connection or set defaults.connections.%s",
		e.Domain, e.Domain)
}

// Resolve returns the connection to use for a domain. An explicitly requested name wins; otherwise the
// domain default decides. Resolution is purely local and never contacts a provider.
func (c *Config) Resolve(name, domain string) (*Resolved, error) {
	if name == "" {
		name = c.Defaults.Connections[domain]
	}
	if name == "" {
		return nil, &SelectionError{Domain: domain}
	}

	conn, ok := c.Connections[name]
	if !ok {
		return nil, &SelectionError{Domain: domain, Name: name}
	}
	// Validate guarantees both references exist.
	service := c.Services[conn.Service]
	cred := c.Credentials[conn.Credential]

	return &Resolved{
		Name:       name,
		Provider:   service.Provider,
		BaseURL:    service.BaseURL,
		Options:    service.Options,
		Target:     conn.Target,
		Service:    conn.Service,
		Credential: conn.Credential,
		EnvNames:   cred.Values,
	}, nil
}
