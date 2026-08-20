package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/capability"
	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

func TestSearchAndDescribeAreLocalAndDeterministic(t *testing.T) {
	core, calls := testCore(t, []string{"primary", "archive"}, nil, true)

	got, err := core.Search(SearchRequest{Query: "page read", Provider: "fake", Effect: capability.EffectRead})
	if err != nil {
		t.Fatalf("Search() = %v", err)
	}
	if len(got.Operations) != 1 || got.Operations[0].ID != "fake.pages.get" {
		t.Fatalf("Search() = %+v", got)
	}
	if want := []string{"archive", "primary"}; !reflect.DeepEqual(got.Operations[0].Connections, want) {
		t.Errorf("connections = %v, want %v", got.Operations[0].Connections, want)
	}

	described, err := core.Describe(DescribeRequest{Operation: "fake.pages.get", Version: 1})
	if err != nil {
		t.Fatalf("Describe() = %v", err)
	}
	if described.Operation.InputSchema == nil || described.Operation.OutputSchema == nil {
		t.Errorf("Describe() omits schemas: %+v", described.Operation)
	}
	if !reflect.DeepEqual(described.Connections, []string{"archive", "primary"}) {
		t.Errorf("connections = %v", described.Connections)
	}
	if *calls != 0 {
		t.Errorf("handlers called = %d, want 0", *calls)
	}
}

func TestInvokeConnectionSelection(t *testing.T) {
	tests := []struct {
		name        string
		connections []string
		defaults    map[string]string
		explicit    string
		want        string
		wantErr     any
	}{
		{name: "explicit", connections: []string{"primary", "archive"}, explicit: "archive", want: "archive"},
		{name: "operation default", connections: []string{"primary", "archive"}, defaults: map[string]string{"fake.pages.get": "archive"}, want: "archive"},
		{name: "provider default", connections: []string{"primary", "archive"}, defaults: map[string]string{"fake": "primary"}, want: "primary"},
		{name: "same operation and provider default", connections: []string{"primary", "archive"}, defaults: map[string]string{"fake": "primary", "fake.pages.get": "primary"}, want: "primary"},
		{name: "only matching", connections: []string{"primary"}, want: "primary"},
		{name: "no matching connection", wantErr: new(ConnectionSelectionError)},
		{name: "ambiguous", connections: []string{"primary", "archive"}, wantErr: new(ConnectionAmbiguousError)},
		{name: "conflicting defaults", connections: []string{"primary", "archive"}, defaults: map[string]string{"fake": "primary", "fake.pages.get": "archive"}, wantErr: new(ConnectionAmbiguousError)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, _ := testCore(t, tt.connections, tt.defaults, true)
			got, err := core.Invoke(context.Background(), InvokeRequest{
				Operation: "fake.pages.get", Connection: tt.explicit,
				Arguments: json.RawMessage(`{"id":"42"}`),
			})
			if tt.wantErr != nil {
				if !errorAs(err, tt.wantErr) {
					t.Fatalf("Invoke() error = %T %v, want %T", err, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Invoke() = %v", err)
			}
			if got.Connection != tt.want {
				t.Errorf("connection = %q, want %q", got.Connection, tt.want)
			}
		})
	}
}

func TestInvokeStopsBeforeProviderIO(t *testing.T) {
	tests := []struct {
		name        string
		request     InvokeRequest
		setup       func(*Core)
		connections []string
		withValue   bool
		wantErr     any
	}{
		{
			name:        "invalid arguments precede route selection",
			request:     InvokeRequest{Operation: "fake.pages.get", Arguments: json.RawMessage(`{"id":7}`)},
			connections: []string{"primary", "archive"},
			withValue:   true, wantErr: new(InvalidRequestError),
		},
		{
			name:      "missing confirmation",
			request:   InvokeRequest{Operation: "fake.pages.delete", Arguments: json.RawMessage(`{"id":"7"}`)},
			withValue: true, wantErr: new(ConfirmationRequiredError),
		},
		{
			name:    "policy denial",
			request: InvokeRequest{Operation: "fake.pages.get", Arguments: json.RawMessage(`{"id":"7"}`)},
			setup: func(core *Core) {
				core.SetPolicy(func(context.Context, InvokeRequest, capability.Descriptor, *config.Resolved) error {
					return errors.New("private policy detail")
				})
			},
			withValue: true, wantErr: new(PolicyDeniedError),
		},
		{
			name:    "missing credential",
			request: InvokeRequest{Operation: "fake.pages.get", Arguments: json.RawMessage(`{"id":"7"}`)},
			wantErr: new(secret.MissingSecretError),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connections := tt.connections
			if len(connections) == 0 {
				connections = []string{"primary"}
			}
			core, calls := testCore(t, connections, nil, tt.withValue)
			if tt.setup != nil {
				tt.setup(core)
			}
			_, err := core.Invoke(context.Background(), tt.request)
			if !errorAs(err, tt.wantErr) {
				t.Fatalf("Invoke() error = %T %v, want %T", err, err, tt.wantErr)
			}
			if *calls != 0 {
				t.Errorf("provider calls = %d, want 0", *calls)
			}
		})
	}
}

func TestInputSchemaErrorsAreDeterministic(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"string"}},"additionalProperties":false}`)
	arguments := json.RawMessage(`{"b":7,"a":"wrong"}`)

	const want = "$.a must be integer"
	for i := 0; i < 100; i++ {
		err := validateJSON(schema, arguments)
		if err == nil || err.Error() != want {
			t.Fatalf("run %d error = %v, want %q", i+1, err, want)
		}
	}
}

func TestInvokeDispatchesReadAndConfirmedMutation(t *testing.T) {
	core, calls := testCore(t, []string{"primary"}, nil, true)
	requests := []InvokeRequest{
		{Operation: "fake.pages.get", Arguments: json.RawMessage(`{"id":"7"}`)},
		{Operation: "fake.pages.delete", Arguments: json.RawMessage(`{"id":"7"}`), Confirmed: true},
	}
	for _, request := range requests {
		response, err := core.Invoke(context.Background(), request)
		if err != nil {
			t.Fatalf("Invoke(%s) = %v", request.Operation, err)
		}
		var result map[string]any
		if err := json.Unmarshal(response.Result, &result); err != nil || result["ok"] != true {
			t.Errorf("result = %s (%v)", response.Result, err)
		}
	}
	if *calls != 2 {
		t.Errorf("provider calls = %d, want 2", *calls)
	}
}

func TestExplicitConnectionAndConfirmationPrecedeSecretsIOAndAudit(t *testing.T) {
	secretReads, handlerCalls := 0, 0
	descriptor := testDescriptor("fake.messages.send", capability.EffectCreate, capability.ConfirmationRequired)
	descriptor.RequiresExplicitConnection = true
	descriptor.InputSchema = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","minLength":1,"maxLength":4}},"required":["id"],"additionalProperties":false}`)
	handler := capability.Handler(func(_ context.Context, resolved *config.Resolved, resolver *secret.Resolver,
		_ *redact.Redactor, _ json.RawMessage) (any, error) {
		handlerCalls++
		if _, err := resolver.Resolve(resolved.Credential, resolved.Secrets, "token"); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
	registry := capability.NewRegistry()
	if err := registry.Register("fake", capability.Operation{Descriptor: descriptor, Handler: handler}); err != nil {
		t.Fatal(err)
	}
	cfg := config.New()
	cfg.Services["service"] = config.Service{Provider: "fake", BaseURL: "https://example.invalid"}
	cfg.Credentials["sender"] = config.Credential{
		Type: config.CredentialTypeEnv, Values: map[string]string{"token": "FAKE_TOKEN"},
	}
	cfg.Connections["alerts"] = config.Connection{Service: "service", Credential: "sender"}
	cfg.Defaults.Connections["fake"] = "alerts"
	resolver := secret.NewWith(func(string) string { secretReads++; return "test-token" }, nil, nil, nil)
	core := New(registry, cfg, resolver, nil)
	var audit bytes.Buffer
	core.SetAudit(&audit)

	tests := []struct {
		name    string
		request InvokeRequest
		wantErr any
	}{
		{
			name: "invalid schema",
			request: InvokeRequest{Operation: descriptor.ID, Connection: "alerts", Confirmed: true,
				Arguments: json.RawMessage(`{"id":"12345"}`)},
			wantErr: new(InvalidRequestError),
		},
		{
			name: "default is ignored",
			request: InvokeRequest{Operation: descriptor.ID, Confirmed: true,
				Arguments: json.RawMessage(`{"id":"1"}`)},
			wantErr: new(ConnectionSelectionError),
		},
		{
			name: "confirmation missing",
			request: InvokeRequest{Operation: descriptor.ID, Connection: "alerts",
				Arguments: json.RawMessage(`{"id":"1"}`)},
			wantErr: new(ConfirmationRequiredError),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := core.Invoke(context.Background(), tt.request)
			if !errorAs(err, tt.wantErr) {
				t.Fatalf("Invoke() = %T %v, want %T", err, err, tt.wantErr)
			}
		})
	}
	core.SetPolicy(func(context.Context, InvokeRequest, capability.Descriptor, *config.Resolved) error {
		return errors.New("private policy detail")
	})
	_, err := core.Invoke(context.Background(), InvokeRequest{
		Operation: descriptor.ID, Connection: "alerts", Confirmed: true,
		Arguments: json.RawMessage(`{"id":"1"}`),
	})
	if !errorAs(err, new(PolicyDeniedError)) {
		t.Fatalf("Invoke(policy denied) = %T %v, want *PolicyDeniedError", err, err)
	}
	core.SetPolicy(nil)
	if secretReads != 0 || handlerCalls != 0 || audit.Len() != 0 {
		t.Fatalf("before confirmed explicit dispatch: secret reads=%d handler calls=%d audit=%q",
			secretReads, handlerCalls, audit.String())
	}

	response, err := core.Invoke(context.Background(), InvokeRequest{
		Operation: descriptor.ID, Connection: "alerts", Confirmed: true,
		Arguments: json.RawMessage(`{"id":"1"}`),
	})
	if err != nil {
		t.Fatalf("Invoke(confirmed) = %v", err)
	}
	if response.Connection != "alerts" || secretReads != 1 || handlerCalls != 1 {
		t.Fatalf("response=%+v secret reads=%d handler calls=%d", response, secretReads, handlerCalls)
	}
	var event struct {
		RequestID  string `json:"request_id"`
		Operation  string `json:"operation"`
		Connection string `json:"connection"`
		Result     string `json:"result"`
		Confirmed  bool   `json:"confirmed"`
		Time       string `json:"time"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(audit.Bytes()), &event); err != nil {
		t.Fatalf("audit = %q: %v", audit.String(), err)
	}
	if len(event.RequestID) != 32 || event.Operation != descriptor.ID || event.Connection != "alerts" ||
		!event.Confirmed || event.Result != "success" || event.Time == "" {
		t.Fatalf("audit event = %+v", event)
	}
	if strings.Contains(audit.String(), `"id":"1"`) || strings.Contains(audit.String(), "test-token") {
		t.Fatalf("audit contains arguments or secret: %s", audit.String())
	}
}

func TestInvokeRejectsOutputOutsideTheDescriptor(t *testing.T) {
	registry := capability.NewRegistry()
	descriptor := testDescriptor("fake.pages.get", capability.EffectRead, capability.ConfirmationNone)
	handler := capability.Handler(func(context.Context, *config.Resolved, *secret.Resolver,
		*redact.Redactor, json.RawMessage) (any, error) {
		return map[string]any{"unexpected": true}, nil
	})
	if err := registry.Register("fake", capability.Operation{Descriptor: descriptor, Handler: handler}); err != nil {
		t.Fatal(err)
	}
	cfg := config.New()
	cfg.Services["fake"] = config.Service{Provider: "fake", BaseURL: "https://example.invalid"}
	cfg.Credentials["reader"] = config.Credential{Type: config.CredentialTypeKeyring}
	cfg.Connections["primary"] = config.Connection{Service: "fake", Credential: "reader"}

	_, err := New(registry, cfg, nil, nil).Invoke(context.Background(), InvokeRequest{
		Operation: "fake.pages.get", Arguments: json.RawMessage(`{"id":"7"}`),
	})
	var invalid *InvalidProviderResponseError
	if !errors.As(err, &invalid) {
		t.Fatalf("Invoke() error = %T %v, want *InvalidProviderResponseError", err, err)
	}
}

func TestInvokeValidatesTheRedactedOutput(t *testing.T) {
	const canary = "registered-secret-value"

	tests := []struct {
		name       string
		allowed    string
		wantErr    bool
		wantResult string
	}{
		{name: "redacted value satisfies schema", allowed: redact.Marker, wantResult: redact.Marker},
		{name: "redaction can invalidate schema", allowed: canary, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := capability.NewRegistry()
			descriptor := testDescriptor("fake.pages.get", capability.EffectRead, capability.ConfirmationNone)
			descriptor.OutputSchema = json.RawMessage(`{"type":"object","properties":{"value":{"type":"string","enum":[` +
				strconv.Quote(tt.allowed) + `]}},"required":["value"],"additionalProperties":false}`)
			handler := capability.Handler(func(context.Context, *config.Resolved, *secret.Resolver,
				*redact.Redactor, json.RawMessage) (any, error) {
				return map[string]any{"value": canary}, nil
			})
			if err := registry.Register("fake", capability.Operation{Descriptor: descriptor, Handler: handler}); err != nil {
				t.Fatal(err)
			}
			cfg := config.New()
			cfg.Services["fake"] = config.Service{Provider: "fake", BaseURL: "https://example.invalid"}
			cfg.Credentials["reader"] = config.Credential{Type: config.CredentialTypeKeyring}
			cfg.Connections["primary"] = config.Connection{Service: "fake", Credential: "reader"}
			redactor := &redact.Redactor{}
			redactor.Add(canary)

			response, err := New(registry, cfg, nil, redactor).Invoke(context.Background(), InvokeRequest{
				Operation: "fake.pages.get", Arguments: json.RawMessage(`{"id":"7"}`),
			})
			if tt.wantErr {
				var invalid *InvalidProviderResponseError
				if !errors.As(err, &invalid) {
					t.Fatalf("Invoke() error = %T %v, want *InvalidProviderResponseError", err, err)
				}
				if strings.Contains(err.Error(), canary) {
					t.Errorf("error leaks secret: %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Invoke() = %v", err)
			}
			var result map[string]string
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatal(err)
			}
			if result["value"] != tt.wantResult || strings.Contains(string(response.Result), canary) {
				t.Errorf("result = %s", response.Result)
			}
		})
	}
}

func testCore(t *testing.T, connections []string, defaults map[string]string, withValue bool) (*Core, *int) {
	t.Helper()
	calls := new(int)
	handler := capability.Handler(func(_ context.Context, resolved *config.Resolved, resolver *secret.Resolver,
		_ *redact.Redactor, _ json.RawMessage) (any, error) {
		if _, err := resolver.Resolve(resolved.Credential, resolved.Secrets, "token"); err != nil {
			return nil, err
		}
		*calls++
		return map[string]any{"ok": true}, nil
	})
	read := testDescriptor("fake.pages.get", capability.EffectRead, capability.ConfirmationNone)
	mutate := testDescriptor("fake.pages.delete", capability.EffectDelete, capability.ConfirmationRequired)
	registry := capability.NewRegistry()
	if err := registry.Register("fake",
		capability.Operation{Descriptor: read, Handler: handler},
		capability.Operation{Descriptor: mutate, Handler: handler},
	); err != nil {
		t.Fatal(err)
	}

	cfg := config.New()
	cfg.Defaults.Connections = defaults
	for _, name := range connections {
		service := name + "-service"
		credential := name + "-credential"
		cfg.Services[service] = config.Service{Provider: "fake", BaseURL: "https://example.invalid"}
		cfg.Credentials[credential] = config.Credential{
			Type: config.CredentialTypeEnv, Values: map[string]string{"token": "FAKE_TOKEN"},
		}
		cfg.Connections[name] = config.Connection{Service: service, Credential: credential}
	}
	env := func(name string) string {
		if withValue && name == "FAKE_TOKEN" {
			return "test-token"
		}
		return ""
	}
	redactor := &redact.Redactor{}
	resolver := secret.NewWith(env, nil, nil, redactor)
	return New(registry, cfg, resolver, redactor), calls
}

func testDescriptor(id string, effect capability.Effect, confirmation capability.Confirmation) capability.Descriptor {
	return capability.Descriptor{
		ID: id, Version: 1, Title: "Read page", Description: "Read one page", Tags: []string{"page", "read"},
		Provider: "fake",
		Risk: capability.Risk{Effect: effect, Idempotency: capability.IdempotencySafe,
			Confirmation: confirmation, DataSensitivity: "test"},
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","minLength":1}},"required":["id"],"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
	}
}

func errorAs(err error, target any) bool {
	switch target.(type) {
	case *InvalidRequestError:
		var typed *InvalidRequestError
		return errors.As(err, &typed)
	case *ConnectionAmbiguousError:
		var typed *ConnectionAmbiguousError
		return errors.As(err, &typed)
	case *ConnectionSelectionError:
		var typed *ConnectionSelectionError
		return errors.As(err, &typed)
	case *ConfirmationRequiredError:
		var typed *ConfirmationRequiredError
		return errors.As(err, &typed)
	case *PolicyDeniedError:
		var typed *PolicyDeniedError
		return errors.As(err, &typed)
	case *secret.MissingSecretError:
		var typed *secret.MissingSecretError
		return errors.As(err, &typed)
	default:
		return false
	}
}
