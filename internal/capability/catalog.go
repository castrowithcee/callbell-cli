package capability

import "fmt"

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
