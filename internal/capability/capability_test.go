package capability

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/redact"
	"github.com/castrowithcee/callbell-cli/internal/secret"
)

var (
	testRisk = Risk{
		Effect:          EffectRead,
		Idempotency:     IdempotencySafe,
		Confirmation:    ConfirmationNone,
		DataSensitivity: "test-data",
	}
	pagesList = Descriptor{
		ID:           "fakewiki.pages.list",
		Version:      1,
		Description:  "List pages",
		Risk:         testRisk,
		Provider:     "fakewiki",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"array"}`),
		Arguments: []Argument{
			{Name: "limit", Description: "Maximum number of pages"},
			{Name: "offset", Description: "Number of pages to skip"},
		},
		Fields: []Field{{Name: "id"}, {Name: "name"}},
	}
	pagesGet = Descriptor{
		ID:           "fakewiki.pages.get",
		Version:      1,
		Description:  "Read one page",
		Risk:         testRisk,
		Provider:     "fakewiki",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		Arguments:    []Argument{{Name: "id", Description: "Page identifier", Required: true}},
		Fields:       []Field{{Name: "id"}, {Name: "html"}},
	}
	tickets = Descriptor{
		ID:           "faketracker.issues.list",
		Version:      1,
		Description:  "List issues",
		Risk:         testRisk,
		Provider:     "faketracker",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"array"}`),
		Fields:       []Field{{Name: "id"}},
	}
)

func operation(d Descriptor) Operation { return Operation{Descriptor: d, Handler: noopHandler} }

func noopHandler(context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor, json.RawMessage) (any, error) {
	return nil, nil
}

func mustRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.Register("fakewiki", operation(pagesList), operation(pagesGet)); err != nil {
		t.Fatalf("Register(fakewiki) = %v", err)
	}
	if err := reg.Register("faketracker", operation(tickets)); err != nil {
		t.Fatalf("Register(faketracker) = %v", err)
	}
	return reg
}

func TestRegisterRejectsInvalidOperations(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		operation Operation
		wantIn    string
	}{
		{"empty provider", "", operation(pagesList), "provider ID must not be empty"},
		{"empty operation ID", "fakewiki", operation(Descriptor{}), "operation ID must not be empty"},
		{
			"uppercase provider ID", "Fakewiki",
			operation(withID(withProvider(pagesList, "Fakewiki"), "Fakewiki.pages.list")),
			"provider ID \"Fakewiki\" must match",
		},
		{"provider mismatch", "fakewiki", operation(withProvider(pagesList, "other")), "want registered provider"},
		{"two segments", "fakewiki", operation(withID(pagesList, "pages.list")), "exactly three segments"},
		{"four segments", "fakewiki", operation(withID(pagesList, "fakewiki.knowledge.pages.list")), "exactly three segments"},
		{"uppercase object", "fakewiki", operation(withID(pagesList, "fakewiki.Pages.list")), "must match [a-z][a-z0-9]*"},
		{"space in object", "fakewiki", operation(withID(pagesList, "fakewiki.page list.get")), "must match [a-z][a-z0-9]*"},
		{"special character", "fakewiki", operation(withID(pagesList, "fakewiki.pages.get-all")), "must match [a-z][a-z0-9]*"},
		{"empty ID segment", "fakewiki", operation(withID(pagesList, "fakewiki..list")), "must match [a-z][a-z0-9]*"},
		{"wrong provider prefix", "fakewiki", operation(withID(pagesList, "other.pages.list")), "provider prefix"},
		{"prefix lookalike", "fakewiki", operation(withID(pagesList, "fakewikix.pages.list")), "provider prefix"},
		{"missing version", "fakewiki", operation(withVersion(pagesList, 0)), "version must be positive"},
		{"missing description", "fakewiki", operation(withDescription(pagesList, "")), "description must not be empty"},
		{"missing effect", "fakewiki", operation(withEffect(pagesList, "")), `effect "" must be one of`},
		{"invalid effect", "fakewiki", operation(withEffect(pagesList, Effect("write"))), `effect "write" must be one of`},
		{"missing idempotency", "fakewiki", operation(withIdempotency(pagesList, "")), `idempotency "" must be one of`},
		{"invalid idempotency", "fakewiki", operation(withIdempotency(pagesList, Idempotency("sometimes"))), `idempotency "sometimes" must be one of`},
		{"missing confirmation", "fakewiki", operation(withConfirmation(pagesList, "")), `confirmation "" must be one of`},
		{"invalid confirmation", "fakewiki", operation(withConfirmation(pagesList, Confirmation("optional"))), `confirmation "optional" must be one of`},
		{"empty data sensitivity", "fakewiki", operation(withDataSensitivity(pagesList, "")), "data sensitivity must not be empty"},
		{"blank data sensitivity", "fakewiki", operation(withDataSensitivity(pagesList, " \t")), "data sensitivity must not be empty"},
		{"missing input schema", "fakewiki", operation(withInputSchema(pagesList, nil)), "input schema must be valid JSON"},
		{"invalid input schema", "fakewiki", operation(withInputSchema(pagesList, json.RawMessage(`{"type":`))), "input schema must be valid JSON"},
		{"non-object input schema", "fakewiki", operation(withInputSchema(pagesList, json.RawMessage(`[]`))), "input schema must be a JSON object"},
		{"missing output schema", "fakewiki", operation(withOutputSchema(pagesList, nil)), "output schema must be valid JSON"},
		{
			"duplicate argument", "fakewiki",
			operation(withArguments(pagesList, []Argument{{Name: "x"}, {Name: "x"}})),
			`argument "x" is declared twice`,
		},
		{
			"duplicate field", "fakewiki",
			operation(withFields(pagesList, []Field{{Name: "x"}, {Name: "x"}})),
			`field "x" is declared twice`,
		},
		{
			"empty field name", "fakewiki",
			operation(withFields(pagesList, []Field{{}})),
			"field name must not be empty",
		},
		{"missing handler", "fakewiki", Operation{Descriptor: pagesList}, "must have a handler"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewRegistry().Register(tt.provider, tt.operation)
			if err == nil {
				t.Fatal("Register() = nil, want an error")
			}
			if got := err.Error(); !strings.Contains(got, tt.wantIn) {
				t.Errorf("error = %q, want it to contain %q", got, tt.wantIn)
			}
		})
	}
}

func TestRegisterRejectsDuplicateIDsAndVersionConflicts(t *testing.T) {
	t.Run("same version is a duplicate", func(t *testing.T) {
		reg := NewRegistry()
		if err := reg.Register("fakewiki", operation(pagesList)); err != nil {
			t.Fatalf("Register() = %v", err)
		}
		err := reg.Register("fakewiki", operation(pagesList))

		var duplicate *DuplicateError
		if !errors.As(err, &duplicate) {
			t.Fatalf("Register() = %v, want a *DuplicateError", err)
		}
		if duplicate.ID != pagesList.ID || duplicate.Version != pagesList.Version {
			t.Errorf("duplicate = %+v", duplicate)
		}
	})

	t.Run("different version is a conflict", func(t *testing.T) {
		reg := NewRegistry()
		if err := reg.Register("fakewiki", operation(pagesList)); err != nil {
			t.Fatalf("Register() = %v", err)
		}
		err := reg.Register("fakewiki", operation(withVersion(pagesList, 2)))

		var conflict *VersionConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("Register() = %v, want a *VersionConflictError", err)
		}
		if conflict.ID != pagesList.ID || conflict.Existing != 1 || conflict.Incoming != 2 {
			t.Errorf("conflict = %+v", conflict)
		}
	})

	t.Run("a duplicate inside one call is rejected atomically", func(t *testing.T) {
		reg := NewRegistry()
		err := reg.Register("fakewiki", operation(pagesGet), operation(pagesList), operation(pagesGet))
		var duplicate *DuplicateError
		if !errors.As(err, &duplicate) {
			t.Fatalf("Register() = %v, want a *DuplicateError", err)
		}
		if got := reg.Provider("fakewiki"); len(got) != 0 {
			t.Errorf("provider holds %v, want nothing after a rejected call", ids(got))
		}
	})

	t.Run("a conflict with registered state rejects the whole call", func(t *testing.T) {
		reg := NewRegistry()
		if err := reg.Register("fakewiki", operation(pagesList)); err != nil {
			t.Fatalf("Register() = %v", err)
		}
		other := pagesGet
		other.ID = "fakewiki.pages.other"

		err := reg.Register("fakewiki", operation(other), operation(withVersion(pagesList, 2)))
		var conflict *VersionConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("Register() = %v, want a *VersionConflictError", err)
		}
		if _, _, ok := reg.Lookup(other.ID); ok {
			t.Errorf("operation %q was recorded by a rejected call", other.ID)
		}
	})

	t.Run("rejections are byte-identical across registries", func(t *testing.T) {
		var first string
		for i := 0; i < 20; i++ {
			reg := NewRegistry()
			if err := reg.Register("fakewiki", operation(pagesList)); err != nil {
				t.Fatalf("Register() = %v", err)
			}
			got := reg.Register("fakewiki", operation(withVersion(pagesList, 2))).Error()
			if i == 0 {
				first = got
			} else if got != first {
				t.Fatalf("run %d error = %q, want %q", i+1, got, first)
			}
		}
	})
}

func TestRegistryOwnsDescriptorsAndHandlers(t *testing.T) {
	t.Run("lookup returns the registered handler", func(t *testing.T) {
		handler := Handler(func(context.Context, *config.Resolved, *secret.Resolver, *redact.Redactor, json.RawMessage) (any, error) {
			return "handled", nil
		})
		reg := NewRegistry()
		if err := reg.Register("fakewiki", Operation{Descriptor: pagesList, Handler: handler}); err != nil {
			t.Fatalf("Register() = %v", err)
		}
		descriptor, got, ok := reg.Lookup(pagesList.ID)
		if !ok || descriptor.ID != pagesList.ID || reflect.ValueOf(got).Pointer() != reflect.ValueOf(handler).Pointer() {
			t.Errorf("Lookup() = (%+v, %v, %v)", descriptor, got, ok)
		}
	})

	t.Run("mutating input and answers does not change registry state", func(t *testing.T) {
		const (
			inputJSON  = `{"type":"object","title":"original input"}`
			outputJSON = `{"type":"array","title":"original output"}`
		)
		inputSchema := json.RawMessage(inputJSON)
		outputSchema := json.RawMessage(outputJSON)
		arguments := []Argument{{Name: "limit", Description: "original"}}
		descriptor := pagesList
		descriptor.InputSchema = inputSchema
		descriptor.OutputSchema = outputSchema
		descriptor.Arguments = arguments
		reg := NewRegistry()
		if err := reg.Register("fakewiki", operation(descriptor)); err != nil {
			t.Fatalf("Register() = %v", err)
		}
		inputSchema[2] = 'X'
		outputSchema[2] = 'X'
		arguments[0].Description = "mutated"

		first := reg.Provider("fakewiki")
		first[0].InputSchema[2] = 'Y'
		first[0].Arguments[0].Name = "mutated"
		lookedUp, _, ok := reg.Lookup(descriptor.ID)
		if !ok {
			t.Fatal("Lookup() did not find the registered operation")
		}
		lookedUp.OutputSchema[2] = 'Z'
		lookedUp.Fields[0].Name = "mutated"

		second := reg.Provider("fakewiki")[0]
		if string(second.InputSchema) != inputJSON || string(second.OutputSchema) != outputJSON ||
			second.Arguments[0].Description != "original" || second.Arguments[0].Name == "mutated" ||
			second.Fields[0].Name == "mutated" {
			t.Errorf("registry returned mutated state: %+v", second)
		}
	})

	t.Run("unknown ID is absent", func(t *testing.T) {
		if _, _, ok := NewRegistry().Lookup("fakewiki.pages.absent"); ok {
			t.Fatal("Lookup() found an unregistered ID")
		}
	})
}

func TestSummary(t *testing.T) {
	tests := []struct {
		name string
		d    Descriptor
		want string
	}{
		{"required arguments only", pagesGet, "read fakewiki.pages.get(id)"},
		{"no required arguments", pagesList, "read fakewiki.pages.list()"},
		{"no arguments", tickets, "read faketracker.issues.list()"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.d.Summary(); got != tt.want {
				t.Errorf("Summary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func ids(descriptors []Descriptor) []string {
	out := make([]string, len(descriptors))
	for i, descriptor := range descriptors {
		out[i] = descriptor.ID
	}
	return out
}

func withID(d Descriptor, id string) Descriptor        { d.ID = id; return d }
func withVersion(d Descriptor, version int) Descriptor { d.Version = version; return d }
func withDescription(d Descriptor, description string) Descriptor {
	d.Description = description
	return d
}
func withEffect(d Descriptor, effect Effect) Descriptor { d.Risk.Effect = effect; return d }
func withIdempotency(d Descriptor, idempotency Idempotency) Descriptor {
	d.Risk.Idempotency = idempotency
	return d
}
func withConfirmation(d Descriptor, confirmation Confirmation) Descriptor {
	d.Risk.Confirmation = confirmation
	return d
}
func withDataSensitivity(d Descriptor, sensitivity string) Descriptor {
	d.Risk.DataSensitivity = sensitivity
	return d
}
func withProvider(d Descriptor, provider string) Descriptor { d.Provider = provider; return d }
func withInputSchema(d Descriptor, schema json.RawMessage) Descriptor {
	d.InputSchema = schema
	return d
}
func withOutputSchema(d Descriptor, schema json.RawMessage) Descriptor {
	d.OutputSchema = schema
	return d
}
func withArguments(d Descriptor, arguments []Argument) Descriptor { d.Arguments = arguments; return d }
func withFields(d Descriptor, fields []Field) Descriptor          { d.Fields = fields; return d }
