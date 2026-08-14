package secret

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// switchOff turns the fallback off while leaving every entry in place, the state the file header itself
// suggests to a reader who wants the file to stop delivering.
func switchOff(t *testing.T, f *File) {
	t.Helper()
	data, err := os.ReadFile(f.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	off := strings.Replace(string(data), "allow_plaintext: true", "allow_plaintext: false", 1)
	if err := os.WriteFile(f.Path(), []byte(off), fileMode); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// What Holds reports must be removable. The switch decides whether the file delivers a secret, not whether
// it can be cleaned up: an entry named as present that nothing can remove is a dead end.
func TestInertFileCanStillBeCleanedUp(t *testing.T) {
	f := tempFile(t)
	if err := f.Set(credName, role, canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if err := f.Set("other", role, canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	switchOff(t, f)

	// It delivers nothing, and it does hold something: two different questions, two different answers.
	if _, err := f.Get(credName, role); !errors.Is(err, ErrDisabled) {
		t.Errorf("Get() = %v, want the switch to keep it from delivering", err)
	}
	holds, err := f.Holds(credName, role)
	if err != nil || !holds {
		t.Fatalf("Holds() = %v, %v; want the entry on disk to be reported", holds, err)
	}

	if err := f.Delete(credName, role); err != nil {
		t.Fatalf("Delete() = %v, want the entry the file holds to be removable", err)
	}

	if holds, err := f.Holds(credName, role); err != nil || holds {
		t.Errorf("Holds() = %v, %v; want the entry gone", holds, err)
	}
	if holds, err := f.Holds("other", role); err != nil || !holds {
		t.Errorf("Holds() = %v, %v; want the other entry kept", holds, err)
	}
	// Deleting neither switches the fallback on nor off.
	data, err := os.ReadFile(f.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "allow_plaintext: false") {
		t.Errorf("file = %q, want the switch left as it stood", data)
	}
}

// A write must not drop what the file already holds, whatever the switch says.
func TestSetKeepsWhatTheFileAlreadyHolds(t *testing.T) {
	f := tempFile(t)
	if err := f.Set(credName, role, canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if err := f.Set("other", role, canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	switchOff(t, f)

	if err := f.Set(credName, "token-secret", canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	for _, pair := range [][2]string{{credName, role}, {credName, "token-secret"}, {"other", role}} {
		if holds, err := f.Holds(pair[0], pair[1]); err != nil || !holds {
			t.Errorf("Holds(%s, %s) = %v, %v; want the entry kept", pair[0], pair[1], holds, err)
		}
	}
	// Writing is the explicit decision that switches the fallback back on, so it delivers again.
	if _, err := f.Get(credName, role); err != nil {
		t.Errorf("Get() = %v, want the switch turned on by the write", err)
	}
}

// The mode check governs every operation, not only reading: a file others can read is not written to and
// not deleted from either, and the refusal names the fix.
func TestWidenedModeRefusesEveryOperation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not carry on Windows")
	}
	f := tempFile(t)
	if err := f.Set(credName, role, canaryPlaintext); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if err := os.Chmod(f.Path(), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var tooOpen *PermissionError
	if _, err := f.Get(credName, role); !errors.As(err, &tooOpen) {
		t.Errorf("Get() = %v, want the mode refusal", err)
	}
	if err := f.Set(credName, role, canaryPlaintext); !errors.As(err, &tooOpen) {
		t.Errorf("Set() = %v, want the mode refusal", err)
	}
	if err := f.Delete(credName, role); !errors.As(err, &tooOpen) {
		t.Errorf("Delete() = %v, want the mode refusal", err)
	}
	if _, err := f.Holds(credName, role); !errors.As(err, &tooOpen) {
		t.Errorf("Holds() = %v, want the mode refusal", err)
	}
	if !strings.Contains(tooOpen.Error(), "chmod 600") {
		t.Errorf("error = %q, want it to name the fix", tooOpen)
	}
}
