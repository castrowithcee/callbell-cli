// Package capability holds operation descriptors and the registry that binds them to provider handlers.
// It performs no I/O and produces no output format: it answers what a connection can do, in a
// deterministic order, so an encoder can render the answer.
package capability

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Effect classifies what an operation does to the remote system.
type Effect string

const (
	EffectRead    Effect = "read"
	EffectCreate  Effect = "create"
	EffectUpdate  Effect = "update"
	EffectDelete  Effect = "delete"
	EffectExecute Effect = "execute"
)

// Idempotency classifies whether repeating an operation is safe.
type Idempotency string

const (
	IdempotencySafe          Idempotency = "safe"
	IdempotencyIdempotent    Idempotency = "idempotent"
	IdempotencyNonIdempotent Idempotency = "non_idempotent"
	IdempotencyUnknown       Idempotency = "unknown"
)

// Confirmation declares whether an operation needs explicit confirmation.
type Confirmation string

const (
	ConfirmationNone     Confirmation = "none"
	ConfirmationRequired Confirmation = "required"
)

// Risk is the complete safety contract of an operation. DataSensitivity is provider-owned because the
// architecture defines no global classification taxonomy.
type Risk struct {
	Effect          Effect
	Idempotency     Idempotency
	Confirmation    Confirmation
	OpenWorld       bool
	DataSensitivity string
}

// Argument is CLI discovery metadata for one named input. InputSchema is the operation contract.
type Argument struct {
	Name        string
	Description string
	Required    bool
}

// Field is CLI projection metadata for one named result value. OutputSchema is the operation contract.
type Field struct {
	Name        string
	Description string
}

// Descriptor is the versioned contract of one provider operation, for example bookstack.pages.list.
// Schemas use JSON Schema's object form. Every configured connection of Provider shares the descriptor.
type Descriptor struct {
	ID           string
	Version      int
	Description  string
	Risk         Risk
	Provider     string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Arguments    []Argument
	Fields       []Field
}

// Operation binds a descriptor to its provider handler. Handler stays opaque until an invocation surface
// exists; the registry only owns and returns it.
type Operation struct {
	Descriptor Descriptor
	Handler    any
}

// DuplicateError reports an operation ID and version that were already registered.
type DuplicateError struct {
	ID      string
	Version int
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("operation %q version %d is already registered", e.ID, e.Version)
}

// VersionConflictError reports an operation ID registered with two versions.
type VersionConflictError struct {
	ID       string
	Existing int
	Incoming int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("operation %q is already registered at version %d, cannot register version %d",
		e.ID, e.Existing, e.Incoming)
}

// Registry maps provider-qualified operation IDs to their descriptors and handlers.
type Registry struct {
	byID       map[string]Operation
	byProvider map[string]map[string]Descriptor
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byID:       map[string]Operation{},
		byProvider: map[string]map[string]Descriptor{},
	}
}

// Register records the operations implemented by one registered provider ID. Operation IDs must use that
// provider as their first segment. Duplicate IDs are rejected, including identical registrations, and a
// second version receives a distinct conflict error. Either the whole call is recorded or none of it.
func (r *Registry) Register(provider string, operations ...Operation) error {
	if provider == "" {
		return fmt.Errorf("a provider ID must not be empty")
	}
	if !validSegment(provider) {
		return fmt.Errorf("provider ID %q must match [a-z][a-z0-9]*", provider)
	}

	staged := make([]Operation, 0, len(operations))
	batch := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		d := operation.Descriptor
		if err := d.validate(provider); err != nil {
			return fmt.Errorf("provider %q: %w", provider, err)
		}
		if operation.Handler == nil {
			return fmt.Errorf("provider %q: operation %q must have a handler", provider, d.ID)
		}
		operation.Descriptor = d.clone()

		known, ok := batch[d.ID]
		if !ok {
			known, ok = r.byID[d.ID]
		}
		if ok {
			if known.Descriptor.Version != d.Version {
				return &VersionConflictError{
					ID: d.ID, Existing: known.Descriptor.Version, Incoming: d.Version,
				}
			}
			return &DuplicateError{ID: d.ID, Version: d.Version}
		}

		batch[d.ID] = operation
		staged = append(staged, operation)
	}

	for _, operation := range staged {
		d := operation.Descriptor
		r.byID[d.ID] = operation
		if r.byProvider[provider] == nil {
			r.byProvider[provider] = map[string]Descriptor{}
		}
		r.byProvider[provider][d.ID] = d
	}
	return nil
}

// Provider returns the descriptors of one provider type, sorted by ID. The result is a copy, so a
// consumer cannot alter what the registry answers next.
func (r *Registry) Provider(provider string) []Descriptor {
	return sorted(r.byProvider[provider])
}

// Lookup returns one registered descriptor and its handler.
func (r *Registry) Lookup(id string) (Descriptor, any, bool) {
	operation, ok := r.byID[id]
	if !ok {
		return Descriptor{}, nil, false
	}
	return operation.Descriptor.clone(), operation.Handler, true
}

// clone deep-copies a descriptor, including its JSON schema bytes, and normalises empty slices to nil.
func (d Descriptor) clone() Descriptor {
	out := d
	out.InputSchema = append(json.RawMessage(nil), d.InputSchema...)
	out.OutputSchema = append(json.RawMessage(nil), d.OutputSchema...)
	out.Arguments, out.Fields = nil, nil
	if len(d.Arguments) > 0 {
		out.Arguments = append([]Argument(nil), d.Arguments...)
	}
	if len(d.Fields) > 0 {
		out.Fields = append([]Field(nil), d.Fields...)
	}
	return out
}

func (d Descriptor) validate(provider string) error {
	if d.ID == "" {
		return fmt.Errorf("an operation ID must not be empty")
	}
	if d.Provider != provider {
		return fmt.Errorf("operation %q declares provider %q, want registered provider %q",
			d.ID, d.Provider, provider)
	}
	segments := strings.Split(d.ID, ".")
	if len(segments) != 3 {
		return fmt.Errorf("operation ID %q must have exactly three segments: provider.object.action", d.ID)
	}
	for _, segment := range segments {
		if !validSegment(segment) {
			return fmt.Errorf("operation ID %q: segment %q must match [a-z][a-z0-9]*", d.ID, segment)
		}
	}
	if segments[0] != provider {
		return fmt.Errorf("operation ID %q has provider prefix %q, want registered provider %q",
			d.ID, segments[0], provider)
	}
	if d.Version <= 0 {
		return fmt.Errorf("operation %q: version must be positive", d.ID)
	}
	if d.Description == "" {
		return fmt.Errorf("operation %q: description must not be empty", d.ID)
	}
	if err := d.Risk.validate(d.ID); err != nil {
		return err
	}
	if err := validateSchema("input", d.ID, d.InputSchema); err != nil {
		return err
	}
	if err := validateSchema("output", d.ID, d.OutputSchema); err != nil {
		return err
	}
	if err := uniqueNames("argument", len(d.Arguments), func(i int) string { return d.Arguments[i].Name }); err != nil {
		return fmt.Errorf("operation %q: %w", d.ID, err)
	}
	if err := uniqueNames("field", len(d.Fields), func(i int) string { return d.Fields[i].Name }); err != nil {
		return fmt.Errorf("operation %q: %w", d.ID, err)
	}
	return nil
}

func (r Risk) validate(id string) error {
	switch r.Effect {
	case EffectRead, EffectCreate, EffectUpdate, EffectDelete, EffectExecute:
	default:
		return fmt.Errorf("operation %q: effect %q must be one of read, create, update, delete, execute",
			id, r.Effect)
	}
	switch r.Idempotency {
	case IdempotencySafe, IdempotencyIdempotent, IdempotencyNonIdempotent, IdempotencyUnknown:
	default:
		return fmt.Errorf("operation %q: idempotency %q must be one of safe, idempotent, non_idempotent, unknown",
			id, r.Idempotency)
	}
	switch r.Confirmation {
	case ConfirmationNone, ConfirmationRequired:
	default:
		return fmt.Errorf("operation %q: confirmation %q must be one of none, required", id, r.Confirmation)
	}
	if strings.TrimSpace(r.DataSensitivity) == "" {
		return fmt.Errorf("operation %q: data sensitivity must not be empty", id)
	}
	return nil
}

func validSegment(segment string) bool {
	for i := 0; i < len(segment); i++ {
		b := segment[i]
		if b >= 'a' && b <= 'z' {
			continue
		}
		if i > 0 && b >= '0' && b <= '9' {
			continue
		}
		return false
	}
	return segment != ""
}

func validateSchema(kind, id string, schema json.RawMessage) error {
	if !json.Valid(schema) {
		return fmt.Errorf("operation %q: %s schema must be valid JSON", id, kind)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(schema, &object); err != nil || object == nil {
		return fmt.Errorf("operation %q: %s schema must be a JSON object", id, kind)
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

func sorted(m map[string]Descriptor) []Descriptor {
	out := make([]Descriptor, 0, len(m))
	for _, d := range m {
		out = append(out, d.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Summary is the short machine-readable form of an operation, used by the output layer.
func (d Descriptor) Summary() string {
	required := make([]string, 0, len(d.Arguments))
	for _, argument := range d.Arguments {
		if argument.Required {
			required = append(required, argument.Name)
		}
	}
	return fmt.Sprintf("%s %s(%s)", d.Risk.Effect, d.ID, strings.Join(required, ","))
}
