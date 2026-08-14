// Package redact removes known secret values from text before it is shown. Whoever resolves a secret
// registers its value here; every diagnostic and error passes through the redactor afterwards.
package redact

import (
	"sort"
	"strings"
	"sync"
)

// Marker replaces a secret value.
const Marker = "[redacted]"

// minLength keeps very short values out of the redactor. Replacing a two-character value would mangle
// unrelated output without protecting anything meaningful.
const minLength = 4

// Redactor holds the secret values seen in this process. The zero value is ready to use.
type Redactor struct {
	mu       sync.RWMutex
	secrets  []string
	replacer *strings.Replacer
}

// Add registers secret values. Empty and very short values are ignored.
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	changed := false
	for _, v := range values {
		if len(v) < minLength || contains(r.secrets, v) {
			continue
		}
		r.secrets = append(r.secrets, v)
		changed = true
	}
	if !changed {
		return
	}
	// Longest first, so a secret that contains another one is replaced as a whole.
	sort.SliceStable(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })

	pairs := make([]string, 0, 2*len(r.secrets))
	for _, secret := range r.secrets {
		pairs = append(pairs, secret, Marker)
	}
	r.replacer = strings.NewReplacer(pairs...)
}

// Apply replaces every registered secret in s.
//
// The replacement is a single left-to-right pass. Replacing one secret after another would let a short
// secret match inside a marker that an earlier replacement had already written.
func (r *Redactor) Apply(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.replacer == nil {
		return s
	}
	return r.replacer.Replace(s)
}

// Error renders an error with every registered secret removed.
func (r *Redactor) Error(err error) string {
	if err == nil {
		return ""
	}
	return r.Apply(err.Error())
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
