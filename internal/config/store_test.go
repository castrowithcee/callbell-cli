package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sample builds a small but complete configuration in memory. No test uses a real secret.
func sample(t *testing.T) *Config {
	t.Helper()
	cfg := New(testProviders)
	must(t, cfg.SetService("wiki", Service{Provider: "bookstack", BaseURL: "https://wiki.example.invalid"}))
	must(t, cfg.SetCredential("reader", Credential{
		Type:   CredentialTypeEnv,
		Values: map[string]string{"token-id": "WIKI_TOKEN_ID", "token-secret": "WIKI_TOKEN_SECRET"},
	}))
	must(t, cfg.SetConnection("wiki", Connection{Service: "wiki", Credential: "reader"}))
	must(t, cfg.SetDefault("knowledge", "wiki"))
	return cfg
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func newTarget(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "callbell", "config.yaml")
	return NewStore(path, testProviders), path
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	store, path := newTarget(t)
	cfg := sample(t)

	if err := store.Save(cfg); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !reflect.DeepEqual(loaded, cfg) {
		t.Errorf("loaded = %+v,\nwant %+v", loaded, cfg)
	}

	// Only variable names are written, never values.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"WIKI_TOKEN_ID", "WIKI_TOKEN_SECRET", "version: 1"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("file is missing %q:\n%s", want, data)
		}
	}
}

func TestSecretRoleDescriptionsExplainTheBookStackValues(t *testing.T) {
	for role, want := range map[string]string{
		"token-id":     "value labeled Token ID",
		"token-secret": "value labeled Token Secret",
	} {
		if got := New(testProviders).SecretRoleDescription(role); !strings.Contains(got, want) {
			t.Errorf("SecretRoleDescription(%q) = %q, want it to contain %q", role, got, want)
		}
	}
	if got := New(testProviders).SecretRoleDescription("unknown"); got != "" {
		t.Errorf("SecretRoleDescription(unknown) = %q, want empty", got)
	}
}

// An empty configuration must round-trip to the same model as any other.
func TestSaveAndLoadEmptyRoundTrip(t *testing.T) {
	store, _ := newTarget(t)
	cfg := store.New()

	if err := store.Save(cfg); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !reflect.DeepEqual(loaded, cfg) {
		t.Errorf("loaded = %+v,\nwant %+v", loaded, cfg)
	}
}

// A configuration reached through a symlink keeps the symlink and updates the real file.
func TestSaveThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-config.yaml")
	link := filepath.Join(dir, "config.yaml")

	if err := NewStore(real, testProviders).Save(New(testProviders)); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := NewStore(link, testProviders).Save(sample(t)); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	loaded, err := NewStore(real, testProviders).Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if len(loaded.Services) != 1 {
		t.Errorf("the real file was not updated: %+v", loaded)
	}
	assertNoLeftovers(t, dir)
}

func TestFilePermissions(t *testing.T) {
	store, path := newTarget(t)

	if err := store.Save(sample(t)); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	file, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := file.Mode().Perm(); got != fileMode {
		t.Errorf("file mode = %o, want %o", got, fileMode)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := dir.Mode().Perm(); got != dirMode {
		t.Errorf("directory mode = %o, want %o", got, dirMode)
	}
}

func TestCRUD(t *testing.T) {
	cfg := sample(t)

	t.Run("update replaces an entry", func(t *testing.T) {
		must(t, cfg.SetService("wiki", Service{Provider: "bookstack", BaseURL: "https://other.example.invalid"}))

		if got := cfg.Services["wiki"].BaseURL; got != "https://other.example.invalid" {
			t.Errorf("base url = %q", got)
		}
	})

	t.Run("a second connection reuses the service", func(t *testing.T) {
		must(t, cfg.SetCredential("auditor", Credential{
			Type:   CredentialTypeEnv,
			Values: map[string]string{"token-id": "AUDIT_ID", "token-secret": "AUDIT_SECRET"},
		}))
		must(t, cfg.SetConnection("wiki-audit", Connection{Service: "wiki", Credential: "auditor"}))

		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() = %v", err)
		}
	})

	t.Run("a referenced service cannot be deleted", func(t *testing.T) {
		err := cfg.DeleteService("wiki")

		var inUse *InUseError
		if !errors.As(err, &inUse) {
			t.Fatalf("DeleteService() = %v, want an *InUseError", err)
		}
		if !reflect.DeepEqual(inUse.By, []string{"wiki", "wiki-audit"}) {
			t.Errorf("used by %v", inUse.By)
		}
	})

	t.Run("a referenced credential cannot be deleted", func(t *testing.T) {
		var inUse *InUseError
		if err := cfg.DeleteCredential("auditor"); !errors.As(err, &inUse) {
			t.Fatalf("DeleteCredential() = %v, want an *InUseError", err)
		}
	})

	t.Run("a default keeps its connection alive", func(t *testing.T) {
		var inUse *InUseError
		if err := cfg.DeleteConnection("wiki"); !errors.As(err, &inUse) {
			t.Fatalf("DeleteConnection() = %v, want an *InUseError", err)
		}
	})

	t.Run("deleting in dependency order works", func(t *testing.T) {
		must(t, cfg.DeleteDefault("knowledge"))
		must(t, cfg.DeleteConnection("wiki"))
		must(t, cfg.DeleteConnection("wiki-audit"))
		must(t, cfg.DeleteCredential("auditor"))
		must(t, cfg.DeleteService("wiki"))

		if len(cfg.Services) != 0 || len(cfg.Connections) != 0 || len(cfg.Defaults.Connections) != 0 {
			t.Errorf("configuration is not empty: %+v", cfg)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() = %v", err)
		}
	})

	t.Run("deleting something absent is reported", func(t *testing.T) {
		var notThere *NotThereError
		for _, err := range []error{
			cfg.DeleteService("absent"), cfg.DeleteCredential("absent"),
			cfg.DeleteConnection("absent"), cfg.DeleteDefault("absent"),
		} {
			if !errors.As(err, &notThere) {
				t.Errorf("error = %v, want a *NotThereError", err)
			}
		}
	})

	t.Run("an empty name is rejected", func(t *testing.T) {
		for _, err := range []error{
			cfg.SetService("", Service{}), cfg.SetCredential("", Credential{}),
			cfg.SetConnection("", Connection{}), cfg.SetDefault("", "wiki"),
		} {
			if err == nil {
				t.Error("Set with an empty name = nil, want an error")
			}
		}
	})
}

// Every failure must leave an existing target byte-identical and must not leave a temporary file behind.
func TestFailedSaveLeavesTargetIntact(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, store *Store) *Config
		wantIn  string
	}{
		{
			name: "an invalid reference is rejected",
			prepare: func(t *testing.T, _ *Store) *Config {
				cfg := sample(t)
				cfg.Connections["wiki"] = Connection{Service: "absent", Credential: "reader"}
				return cfg
			},
			wantIn: "unknown service",
		},
		{
			name: "a wrong version is rejected",
			prepare: func(t *testing.T, _ *Store) *Config {
				cfg := sample(t)
				cfg.Version = 2
				return cfg
			},
			wantIn: "version",
		},
		{
			name: "an encoding failure is rejected",
			prepare: func(t *testing.T, store *Store) *Config {
				store.marshal = func(any) ([]byte, error) { return nil, errors.New("simulated encoder failure") }
				return sample(t)
			},
			wantIn: "could not be encoded",
		},
		{
			name: "output that would not load back is rejected",
			prepare: func(t *testing.T, store *Store) *Config {
				store.marshal = func(any) ([]byte, error) { return []byte("version: 1\nsurprise: true\n"), nil }
				return sample(t)
			},
			wantIn: "not loadable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, path := newTarget(t)
			if err := store.Save(sample(t)); err != nil {
				t.Fatalf("initial Save() = %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			err = store.Save(tt.prepare(t, store))

			if err == nil {
				t.Fatal("Save() = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantIn)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("read: %v", readErr)
			}
			if string(after) != string(before) {
				t.Errorf("the target changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			assertNoLeftovers(t, filepath.Dir(path))
		})
	}
}

// A directory that cannot be written to must fail before the target is touched.
func TestSaveIntoUnwritableDirectory(t *testing.T) {
	store, path := newTarget(t)
	if err := store.Save(sample(t)); err != nil {
		t.Fatalf("initial Save() = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, dirMode) })

	if err := store.Save(sample(t)); err == nil {
		t.Fatal("Save() = nil, want a write failure")
	}

	if err := os.Chmod(dir, dirMode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != string(before) {
		t.Error("the target changed although the write failed")
	}
	assertNoLeftovers(t, dir)
}

func TestSaveLeavesNoTemporaryFiles(t *testing.T) {
	store, path := newTarget(t)

	for i := 0; i < 3; i++ {
		if err := store.Save(sample(t)); err != nil {
			t.Fatalf("Save() = %v", err)
		}
	}

	assertNoLeftovers(t, filepath.Dir(path))
}

func TestLoadMissingTarget(t *testing.T) {
	store, _ := newTarget(t)

	_, err := store.Load()

	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("Load() = %v, want a *NotFoundError", err)
	}
}

// The store creates a configuration where none existed.
func TestSaveCreatesTheFileAndDirectory(t *testing.T) {
	store, path := newTarget(t)

	if err := store.Save(store.New()); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("stat: %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if loaded.Version != Version {
		t.Errorf("version = %d, want %d", loaded.Version, Version)
	}
}

func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".callbell-config-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
