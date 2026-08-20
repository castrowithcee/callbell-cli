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

// UnsupportedError reports that an operation is not offered. Connection is empty when no configured
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
// provider ID; a provider that no connection uses contributes nothing.
type Catalog struct {
	registry    *Registry
	connections map[string]string
}

// NewCatalog binds a registry to the configured connections.
func NewCatalog(registry *Registry, connections map[string]string) *Catalog {
	return &Catalog{registry: registry, connections: connections}
}

// List returns the operations of one connection, or the deduplicated union of all configured connections
// when connection is empty. The result is sorted by ID and stable across calls.
func (c *Catalog) List(connection string) ([]Descriptor, error) {
	if connection != "" {
		provider, ok := c.connections[connection]
		if !ok {
			return nil, &UnknownConnectionError{Name: connection}
		}
		return c.registry.Provider(provider), nil
	}

	// Provider-qualified IDs let every connection of one provider share one descriptor.
	union := map[string]Descriptor{}
	for _, name := range sortedKeys(c.connections) {
		for _, operation := range c.registry.Provider(c.connections[name]) {
			union[operation.ID] = operation
		}
	}
	return sorted(union), nil
}

// Describe returns one operation descriptor, filtered the same way as List.
func (c *Catalog) Describe(connection, id string) (Descriptor, error) {
	operations, err := c.List(connection)
	if err != nil {
		return Descriptor{}, err
	}
	for _, operation := range operations {
		if operation.ID == id {
			return operation, nil
		}
	}
	return Descriptor{}, &UnsupportedError{Connection: connection, Capability: id}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
