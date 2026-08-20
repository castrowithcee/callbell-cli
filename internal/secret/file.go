package secret

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	yaml "go.yaml.in/yaml/v3"
)

// FileName is the plaintext fallback. It lives beside config.yaml, in the same directory the
// configuration was resolved from, and never inside it: the configuration file must stay a file that can
// be read, diffed, and shared without exposing a secret.
const FileName = "credentials.yaml"

// fileVersion is the schema version of the fallback file.
const fileVersion = 1

// File modes. The fallback holds secrets in clear text, so it is readable by its owner only.
const (
	fileMode = 0o600
	dirMode  = 0o700
)

// header explains the file to whoever finds it later. It is a comment, so reading it back is unaffected.
const header = `# Callbell CLI plaintext credential fallback.
#
# This file holds secrets in clear text. It exists only because it was switched on explicitly, and it is
# read only while allow_plaintext is true. It is the named way out for a machine without a working system
# credential store; on a machine that has one, delete this file and keep the secrets there.
#
# The file must stay readable by its owner alone (mode 0600).
`

// File is the plaintext fallback file. A nil-free zero value is not useful; use NewFile.
type File struct{ path string }

// NewFile returns the fallback file at path. The file does not have to exist.
func NewFile(path string) *File { return &File{path: path} }

// Path returns the file this fallback reads and writes.
func (f *File) Path() string { return f.path }

// content is the on-disk shape. allow_plaintext is the switch: the fallback is inert without it, so a file
// left over from an experiment, or written by something else, cannot deliver a secret by accident.
type content struct {
	Version        int                          `yaml:"version"`
	AllowPlaintext bool                         `yaml:"allow_plaintext"`
	Credentials    map[string]map[string]string `yaml:"credentials,omitempty"`
}

// Get returns the stored secret. It reports ErrDisabled when the fallback is absent or not switched on,
// and ErrNoEntry when it is switched on but holds nothing for the pair.
func (f *File) Get(credential, role string) (string, error) {
	c, err := f.read()
	if err != nil {
		return "", err
	}
	if value := c.Credentials[credential][role]; value != "" {
		return value, nil
	}
	return "", ErrNoEntry
}

// Set stores a secret and switches the fallback on. This is the only place the file is created.
//
// It reads what is on disk, not what may be delivered. Reading through the switch would hand back an empty
// file whenever the fallback was off, and writing that back would drop every entry the file already had:
// silent loss of secrets, at the one place in this project where they sit in clear text.
func (f *File) Set(credential, role, value string) error {
	c, err := f.load()
	if err != nil && !errors.Is(err, ErrDisabled) && !errors.Is(err, ErrNoEntry) {
		return err
	}
	if c.Credentials == nil {
		c.Credentials = map[string]map[string]string{}
	}
	if c.Credentials[credential] == nil {
		c.Credentials[credential] = map[string]string{}
	}
	c.Credentials[credential][role] = value
	c.Version = fileVersion
	c.AllowPlaintext = true
	return f.write(c)
}

// Delete removes a secret. When the last entry is gone the file is removed as well, so no switched-on
// plaintext file stays behind without a reason to exist.
//
// Like Set it reads what is on disk. The switch decides whether a file delivers a secret; it must not
// decide whether one can be cleaned up. An entry nobody wants read any more is exactly the entry that has
// to be removable, and Holds reports it, so refusing to delete it would leave a copy that is named as
// present and cannot be got rid of. The switch itself is left as it stands: deleting neither turns the
// fallback on nor off.
func (f *File) Delete(credential, role string) error {
	c, err := f.load()
	if err != nil {
		// An absent or empty file simply holds nothing to delete.
		if errors.Is(err, ErrDisabled) {
			return ErrNoEntry
		}
		return err
	}
	if _, ok := c.Credentials[credential][role]; !ok {
		return ErrNoEntry
	}
	delete(c.Credentials[credential], role)
	if len(c.Credentials[credential]) == 0 {
		delete(c.Credentials, credential)
	}
	if len(c.Credentials) == 0 {
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cannot remove %s: %w", f.path, err)
		}
		return nil
	}
	return f.write(c)
}

// PermissionError reports a plaintext fallback that other users can read. It names the file and the way
// to fix it, and like every other message about this file it quotes no line of its content.
type PermissionError struct {
	Path string
	Mode fs.FileMode
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("%s holds secrets in clear text but its mode is %04o; it is not read until it is "+
		"private again: chmod 600 %s", e.Path, e.Mode.Perm(), e.Path)
}

// checkMode refuses a fallback that is readable or writable by anyone but its owner. The file is the one
// place in this project where a secret sits in clear text, so a widened mode is a defect to report rather
// than a detail to ignore, the same way ssh refuses a private key that others can read.
//
// Windows is exempt because the check cannot mean anything there. Access is governed by ACLs, and os.Stat
// synthesises a mode from the read-only attribute alone, so every ordinary file reports 0666 and would be
// refused. The Credential Manager is the real store on that platform anyway; the fallback exists for a
// Unix machine without a running secret service.
func checkMode(path string, info fs.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return &PermissionError{Path: path, Mode: perm}
	}
	return nil
}

// Holds reports whether the file keeps an entry for the pair, whether or not the fallback is switched on.
//
// It is a different question from Get. Get asks what may be delivered, and an inert file delivers nothing;
// Holds asks what lies on disk, because a copy of a secret in a file nobody reads is still a copy of that
// secret. A file that cannot be read at all is neither: that is an error, and the caller has to treat it as
// "cannot tell" rather than as "nothing there".
func (f *File) Holds(credential, role string) (bool, error) {
	c, err := f.load()
	switch {
	case errors.Is(err, ErrDisabled):
		// An absent and an empty file both hold nothing; there is no uncertainty in either.
		return false, nil
	case err != nil:
		return false, err
	}
	return c.Credentials[credential][role] != "", nil
}

// read returns the file content for delivery, and is used by Get alone.
//
// It is the answer to "what may this file hand out": an absent file and a file that was never switched on
// are the same thing to a reader, ErrDisabled. Every operation that manages the content rather than
// delivering it goes through load instead, because the switch must not decide what is on disk.
func (f *File) read() (content, error) {
	c, err := f.load()
	if err != nil {
		return content{}, err
	}
	if !c.AllowPlaintext {
		return content{}, ErrDisabled
	}
	return c, nil
}

// load returns what is on disk, without asking whether the fallback was switched on. An absent or empty
// file reports ErrDisabled: there is nothing there to talk about.
func (f *File) load() (content, error) {
	info, err := os.Stat(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return content{}, ErrDisabled
		}
		return content{}, fmt.Errorf("cannot read %s: %w", f.path, err)
	}
	if err := checkMode(f.path, info); err != nil {
		return content{}, err
	}

	data, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return content{}, ErrDisabled
		}
		return content{}, fmt.Errorf("cannot read %s: %w", f.path, err)
	}

	var c content
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		// An empty file carries no switch and no entry, so it is as inert as an absent one.
		if errors.Is(err, io.EOF) {
			return content{}, ErrDisabled
		}
		// The message names the file, never a line of it: every line may be a secret.
		return content{}, fmt.Errorf("cannot read %s: the file is not a valid credential fallback", f.path)
	}
	return c, nil
}

// write replaces the file atomically with mode 0600, the same way the configuration store does.
func (f *File) write(c content) error {
	body, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("cannot encode %s", f.path)
	}
	data := append([]byte(header), body...)

	target := f.path
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".callbell-credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", target, err)
	}
	name := tmp.Name()
	moved := false
	defer func() {
		if !moved {
			_ = os.Remove(name)
		}
	}()

	// CreateTemp already uses 0600; making it explicit keeps an umask from widening it.
	if err := os.Chmod(name, fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot set the permissions of %s: %w", name, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write %s: %w", name, err)
	}
	if err := os.Rename(name, target); err != nil {
		return fmt.Errorf("cannot replace %s: %w", target, err)
	}
	moved = true
	return nil
}
