package secret

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempFile(t *testing.T) *File {
	t.Helper()
	return NewFile(filepath.Join(t.TempDir(), FileName))
}

// The file comes into existence only through an explicit write, with the switch set and mode 0600.
func TestFileSet(t *testing.T) {
	f := tempFile(t)

	if _, err := os.Stat(f.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file exists before anything was written: %v", err)
	}
	if err := f.Set(credName, role, canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	info, err := os.Stat(f.Path())
	if err != nil {
		t.Fatalf("Stat() = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(f.Path())
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}
	if !strings.Contains(string(data), "allow_plaintext: true") {
		t.Errorf("file = %q, want the switch written", data)
	}
	if !strings.HasPrefix(string(data), "#") {
		t.Errorf("file = %q, want the explaining header first", data)
	}

	got, err := f.Get(credName, role)
	if err != nil || got != canaryPlaintext {
		t.Errorf("Get() = %q, %v, want the stored value", got, err)
	}
}

// Without the switch the file delivers nothing, however complete it looks.
func TestFileWithoutTheSwitch(t *testing.T) {
	f := tempFile(t)
	body := "version: 1\nallow_plaintext: false\ncredentials:\n  " + credName + ":\n    " + role + ": " + canaryPlaintext + "\n"
	if err := os.WriteFile(f.Path(), []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if _, err := f.Get(credName, role); !errors.Is(err, ErrDisabled) {
		t.Errorf("Get() = %v, want ErrDisabled", err)
	}
}

func TestFileGetStates(t *testing.T) {
	t.Run("an absent file is not enabled", func(t *testing.T) {
		if _, err := tempFile(t).Get(credName, role); !errors.Is(err, ErrDisabled) {
			t.Errorf("Get() = %v, want ErrDisabled", err)
		}
	})

	t.Run("a switched-on file without the entry", func(t *testing.T) {
		f := tempFile(t)
		if err := f.Set(credName, "token-secret", canaryPlaintext); err != nil {
			t.Fatalf("Set() = %v", err)
		}
		if _, err := f.Get(credName, role); !errors.Is(err, ErrNoEntry) {
			t.Errorf("Get() = %v, want ErrNoEntry", err)
		}
	})

	t.Run("a file that is not a fallback at all", func(t *testing.T) {
		f := tempFile(t)
		if err := os.WriteFile(f.Path(), []byte("nonsense: [1, 2\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		_, err := f.Get(credName, role)
		if err == nil || errors.Is(err, ErrNoEntry) || errors.Is(err, ErrDisabled) {
			t.Errorf("Get() = %v, want a read failure", err)
		}
		if strings.Contains(err.Error(), "nonsense") {
			t.Errorf("error = %q, want it to quote no line of the file", err)
		}
	})
}

// Deleting the last entry removes the file, so no switched-on plaintext file stays behind empty.
func TestFileDelete(t *testing.T) {
	f := tempFile(t)
	if err := f.Set(credName, role, canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if err := f.Set(credName, "token-secret", canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	if err := f.Delete(credName, role); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if _, err := os.Stat(f.Path()); err != nil {
		t.Fatalf("the file is gone while an entry remains: %v", err)
	}
	if err := f.Delete(credName, "token-secret"); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if _, err := os.Stat(f.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the empty file was kept: %v", err)
	}
	if err := f.Delete(credName, role); !errors.Is(err, ErrNoEntry) {
		t.Errorf("Delete() = %v, want ErrNoEntry", err)
	}
}
