package redact

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestApply(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		in      string
		want    string
	}{
		{"nothing registered", nil, "plain text", "plain text"},
		{"single secret", []string{"s3cr3t-value"}, "token s3cr3t-value failed", "token " + Marker + " failed"},
		{
			name:    "every occurrence",
			secrets: []string{"s3cr3t-value"},
			in:      "s3cr3t-value and s3cr3t-value",
			want:    Marker + " and " + Marker,
		},
		{
			name:    "several secrets",
			secrets: []string{"first-secret", "second-secret"},
			in:      "first-secret then second-secret",
			want:    Marker + " then " + Marker,
		},
		{
			// A secret that contains another must not leave the remainder visible.
			name:    "overlapping secrets",
			secrets: []string{"abcd", "abcdefgh"},
			in:      "value abcdefgh here",
			want:    "value " + Marker + " here",
		},
		{
			// A short secret must never match inside a marker an earlier replacement wrote.
			name:    "a secret that is a substring of the marker",
			secrets: []string{"mySecretValue1", "dact"},
			in:      "token=mySecretValue1 end",
			want:    "token=" + Marker + " end",
		},
		{
			name:    "the marker itself is left alone",
			secrets: []string{"reda", "s3cr3t-value"},
			in:      "s3cr3t-value",
			want:    Marker,
		},
		{"too short to register", []string{"ab"}, "ab stays", "ab stays"},
		{"empty value is ignored", []string{""}, "unchanged", "unchanged"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r Redactor
			r.Add(tt.secrets...)

			if got := r.Apply(tt.in); got != tt.want {
				t.Errorf("Apply() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestError(t *testing.T) {
	var r Redactor
	r.Add("s3cr3t-value")

	t.Run("nil error", func(t *testing.T) {
		if got := r.Error(nil); got != "" {
			t.Errorf("Error(nil) = %q, want empty", got)
		}
	})

	t.Run("wrapped error", func(t *testing.T) {
		err := fmt.Errorf("request failed: %w", errors.New("Authorization: Token s3cr3t-value"))

		got := r.Error(err)

		if strings.Contains(got, "s3cr3t-value") {
			t.Errorf("Error() = %q, still carries the secret", got)
		}
		if !strings.Contains(got, Marker) {
			t.Errorf("Error() = %q, want the marker", got)
		}
	})
}

// Registering the same value twice must not grow the set or change the result.
func TestAddIsIdempotent(t *testing.T) {
	var r Redactor
	r.Add("s3cr3t-value", "s3cr3t-value")
	r.Add("s3cr3t-value")

	if got, want := r.Apply("s3cr3t-value"), Marker; got != want {
		t.Errorf("Apply() = %q, want %q", got, want)
	}
	if len(r.secrets) != 1 {
		t.Errorf("secrets = %v, want one entry", r.secrets)
	}
}

// Providers may resolve credentials concurrently while output is being written.
func TestConcurrentUse(t *testing.T) {
	var (
		r  Redactor
		wg sync.WaitGroup
	)
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); r.Add(fmt.Sprintf("secret-value-%02d", i)) }(i)
		go func() { defer wg.Done(); _ = r.Apply("secret-value-07 in a message") }()
	}
	wg.Wait()

	if got := r.Apply("secret-value-07 in a message"); strings.Contains(got, "secret-value-07") {
		t.Errorf("Apply() = %q, want the secret removed", got)
	}
}
