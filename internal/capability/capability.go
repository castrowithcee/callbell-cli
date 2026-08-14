// Package capability holds the provider-independent capability contracts and the registry that binds them
// to provider types. It performs no I/O and produces no output format: it answers what a connection can
// do, in a deterministic order, so an encoder can render the answer.
package capability

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// RiskLevel classifies what a capability does to the remote system. More levels follow after the MVP; the
// executable MVP registers read capabilities only.
type RiskLevel string

// RiskRead marks a capability that only reads.
const RiskRead RiskLevel = "read"

// Argument is one input of a capability.
type Argument struct {
	Name        string
	Description string
	Required    bool
}

// Field is one value a capability returns.
type Field struct {
	Name        string
	Description string
}

// Capability is the contract of one domain operation, for example knowledge.pages.list. It is
// provider-independent: several providers may implement the same contract, and several connections of one
// provider share it instead of declaring it again.
type Capability struct {
	Name        string
	Description string
	Risk        RiskLevel
	Arguments   []Argument
	Fields      []Field
}

// ConflictError reports that two providers registered the same capability name with different contracts.
type ConflictError struct {
	Name     string
	Provider string
	Existing string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("provider %q registers capability %q with a contract that differs from provider %q",
		e.Provider, e.Name, e.Existing)
}

// Registry maps provider types to the capabilities they implement.
type Registry struct {
	byProvider map[string]map[string]Capability
	owner      map[string]string // capability name -> provider that registered it first
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byProvider: map[string]map[string]Capability{},
		owner:      map[string]string{},
	}
}

// Register records the capabilities a provider type implements. Registering the same name twice is allowed
// as long as the contracts are identical; a differing contract is an error. Either the whole call is
// recorded or none of it.
func (r *Registry) Register(provider string, caps ...Capability) error {
	if provider == "" {
		return fmt.Errorf("a provider name must not be empty")
	}

	staged := make([]Capability, 0, len(caps))
	batch := make(map[string]Capability, len(caps))
	for _, c := range caps {
		if err := c.validate(); err != nil {
			return fmt.Errorf("provider %q: %w", provider, err)
		}
		c = c.clone()

		known, from := c, ""
		if earlier, ok := batch[c.Name]; ok {
			known, from = earlier, provider
		} else if owner, ok := r.owner[c.Name]; ok {
			known, from = r.byProvider[owner][c.Name], owner
		}
		if from != "" && !reflect.DeepEqual(known, c) {
			return &ConflictError{Name: c.Name, Provider: provider, Existing: from}
		}

		batch[c.Name] = c
		staged = append(staged, c)
	}

	for _, c := range staged {
		if _, ok := r.owner[c.Name]; !ok {
			r.owner[c.Name] = provider
		}
		if r.byProvider[provider] == nil {
			r.byProvider[provider] = map[string]Capability{}
		}
		r.byProvider[provider][c.Name] = c
	}
	return nil
}

// Provider returns the capabilities of one provider type, sorted by name. The result is a copy, so a
// consumer cannot alter what the registry answers next.
func (r *Registry) Provider(provider string) []Capability {
	return sorted(r.byProvider[provider])
}

// clone deep-copies a capability and normalises empty slices to nil, so that two providers declaring "no
// arguments" in different ways still count as the same contract.
func (c Capability) clone() Capability {
	out := c
	out.Arguments, out.Fields = nil, nil
	if len(c.Arguments) > 0 {
		out.Arguments = append([]Argument(nil), c.Arguments...)
	}
	if len(c.Fields) > 0 {
		out.Fields = append([]Field(nil), c.Fields...)
	}
	return out
}

func (c Capability) validate() error {
	if c.Name == "" {
		return fmt.Errorf("a capability name must not be empty")
	}
	if c.Risk != RiskRead {
		return fmt.Errorf("capability %q: risk level %q is not available in this build, want %q",
			c.Name, c.Risk, RiskRead)
	}
	if err := uniqueNames("argument", len(c.Arguments), func(i int) string { return c.Arguments[i].Name }); err != nil {
		return fmt.Errorf("capability %q: %w", c.Name, err)
	}
	if err := uniqueNames("field", len(c.Fields), func(i int) string { return c.Fields[i].Name }); err != nil {
		return fmt.Errorf("capability %q: %w", c.Name, err)
	}
	return nil
}

func uniqueNames(kind string, n int, name func(int) string) error {
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		switch {
		case name(i) == "":
			return fmt.Errorf("an %s name must not be empty", kind)
		case seen[name(i)]:
			return fmt.Errorf("%s %q is declared twice", kind, name(i))
		}
		seen[name(i)] = true
	}
	return nil
}

func sorted(m map[string]Capability) []Capability {
	out := make([]Capability, 0, len(m))
	for _, c := range m {
		out = append(out, c.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Summary is the short machine-readable form of a capability, used by the output layer.
func (c Capability) Summary() string {
	required := make([]string, 0, len(c.Arguments))
	for _, a := range c.Arguments {
		if a.Required {
			required = append(required, a.Name)
		}
	}
	return fmt.Sprintf("%s %s(%s)", c.Risk, c.Name, strings.Join(required, ","))
}
