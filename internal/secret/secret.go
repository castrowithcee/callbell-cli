// Package secret resolves the secrets a credential stands for. Resolution is provider independent and
// follows one fixed cascade, where the first stage that delivers wins:
//
//  1. the environment variable
//  2. the system credential store
//  3. the plaintext fallback file, and only when it was switched on explicitly
//
// The order follows the rule that the more explicit and the more short-lived source wins, the same order
// gh, the AWS CLI and kubectl use. Because overriding is allowed, every caller is told which stage
// delivered: otherwise a forgotten environment variable would shadow the credential store silently.
//
// The cascade describes how a credential is resolved that does not say where its secrets are. A credential
// of type env does say it: it names its variable, and that description is exhaustive. Resolution for such a
// credential therefore ends after stage one. Anything else would let a developer's local credential store
// or plaintext file stand in for a variable a CI run forgot to set, which is the opposite of what naming
// the variable was for.
//
// A resolved value is handed to the redactor and to the caller and to nobody else. It is never written to
// the configuration, never logged, and never part of an error message.
package secret

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/castrowithcee/callbell-cli/internal/config"
	"github.com/castrowithcee/callbell-cli/internal/redact"
)

// Source names the stage of the cascade that delivered a secret. The value is what a user sees, so it is
// prose rather than an identifier.
type Source string

// The stages of the cascade, plus the outcome that no stage delivered.
const (
	SourceEnv       Source = "environment variable"
	SourceStore     Source = "credential store"
	SourcePlaintext Source = "plaintext file"
	SourceMissing   Source = "missing"
)

// Value is a resolved secret together with the stage that produced it and the stages that were tried
// before. Only the resolver and the provider that authenticates with it ever see Secret.
type Value struct {
	Secret  string
	Source  Source
	Checked []string
}

// MissingSecretError reports that a credential does not yield a secret for a role. It is a usage problem.
//
// The message points at the configuration key that needs fixing and never repeats what the user wrote
// there. The configured text is treated as a possible secret, because a user who pastes a token into the
// field instead of a variable name writes something that a name rule cannot tell apart from a real name: a
// BookStack token is letters and digits, and so is a legal variable name. Naming the key and listing the
// stages that were checked keeps the message actionable without echoing the input.
//
// Err carries what a stage had to say beyond not delivering, for example that the plaintext fallback was
// refused and how to make it usable again. It is written by the resolver, holds no configured text and no
// stored value, and is unwrapped, so the same underlying state is classified the same way whether it was
// hit while reading, writing, or deleting.
type MissingSecretError struct {
	Credential string
	Role       string
	Type       string
	Checked    []string
	Err        error
}

func (e *MissingSecretError) Error() string {
	// A keyring credential has no values section, so naming one would send the user to a key that does
	// not exist in their file.
	key := fmt.Sprintf("credentials.%s.values.%s", e.Credential, e.Role)
	if e.Type == config.CredentialTypeKeyring {
		key = fmt.Sprintf("credentials.%s, secret role %s", e.Credential, e.Role)
	}

	parts := []string{
		key + " does not yield a secret",
		"checked: " + strings.Join(e.Checked, ", "),
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(append(parts, e.remedy()), "; ")
}

func (e *MissingSecretError) Unwrap() error { return e.Err }

// remedy names the way out. It differs by credential type, and it stays within the same rule as the rest
// of the message: it names commands, keys, and the derived variable, never a configured text, a stored
// value, or anything read from a file.
func (e *MissingSecretError) remedy() string {
	if e.Type == config.CredentialTypeKeyring {
		return fmt.Sprintf("store it with 'callbell credential set %s %s', or export %s",
			e.Credential, e.Role, DerivedEnvName(e.Credential, e.Role))
	}
	return fmt.Sprintf("set the environment variable that credentials.%s.values.%s names, or change that "+
		"credential to type keyring and use 'callbell credential set'", e.Credential, e.Role)
}

// Errors a Store reports. Anything else from a store is treated like ErrUnavailable, because a store that
// fails in an unexpected way must not be a dead end either.
var (
	// ErrNoEntry reports that the store answered and holds nothing under the key.
	ErrNoEntry = errors.New("the credential store holds no entry")
	// ErrUnavailable reports that no credential store could be reached, for example because no secret
	// service is running.
	ErrUnavailable = errors.New("the credential store is unavailable")
	// ErrDisabled reports that the system credential store was switched off deliberately.
	ErrDisabled = errors.New("the credential store is switched off")
	// ErrTimedOut reports a store that did not answer inside the deadline. It always accompanies
	// ErrUnavailable: to the cascade a store that never answers is a store that cannot be reached.
	ErrTimedOut = errors.New("the credential store did not answer in time")
)

// Store is the system credential store. It is an interface so a test never touches the store of the
// machine it runs on, and so a platform without one can be represented instead of aborting.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
}

// StoreKey is the account name one (credential, role) pair uses inside the store. Credential and role are
// both configuration names, so the key is stable, readable in the platform's own key manager, and free of
// characters that would need quoting.
func StoreKey(credential, role string) string { return credential + "/" + role }

// EnvName returns the environment variable that can carry the secret of one (credential, role) pair.
//
// A credential of type env names its variable itself: that is the unchanged path for CI and headless use.
// Every other credential type derives the name, so stage one of the cascade exists for it too without
// writing anything into the configuration.
func EnvName(credential string, cred config.Credential, role string) string {
	if cred.Type == config.CredentialTypeEnv {
		return cred.Values[role]
	}
	return DerivedEnvName(credential, role)
}

// DerivedEnvName is the deterministic variable name of a credential that does not name one itself:
// CALLBELL_<CREDENTIAL>_<ROLE>, upper-cased, with every character that is not a letter or a digit replaced
// by an underscore. The fixed prefix also guarantees a name that cannot start with a digit.
func DerivedEnvName(credential, role string) string {
	return "CALLBELL_" + envPart(credential) + "_" + envPart(role)
}

func envPart(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Resolver resolves secrets through the cascade. It is built once per run and shared.
type Resolver struct {
	env       func(string) string
	store     Store
	plaintext *File
	redactor  *redact.Redactor
}

// New returns the resolver of a normal run: the process environment, the credential store of the
// platform, and the plaintext fallback file in dir, which is the directory holding config.yaml. It fails
// only when the store was selected with an unrecognised value.
func New(dir string, red *redact.Redactor) (*Resolver, error) {
	store, err := SystemStore()
	if err != nil {
		return nil, err
	}
	return NewWith(os.Getenv, store, NewFile(filepath.Join(dir, FileName)), red), nil
}

// NewWith returns a resolver over an explicit environment, store, and fallback file. A nil file means the
// plaintext stage cannot deliver. It is how tests build a resolver that touches nothing of the machine.
func NewWith(env func(string) string, store Store, plaintext *File, red *redact.Redactor) *Resolver {
	if env == nil {
		env = func(string) string { return "" }
	}
	if store == nil {
		store = unavailableStore{err: ErrDisabled}
	}
	return &Resolver{env: env, store: store, plaintext: plaintext, redactor: red}
}

// Resolve returns the secret of one (credential, role) pair. The credential name and its configuration
// entry are passed separately because the entry alone does not know what it is called.
func (r *Resolver) Resolve(credential string, cred config.Credential, role string) (Value, error) {
	checked := make([]string, 0, 3)

	envName := EnvName(credential, cred, role)
	switch value := r.env(envName); {
	case envName == "":
		// Only a credential of type env can stay silent about its variable, and then the role is not
		// configured at all. The message names the stage, never the text behind the key.
		checked = append(checked, stage(SourceEnv, "not named"))
	case value != "":
		return r.deliver(value, SourceEnv, checked), nil
	default:
		checked = append(checked, stage(SourceEnv, "not set"))
	}

	// A credential of type env is fully described by the variable it names. Falling through to a store or
	// to a file would let something local answer for a variable that is simply not set, which is exactly
	// the CI and headless case this type exists for.
	if cred.Type == config.CredentialTypeEnv {
		return Value{}, missing(credential, cred, role, checked, nil)
	}

	switch value, err := r.store.Get(StoreKey(credential, role)); {
	case err == nil && value != "":
		return r.deliver(value, SourceStore, checked), nil
	case err == nil, errors.Is(err, ErrNoEntry):
		checked = append(checked, stage(SourceStore, "no entry"))
	case errors.Is(err, ErrDisabled):
		checked = append(checked, stage(SourceStore, "switched off"))
	case errors.Is(err, ErrTimedOut):
		checked = append(checked, stage(SourceStore, "timed out"))
	default:
		// A machine without a running secret service must not be a dead end, so an unreachable store is
		// one more stage that did not deliver rather than a failure.
		checked = append(checked, stage(SourceStore, "unavailable"))
	}

	value, err := r.fallback(credential, role)
	var cause error
	var tooOpen *PermissionError
	switch {
	case err == nil && value != "":
		return r.deliver(value, SourcePlaintext, checked), nil
	case errors.Is(err, ErrNoEntry):
		checked = append(checked, stage(SourcePlaintext, "no entry"))
	case errors.Is(err, ErrDisabled):
		// The same wording for the absent file and for the file that was never switched on: to a
		// reader they are the same thing, and the difference would only hint at what is on disk.
		checked = append(checked, stage(SourcePlaintext, "not enabled"))
	case errors.As(err, &tooOpen):
		checked = append(checked, stage(SourcePlaintext, "readable by others"))
		// The refusal is actionable and names no content, so it is carried into the message and stays
		// reachable for whoever classifies the error.
		cause = tooOpen
	default:
		checked = append(checked, stage(SourcePlaintext, "unreadable"))
	}

	return Value{}, missing(credential, cred, role, checked, cause)
}

func missing(credential string, cred config.Credential, role string, checked []string, cause error) error {
	return &MissingSecretError{
		Credential: credential, Role: role, Type: cred.Type, Checked: checked, Err: cause,
	}
}

// Status reports which stage would deliver, without handing the value to the caller. A user interface asks
// here, so it can show the source of a secret it must never see.
func (r *Resolver) Status(credential string, cred config.Credential, role string) (Source, []string) {
	value, err := r.Resolve(credential, cred, role)
	var missing *MissingSecretError
	if errors.As(err, &missing) {
		return SourceMissing, missing.Checked
	}
	if err != nil {
		return SourceMissing, []string{err.Error()}
	}
	return value.Source, value.Checked
}

// Set stores a secret for one (credential, role) pair in the system credential store. It never writes the
// plaintext fallback: that needs SetPlaintext and therefore an explicit decision.
func (r *Resolver) Set(credential, role, value string) error {
	r.register(value)
	return r.store.Set(StoreKey(credential, role), value)
}

// SetPlaintext writes a secret into the plaintext fallback file and switches that fallback on. It is the
// named way out for a machine without a credential store, and the only way the file ever comes into
// existence.
func (r *Resolver) SetPlaintext(credential, role, value string) error {
	if r.plaintext == nil {
		return ErrUnavailable
	}
	r.register(value)
	return r.plaintext.Set(credential, role, value)
}

// Delete removes a secret from the credential store and from the plaintext fallback and reports where it
// was actually removed. ErrNoEntry means nothing was stored anywhere.
//
// A delete succeeds only when no stage kept an entry back. If one place could not be cleared, the result
// is a *RemainingError naming that place, even when another place was cleared: a half-done delete that
// reports success would leave a copy of the secret behind under an all-clear.
func (r *Resolver) Delete(credential, role string) ([]Source, error) {
	var cleared []Source
	var remaining []Source
	var causes []error

	// A store that was switched off deliberately is not consulted and therefore holds nothing back that
	// this run was ever going to touch. Every other failure leaves an entry that may still be there.
	switch err := r.store.Delete(StoreKey(credential, role)); {
	case err == nil:
		cleared = append(cleared, SourceStore)
	case errors.Is(err, ErrNoEntry), errors.Is(err, ErrDisabled):
	default:
		remaining = append(remaining, SourceStore)
		causes = append(causes, err)
	}

	if r.plaintext != nil {
		switch err := r.plaintext.Delete(credential, role); {
		case err == nil:
			cleared = append(cleared, SourcePlaintext)
		case errors.Is(err, ErrNoEntry), errors.Is(err, ErrDisabled):
		default:
			remaining = append(remaining, SourcePlaintext)
			causes = append(causes, err)
		}
	}

	// Every place must be clear before a delete may call itself done. Reporting the part that worked and
	// staying silent about the copy that is still on disk would be the worst of both.
	if len(remaining) > 0 {
		return cleared, &RemainingError{
			Credential: credential, Role: role,
			Cleared: cleared, Remaining: remaining, Err: errors.Join(causes...),
		}
	}
	if len(cleared) == 0 {
		return nil, ErrNoEntry
	}
	return cleared, nil
}

// RemainingError reports a delete that could not clear every place a secret may sit. It names what is
// still there, what was removed, and what each stage said, so the blocker rather than the first failure
// decides the message. Its causes are unwrapped, so a caller classifies it by what actually blocked.
type RemainingError struct {
	Credential string
	Role       string
	Cleared    []Source
	Remaining  []Source
	Err        error
}

func (e *RemainingError) Error() string {
	msg := fmt.Sprintf("the secret for %s.%s may still be stored in the %s",
		e.Credential, e.Role, joinSources(e.Remaining))
	if len(e.Cleared) > 0 {
		msg += fmt.Sprintf(" (it was removed from the %s)", joinSources(e.Cleared))
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *RemainingError) Unwrap() error { return e.Err }

func joinSources(sources []Source) string {
	names := make([]string, len(sources))
	for i, s := range sources {
		names[i] = string(s)
	}
	return strings.Join(names, " and the ")
}

// Plaintext returns the fallback file this resolver reads, so a caller can name its path in a message. It
// is nil when no fallback is configured.
func (r *Resolver) Plaintext() *File { return r.plaintext }

// StoreSkipped reports whether the system credential store was switched off for this run and was therefore
// never consulted. A command that removes secrets has to be able to say so: it cleared what it could
// reach, and a silent success would otherwise read as "the secret is gone everywhere".
func (r *Resolver) StoreSkipped() bool {
	s, ok := r.store.(unavailableStore)
	return ok && errors.Is(s.err, ErrDisabled)
}

// Lookup reports whether an environment variable carries a value, without returning it. It answers the one
// question a user interface has about stage one: does something shadow the store?
func (r *Resolver) Lookup(name string) bool { return name != "" && r.env(name) != "" }

func (r *Resolver) fallback(credential, role string) (string, error) {
	if r.plaintext == nil {
		return "", ErrDisabled
	}
	return r.plaintext.Get(credential, role)
}

// deliver registers the value with the redactor before it leaves the resolver, so everything printed after
// this point is covered even if a provider echoes the credential back.
func (r *Resolver) deliver(value string, source Source, checked []string) Value {
	r.register(value)
	return Value{Secret: value, Source: source, Checked: checked}
}

func (r *Resolver) register(value string) {
	if r.redactor != nil {
		r.redactor.Add(value)
	}
}

func stage(source Source, reason string) string {
	return fmt.Sprintf("%s (%s)", source, reason)
}
