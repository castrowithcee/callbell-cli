package capability

import (
	"fmt"
	"sort"
)

// UnknownConnectionError reports a connection that is not configured.
type UnknownConnectionError struct{ Name string }

func (e *UnknownConnectionError) Error() string {
	return fmt.Sprintf("unknown connection %q", e.Name)
}

// UnsupportedError reports that a capability is not offered. Connection is empty when no configured
// connection offers it at all.
type UnsupportedError struct {
	Connection string
	Capability string
}

func (e *UnsupportedError) Error() string {
	if e.Connection == "" {
		return fmt.Sprintf("no configured connection offers capability %q", e.Capability)
	}
	return fmt.Sprintf("connection %q does not offer capability %q", e.Connection, e.Capability)
}

// Catalog answers what the configured connections can do. Connections map a connection name to its
// provider type; a provider that no connection uses contributes nothing.
type Catalog struct {
	registry    *Registry
	connections map[string]string
}

// NewCatalog binds a registry to the configured connections.
func NewCatalog(registry *Registry, connections map[string]string) *Catalog {
	return &Catalog{registry: registry, connections: connections}
}

// List returns the capabilities of one connection, or the deduplicated union of all configured
// connections when connection is empty. The result is sorted by name and stable across calls.
func (c *Catalog) List(connection string) ([]Capability, error) {
	if connection != "" {
		provider, ok := c.connections[connection]
		if !ok {
			return nil, &UnknownConnectionError{Name: connection}
		}
		return c.registry.Provider(provider), nil
	}

	// Registration guarantees that one name maps to one contract, so the union deduplicates by name.
	union := map[string]Capability{}
	for _, name := range sortedKeys(c.connections) {
		for _, cap := range c.registry.Provider(c.connections[name]) {
			union[cap.Name] = cap
		}
	}
	return sorted(union), nil
}

// Describe returns one capability, filtered the same way as List.
func (c *Catalog) Describe(connection, name string) (Capability, error) {
	caps, err := c.List(connection)
	if err != nil {
		return Capability{}, err
	}
	for _, cap := range caps {
		if cap.Name == name {
			return cap, nil
		}
	}
	return Capability{}, &UnsupportedError{Connection: connection, Capability: name}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
