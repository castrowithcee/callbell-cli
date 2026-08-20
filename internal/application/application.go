// Package application implements the provider-independent Search, Describe, and Invoke use cases.
package application

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/output"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

const maxSearchResults = 50

// Core owns the configured, provider-independent operations surface.
type Core struct {
	registry *capability.Registry
	config   *config.Config
	secrets  *secret.Resolver
	redactor *redact.Redactor
	policy   Policy
}

// Policy may reject a fully validated request after connection selection and before confirmation,
// credential resolution, or provider I/O. A nil policy allows the request.
type Policy func(context.Context, InvokeRequest, capability.Descriptor, *config.Resolved) error

// New returns an application core over one validated configuration.
func New(registry *capability.Registry, cfg *config.Config, secrets *secret.Resolver, redactor *redact.Redactor) *Core {
	return &Core{registry: registry, config: cfg, secrets: secrets, redactor: redactor}
}

// SetPolicy installs the policy used by subsequent invocations.
func (c *Core) SetPolicy(policy Policy) { c.policy = policy }

// SearchRequest filters the local operation catalog. Limit is capped even when omitted or non-positive.
type SearchRequest struct {
	Query      string            `json:"query,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Connection string            `json:"connection,omitempty"`
	Effect     capability.Effect `json:"effect,omitempty"`
	Limit      int               `json:"limit,omitempty"`
}

// SearchHit is the bounded discovery view of one descriptor.
type SearchHit struct {
	ID          string            `json:"id"`
	Version     int               `json:"version"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Tags        []string          `json:"tags"`
	Provider    string            `json:"provider"`
	Effect      capability.Effect `json:"effect"`
	Connections []string          `json:"connections"`
}

// SearchResponse is the payload inside the CLI envelope.
type SearchResponse struct {
	Operations []SearchHit `json:"operations"`
}

// Search performs deterministic local discovery and never resolves credentials or calls a provider.
func (c *Core) Search(request SearchRequest) (SearchResponse, error) {
	if request.Effect != "" && !validEffect(request.Effect) {
		return SearchResponse{}, &InvalidRequestError{Message: fmt.Sprintf("unknown effect %q", request.Effect)}
	}

	var selectedProvider string
	if request.Connection != "" {
		resolved, err := c.connection(request.Connection)
		if err != nil {
			return SearchResponse{}, err
		}
		selectedProvider = resolved.Provider
	}

	limit := request.Limit
	if limit <= 0 || limit > maxSearchResults {
		limit = maxSearchResults
	}
	terms := strings.Fields(strings.ToLower(request.Query))
	hits := make([]SearchHit, 0)
	for _, descriptor := range c.registry.All() {
		if request.Provider != "" && descriptor.Provider != request.Provider {
			continue
		}
		if selectedProvider != "" && descriptor.Provider != selectedProvider {
			continue
		}
		if request.Effect != "" && descriptor.Risk.Effect != request.Effect {
			continue
		}
		haystack := strings.ToLower(strings.Join(append([]string{
			descriptor.ID, descriptor.Title, descriptor.Description,
		}, descriptor.Tags...), " "))
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		title := descriptor.Title
		if title == "" {
			title = descriptor.Description
		}
		hits = append(hits, SearchHit{
			ID: descriptor.ID, Version: descriptor.Version, Title: title,
			Description: descriptor.Description, Tags: nonNilStrings(descriptor.Tags),
			Provider: descriptor.Provider, Effect: descriptor.Risk.Effect,
			Connections: c.connectionNames(descriptor.Provider),
		})
		if len(hits) == limit {
			break
		}
	}
	return SearchResponse{Operations: hits}, nil
}

// DescribeRequest selects exactly one versioned descriptor. Connection only restricts its possible routes.
type DescribeRequest struct {
	Operation  string `json:"operation"`
	Version    int    `json:"version,omitempty"`
	Connection string `json:"connection,omitempty"`
}

// DescribeResponse is the complete operation contract and its possible configured routes.
type DescribeResponse struct {
	Operation   capability.Descriptor `json:"operation"`
	Connections []string              `json:"connections"`
}

// Describe returns one registered descriptor without contacting a provider.
func (c *Core) Describe(request DescribeRequest) (DescribeResponse, error) {
	descriptor, _, err := c.operation(request.Operation, request.Version)
	if err != nil {
		return DescribeResponse{}, err
	}
	connections := c.connectionNames(descriptor.Provider)
	if request.Connection != "" {
		resolved, err := c.connection(request.Connection)
		if err != nil {
			return DescribeResponse{}, err
		}
		if resolved.Provider != descriptor.Provider {
			return DescribeResponse{}, &capability.UnsupportedError{
				Connection: request.Connection, Capability: request.Operation,
			}
		}
		connections = []string{request.Connection}
	}
	return DescribeResponse{Operation: descriptor, Connections: connections}, nil
}

// InvokeRequest is one direct, request-bound invocation. Confirmed is consumed only by this value and is
// never persisted or reused.
type InvokeRequest struct {
	Operation  string          `json:"operation"`
	Version    int             `json:"version,omitempty"`
	Connection string          `json:"connection,omitempty"`
	Arguments  json.RawMessage `json:"arguments"`
	Confirmed  bool            `json:"confirm,omitempty"`
}

// InvokeResponse records the exact operation contract and route that produced Result.
type InvokeResponse struct {
	Operation  string          `json:"operation"`
	Version    int             `json:"version"`
	Connection string          `json:"connection"`
	Result     json.RawMessage `json:"result"`
}

// Invoke validates arguments before route selection, then applies policy and confirmation before
// dispatch. Provider output is normalized and validated before it can reach the caller.
func (c *Core) Invoke(ctx context.Context, request InvokeRequest) (InvokeResponse, error) {
	descriptor, rawHandler, err := c.operation(request.Operation, request.Version)
	if err != nil {
		return InvokeResponse{}, err
	}
	if len(request.Arguments) == 0 {
		request.Arguments = json.RawMessage(`{}`)
	}
	if err := validateJSON(descriptor.InputSchema, request.Arguments); err != nil {
		return InvokeResponse{}, &InvalidRequestError{Message: err.Error()}
	}

	resolved, err := c.selectConnection(request.Connection, descriptor)
	if err != nil {
		return InvokeResponse{}, err
	}
	if c.policy != nil {
		if err := c.policy(ctx, request, descriptor, resolved); err != nil {
			return InvokeResponse{}, &PolicyDeniedError{Operation: descriptor.ID}
		}
	}
	if descriptor.Risk.Confirmation == capability.ConfirmationRequired && !request.Confirmed {
		return InvokeResponse{}, &ConfirmationRequiredError{Operation: descriptor.ID}
	}

	handler := rawHandler
	if handler == nil {
		return InvokeResponse{}, fmt.Errorf("operation %q has an invalid handler", descriptor.ID)
	}
	value, err := handler(ctx, resolved, c.secrets, c.redactor, request.Arguments)
	if err != nil {
		return InvokeResponse{}, err
	}
	normalized, err := normalize(value)
	if err != nil {
		return InvokeResponse{}, &InvalidProviderResponseError{Operation: descriptor.ID}
	}
	normalized = redactValue(c.redactor, normalized)
	if err := validateValue(descriptor.OutputSchema, normalized); err != nil {
		return InvokeResponse{}, &InvalidProviderResponseError{Operation: descriptor.ID}
	}
	result, err := json.Marshal(normalized)
	if err != nil {
		return InvokeResponse{}, &InvalidProviderResponseError{Operation: descriptor.ID}
	}
	return InvokeResponse{
		Operation: descriptor.ID, Version: descriptor.Version, Connection: resolved.Name, Result: result,
	}, nil
}

func (c *Core) operation(id string, version int) (capability.Descriptor, capability.Handler, error) {
	if id == "" {
		return capability.Descriptor{}, nil, &InvalidRequestError{Message: "operation must not be empty"}
	}
	descriptor, handler, ok := c.registry.Lookup(id)
	if !ok || (version != 0 && descriptor.Version != version) {
		return capability.Descriptor{}, nil, &UnknownOperationError{Operation: id, Version: version}
	}
	return descriptor, handler, nil
}

func (c *Core) selectConnection(explicit string, descriptor capability.Descriptor) (*config.Resolved, error) {
	if explicit != "" {
		resolved, err := c.connection(explicit)
		if err != nil {
			return nil, err
		}
		if resolved.Provider != descriptor.Provider {
			return nil, &capability.UnsupportedError{Connection: explicit, Capability: descriptor.ID}
		}
		return resolved, nil
	}

	defaults := map[string]bool{}
	for _, key := range []string{descriptor.ID, descriptor.Provider} {
		if name := c.config.Defaults.Connections[key]; name != "" {
			defaults[name] = true
		}
	}
	if len(defaults) > 1 {
		return nil, &ConnectionAmbiguousError{Operation: descriptor.ID, Connections: sortedSet(defaults)}
	}
	for name := range defaults {
		resolved, err := c.connection(name)
		if err != nil {
			return nil, err
		}
		if resolved.Provider != descriptor.Provider {
			return nil, &capability.UnsupportedError{Connection: name, Capability: descriptor.ID}
		}
		return resolved, nil
	}

	connections := c.connectionNames(descriptor.Provider)
	switch len(connections) {
	case 0:
		return nil, &ConnectionSelectionError{Operation: descriptor.ID}
	case 1:
		return c.connection(connections[0])
	default:
		return nil, &ConnectionAmbiguousError{Operation: descriptor.ID, Connections: connections}
	}
}

func (c *Core) connection(name string) (*config.Resolved, error) {
	connection, ok := c.config.Connections[name]
	if !ok {
		return nil, &capability.UnknownConnectionError{Name: name}
	}
	service := c.config.Services[connection.Service]
	return &config.Resolved{
		Name: name, Provider: service.Provider, BaseURL: service.BaseURL, Options: service.Options,
		Target: connection.Target, Service: connection.Service, Credential: connection.Credential,
		Secrets: c.config.Credentials[connection.Credential],
	}, nil
}

func (c *Core) connectionNames(provider string) []string {
	names := make([]string, 0)
	for name, connection := range c.config.Connections {
		if c.config.Services[connection.Service].Provider == provider {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func validEffect(effect capability.Effect) bool {
	switch effect {
	case capability.EffectRead, capability.EffectCreate, capability.EffectUpdate,
		capability.EffectDelete, capability.EffectExecute:
		return true
	}
	return false
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return append([]string(nil), values...)
}

func sortedSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func normalize(value any) (any, error) {
	switch result := value.(type) {
	case output.Collection:
		rows := make([]any, len(result.Rows))
		for i, row := range result.Rows {
			object := make(map[string]any, len(result.Columns))
			for _, column := range result.Columns {
				object[column] = row[column]
			}
			rows[i] = object
		}
		value = rows
	case output.Object:
		object := make(map[string]any, len(result.Fields))
		for _, field := range result.Fields {
			object[field.Name] = field.Value
		}
		value = object
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func redactValue(redactor *redact.Redactor, value any) any {
	if redactor == nil {
		return value
	}
	switch value := value.(type) {
	case string:
		return redactor.Apply(value)
	case []any:
		for i := range value {
			value[i] = redactValue(redactor, value[i])
		}
	case map[string]any:
		for key := range value {
			value[key] = redactValue(redactor, value[key])
		}
	}
	return value
}
