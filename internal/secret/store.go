package secret

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

// StoreService is the service name every callbell entry lives under: one namespace in Secret Service on
// Linux, in the macOS Keychain, and in the Windows Credential Manager, so the entries are recognisable in
// the platform's own key manager and removable without callbell.
const StoreService = "callbell-cli"

// StoreSelector names the environment variable that chooses the credential store. "auto", the default,
// uses the store of the platform. "none" replaces it with a store that holds nothing, which is what a CI
// run, a container, and every test wants: no D-Bus call, no unlock prompt, no dependency on the machine.
const StoreSelector = "CALLBELL_CREDENTIAL_STORE"

// The values StoreSelector accepts. They are exported because a caller that switched the store off has to
// be able to say so: a command that skipped the store must not look like one that cleared it.
const (
	StoreAuto = "auto"
	StoreNone = "none"
)

// SystemStore returns the credential store this process should use.
//
// An unrecognised selector is an error rather than a silent "auto": a user who writes off or disabled
// means to switch the store off, and quietly doing the opposite is the one outcome they did not ask for.
// The message names the accepted values without quoting what was written.
//
// callbell-dev: github.com/zalando/go-keyring is the whole dependency here. It is small, has no cgo, and
// covers exactly the three platforms this project targets. Anything richer would add a backend zoo that
// nothing in the cascade needs.
func SystemStore() (Store, error) {
	switch os.Getenv(StoreSelector) {
	case "", StoreAuto:
		return systemStore{}, nil
	case StoreNone:
		return unavailableStore{err: ErrDisabled}, nil
	default:
		return nil, fmt.Errorf("%s must be %s or %s", StoreSelector, StoreAuto, StoreNone)
	}
}

// storeTimeout bounds every call into the platform store.
//
// The library offers no context, and the call goes to a service in another process: a half-started
// keyring daemon can leave it waiting forever, which would freeze a command and, later, an interface that
// has to stay usable. Thirty seconds is long enough for a person to answer a legitimate unlock dialog and
// short enough that a stuck daemon does not hold a run indefinitely. It is the same order as the request
// timeout the provider uses, so nothing in this binary waits on anything for longer than that.
const storeTimeout = 30 * time.Second

type systemStore struct{}

func (systemStore) Get(key string) (string, error) {
	value, err := within(storeTimeout, func() (string, error) { return keyring.Get(StoreService, key) })
	switch {
	case err == nil:
		return value, nil
	case errors.Is(err, keyring.ErrNotFound):
		return "", ErrNoEntry
	case errors.Is(err, ErrUnavailable):
		return "", err
	default:
		return "", classify(err)
	}
}

func (systemStore) Set(key, value string) error {
	_, err := within(storeTimeout, func() (struct{}, error) {
		return struct{}{}, keyring.Set(StoreService, key, value)
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnavailable):
		return err
	default:
		return classify(err)
	}
}

func (systemStore) Delete(key string) error {
	_, err := within(storeTimeout, func() (struct{}, error) {
		return struct{}{}, keyring.Delete(StoreService, key)
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keyring.ErrNotFound):
		return ErrNoEntry
	case errors.Is(err, ErrUnavailable):
		return err
	default:
		return classify(err)
	}
}

// within runs one store operation under a deadline. A deadline that passes is not an abort: it is the same
// class as a store that cannot be reached, so the cascade moves on and the stage says what happened.
//
// The channel is buffered, so the operation can still finish and hand back its result after the deadline
// without blocking on a receiver that has gone away.
func within[T any](limit time.Duration, op func() (T, error)) (T, error) {
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := op()
		done <- outcome{value: value, err: err}
	}()

	timer := time.NewTimer(limit)
	defer timer.Stop()

	select {
	case got := <-done:
		return got.value, got.err
	case <-timer.C:
		var zero T
		return zero, fmt.Errorf("%w: %w after %s", ErrUnavailable, ErrTimedOut, limit)
	}
}

// classify turns anything the platform reports into the one class the cascade acts on. The original text
// is kept for diagnosis: it describes the service, never a stored value.
func classify(err error) error {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "locked collection") ||
		strings.Contains(message, "failed to unlock correct collection") {
		return fmt.Errorf("%w: %w", ErrUnavailable, ErrLocked)
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

// unavailableStore stands in wherever no store can be reached. It keeps a missing service from becoming a
// dead end: the cascade simply moves on to the next stage.
type unavailableStore struct{ err error }

func (s unavailableStore) Get(string) (string, error) { return "", s.err }
func (s unavailableStore) Set(string, string) error   { return s.err }
func (s unavailableStore) Delete(string) error        { return s.err }

// MemoryStore is a credential store that lives in this process only.
//
// It is exported on purpose: the resolver must be exercised without touching the store of the machine the
// tests run on, and the packages that need it are not this one.
type MemoryStore struct {
	mu          sync.Mutex
	entries     map[string]string
	unavailable error
}

// NewMemoryStore returns an empty in-process store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{entries: map[string]string{}} }

// Fail makes every operation report err, which is how a machine without a running secret service behaves.
// Passing nil makes the store work again.
func (s *MemoryStore) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavailable = err
}

func (s *MemoryStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable != nil {
		return "", s.unavailable
	}
	value, ok := s.entries[key]
	if !ok {
		return "", ErrNoEntry
	}
	return value, nil
}

func (s *MemoryStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable != nil {
		return s.unavailable
	}
	s.entries[key] = value
	return nil
}

func (s *MemoryStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable != nil {
		return s.unavailable
	}
	if _, ok := s.entries[key]; !ok {
		return ErrNoEntry
	}
	delete(s.entries, key)
	return nil
}
