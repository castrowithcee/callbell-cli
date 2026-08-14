package capability

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Two fake providers stand in for real ones. pagesList is a domain contract both of them implement.
var (
	pagesList = Capability{
		Name:        "knowledge.pages.list",
		Description: "List pages",
		Risk:        RiskRead,
		Arguments: []Argument{
			{Name: "limit", Description: "Maximum number of pages"},
			{Name: "offset", Description: "Number of pages to skip"},
		},
		Fields: []Field{{Name: "id"}, {Name: "name"}},
	}
	pagesGet = Capability{
		Name:        "knowledge.pages.get",
		Description: "Read one page",
		Risk:        RiskRead,
		Arguments:   []Argument{{Name: "id", Description: "Page identifier", Required: true}},
		Fields:      []Field{{Name: "id"}, {Name: "html"}},
	}
	tickets = Capability{
		Name:        "tickets.issues.list",
		Description: "List issues",
		Risk:        RiskRead,
		Fields:      []Field{{Name: "id"}},
	}
)

func mustRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.Register("fake-wiki", pagesList, pagesGet); err != nil {
		t.Fatalf("Register(fake-wiki) = %v", err)
	}
	if err := reg.Register("fake-tracker", tickets); err != nil {
		t.Fatalf("Register(fake-tracker) = %v", err)
	}
	return reg
}

func TestRegisterRejects(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		cap      Capability
		wantIn   string
	}{
		{"empty provider", "", pagesList, "provider name must not be empty"},
		{"empty capability name", "fake", Capability{Risk: RiskRead}, "capability name must not be empty"},
		{"missing risk level", "fake", Capability{Name: "a"}, "risk level"},
		{
			name: "mutating risk level is not available yet", provider: "fake",
			cap:    Capability{Name: "a", Risk: RiskLevel("write")},
			wantIn: `risk level "write" is not available`,
		},
		{
			name: "duplicate argument", provider: "fake",
			cap:    Capability{Name: "a", Risk: RiskRead, Arguments: []Argument{{Name: "x"}, {Name: "x"}}},
			wantIn: `argument "x" is declared twice`,
		},
		{
			name: "duplicate field", provider: "fake",
			cap:    Capability{Name: "a", Risk: RiskRead, Fields: []Field{{Name: "x"}, {Name: "x"}}},
			wantIn: `field "x" is declared twice`,
		},
		{
			name: "empty field name", provider: "fake",
			cap:    Capability{Name: "a", Risk: RiskRead, Fields: []Field{{}}},
			wantIn: "field name must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewRegistry().Register(tt.provider, tt.cap)

			if err == nil {
				t.Fatal("Register() = nil, want an error")
			}
			if got := err.Error(); !strings.Contains(got, tt.wantIn) {
				t.Errorf("error = %q, want it to contain %q", got, tt.wantIn)
			}
		})
	}
}

// Several providers may implement the same domain contract. Only a differing contract is an error.
func TestRegisterSharedContracts(t *testing.T) {
	t.Run("identical contract from two providers is allowed", func(t *testing.T) {
		reg := NewRegistry()
		if err := reg.Register("fake-wiki", pagesList); err != nil {
			t.Fatalf("Register() = %v", err)
		}
		if err := reg.Register("other-wiki", pagesList); err != nil {
			t.Errorf("Register() = %v, want nil", err)
		}
	})

	t.Run("differing contract is a registry error", func(t *testing.T) {
		changed := pagesList
		changed.Fields = []Field{{Name: "id"}, {Name: "title"}}

		reg := NewRegistry()
		if err := reg.Register("fake-wiki", pagesList); err != nil {
			t.Fatalf("Register() = %v", err)
		}
		err := reg.Register("other-wiki", changed)

		var conflict *ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("Register() = %v, want a *ConflictError", err)
		}
		if conflict.Name != pagesList.Name {
			t.Errorf("conflict name = %q, want %q", conflict.Name, pagesList.Name)
		}
	})

	t.Run("re-registering the same provider is allowed", func(t *testing.T) {
		reg := NewRegistry()
		if err := reg.Register("fake-wiki", pagesList); err != nil {
			t.Fatalf("Register() = %v", err)
		}
		if err := reg.Register("fake-wiki", pagesList); err != nil {
			t.Errorf("Register() = %v, want nil", err)
		}
	})
}

// The registry owns its contracts: neither the caller's input nor a previous answer may change what it
// reports later.
func TestRegistryIsolatesContracts(t *testing.T) {
	t.Run("mutating the input after registration has no effect", func(t *testing.T) {
		args := []Argument{{Name: "limit", Description: "original"}}
		reg := NewRegistry()
		if err := reg.Register("fake", Capability{Name: "a", Risk: RiskRead, Arguments: args}); err != nil {
			t.Fatalf("Register() = %v", err)
		}

		args[0].Description = "mutated"

		if got := reg.Provider("fake")[0].Arguments[0].Description; got != "original" {
			t.Errorf("description = %q, want original", got)
		}
	})

	t.Run("mutating an answer has no effect on the next one", func(t *testing.T) {
		reg := mustRegistry(t)

		first := reg.Provider("fake-wiki")
		first[0].Arguments[0].Name = "mutated"
		first[0].Fields[0].Name = "mutated"

		second := reg.Provider("fake-wiki")
		if second[0].Arguments[0].Name == "mutated" || second[0].Fields[0].Name == "mutated" {
			t.Errorf("the registry returned mutated state: %+v", second[0])
		}
	})

	t.Run("a rejected registration records nothing", func(t *testing.T) {
		reg := NewRegistry()
		if err := reg.Register("first", pagesList); err != nil {
			t.Fatalf("Register() = %v", err)
		}
		conflicting := pagesList
		conflicting.Description = "different"

		if err := reg.Register("second", tickets, conflicting); err == nil {
			t.Fatal("Register() = nil, want a conflict error")
		}

		if got := reg.Provider("second"); len(got) != 0 {
			t.Errorf("provider second holds %v, want nothing after a rejected call", names(got))
		}
	})

	t.Run("a conflict inside one call is detected", func(t *testing.T) {
		changed := pagesList
		changed.Description = "different"

		err := NewRegistry().Register("fake", pagesList, changed)

		var conflict *ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("Register() = %v, want a *ConflictError", err)
		}
	})

	// "no arguments" is one contract, whether a provider writes nil or an empty slice.
	t.Run("nil and empty slices are the same contract", func(t *testing.T) {
		implicit := Capability{Name: "a", Risk: RiskRead}
		explicit := Capability{Name: "a", Risk: RiskRead, Arguments: []Argument{}, Fields: []Field{}}

		reg := NewRegistry()
		if err := reg.Register("first", implicit); err != nil {
			t.Fatalf("Register() = %v", err)
		}
		if err := reg.Register("second", explicit); err != nil {
			t.Errorf("Register() = %v, want nil", err)
		}
	})
}

func TestCatalogList(t *testing.T) {
	reg := mustRegistry(t)
	// wiki and wiki-audit are two connections of the same provider; archive is a second instance.
	cat := NewCatalog(reg, map[string]string{
		"wiki":       "fake-wiki",
		"wiki-audit": "fake-wiki",
		"archive":    "fake-wiki",
		"tracker":    "fake-tracker",
	})

	t.Run("union deduplicates by name", func(t *testing.T) {
		got, err := cat.List("")
		if err != nil {
			t.Fatalf("List() = %v", err)
		}
		if want := []string{"knowledge.pages.get", "knowledge.pages.list", "tickets.issues.list"}; !reflect.DeepEqual(names(got), want) {
			t.Errorf("names = %v, want %v", names(got), want)
		}
	})

	t.Run("connection filter narrows the offer", func(t *testing.T) {
		got, err := cat.List("tracker")
		if err != nil {
			t.Fatalf("List() = %v", err)
		}
		if want := []string{"tickets.issues.list"}; !reflect.DeepEqual(names(got), want) {
			t.Errorf("names = %v, want %v", names(got), want)
		}
	})

	t.Run("unknown connection", func(t *testing.T) {
		_, err := cat.List("absent")

		var unknown *UnknownConnectionError
		if !errors.As(err, &unknown) {
			t.Fatalf("List() = %v, want an *UnknownConnectionError", err)
		}
	})

	t.Run("repeated queries are identically ordered", func(t *testing.T) {
		first, err := cat.List("")
		if err != nil {
			t.Fatalf("List() = %v", err)
		}
		for i := 0; i < 20; i++ {
			got, err := cat.List("")
			if err != nil {
				t.Fatalf("List() = %v", err)
			}
			if !reflect.DeepEqual(got, first) {
				t.Fatalf("run %d differs: %v, want %v", i+2, names(got), names(first))
			}
		}
	})
}

// A provider that no connection uses contributes nothing to the available capabilities.
func TestCatalogIgnoresUnconfiguredProviders(t *testing.T) {
	cat := NewCatalog(mustRegistry(t), map[string]string{"tracker": "fake-tracker"})

	got, err := cat.List("")

	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if want := []string{"tickets.issues.list"}; !reflect.DeepEqual(names(got), want) {
		t.Errorf("names = %v, want %v; fake-wiki is registered but unused", names(got), want)
	}
}

func TestCatalogWithoutConnections(t *testing.T) {
	got, err := NewCatalog(mustRegistry(t), nil).List("")

	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("names = %v, want none", names(got))
	}
}

func TestCatalogDescribe(t *testing.T) {
	cat := NewCatalog(mustRegistry(t), map[string]string{"wiki": "fake-wiki", "tracker": "fake-tracker"})

	t.Run("full contract is returned", func(t *testing.T) {
		got, err := cat.Describe("wiki", "knowledge.pages.get")
		if err != nil {
			t.Fatalf("Describe() = %v", err)
		}
		if !reflect.DeepEqual(got, pagesGet) {
			t.Errorf("capability = %+v, want %+v", got, pagesGet)
		}
	})

	t.Run("union describes any offered capability", func(t *testing.T) {
		if _, err := cat.Describe("", "tickets.issues.list"); err != nil {
			t.Errorf("Describe() = %v, want nil", err)
		}
	})

	t.Run("connection does not offer the capability", func(t *testing.T) {
		_, err := cat.Describe("tracker", "knowledge.pages.get")

		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) {
			t.Fatalf("Describe() = %v, want an *UnsupportedError", err)
		}
		if unsupported.Connection != "tracker" {
			t.Errorf("connection = %q, want tracker", unsupported.Connection)
		}
	})

	t.Run("no connection offers the capability", func(t *testing.T) {
		_, err := cat.Describe("", "absent.capability")

		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) {
			t.Fatalf("Describe() = %v, want an *UnsupportedError", err)
		}
		if unsupported.Connection != "" {
			t.Errorf("connection = %q, want empty", unsupported.Connection)
		}
	})

	// An unknown connection and a connection without the capability are different errors.
	t.Run("unknown connection differs from unsupported capability", func(t *testing.T) {
		_, unknownErr := cat.Describe("absent", "knowledge.pages.get")
		_, unsupportedErr := cat.Describe("tracker", "knowledge.pages.get")

		var unknown *UnknownConnectionError
		var unsupported *UnsupportedError
		if !errors.As(unknownErr, &unknown) || errors.As(unknownErr, &unsupported) {
			t.Errorf("unknown connection produced %v", unknownErr)
		}
		if !errors.As(unsupportedErr, &unsupported) || errors.As(unsupportedErr, &unknown) {
			t.Errorf("unsupported capability produced %v", unsupportedErr)
		}
	})
}

func TestSummary(t *testing.T) {
	tests := []struct {
		name string
		cap  Capability
		want string
	}{
		{"required arguments only", pagesGet, "read knowledge.pages.get(id)"},
		{"no required arguments", pagesList, "read knowledge.pages.list()"},
		{"no arguments at all", tickets, "read tickets.issues.list()"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cap.Summary(); got != tt.want {
				t.Errorf("Summary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func names(caps []Capability) []string {
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = c.Name
	}
	return out
}
