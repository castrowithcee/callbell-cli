// Package ratelimit spaces the requests that share one provider credential.
//
// A provider API counts its limit per key, across every endpoint that key can reach, so the spacing
// belongs to the key and not to a single client or operation. A Registry keeps one Limiter per key for
// the lifetime of the process; a Limiter reserves the next slot and blocks until it is due.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"
)

// Limiter spaces the calls of one credential by a fixed minimum interval. Its clock and its sleep are
// injected, so a test can prove the spacing without waiting for it.
type Limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

// New returns a limiter that keeps at least interval between two reserved slots. now and sleep are the
// test seam; production passes time.Now and Sleep.
func New(interval time.Duration, now func() time.Time,
	sleep func(context.Context, time.Duration) error) *Limiter {
	return &Limiter{interval: interval, now: now, sleep: sleep}
}

// Wait reserves the next slot and blocks until it is due. It returns the context error when the request
// ends while it waits, so a cancelled request never becomes a provider call.
func (l *Limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	now := l.now()
	wait := l.next.Sub(now)
	if wait < 0 {
		wait = 0
	}
	l.next = now.Add(wait + l.interval)
	l.mu.Unlock()

	if wait <= 0 {
		return ctx.Err()
	}
	return l.sleep(ctx, wait)
}

// HoldFor delays the next slot by at least d, measured from now. A provider calls it when the API itself
// reported that the budget of this key is exhausted; a value of zero or less changes nothing.
func (l *Limiter) HoldFor(d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := l.now().Add(d); until.After(l.next) {
		l.next = until
	}
}

// Sleep is the production sleep of a limiter: it waits for d unless the context ends first.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Registry holds the limiter of every credential one provider has seen in this process.
//
// callbell-dev: the budget is per key and per process. A shared bucket across concurrent Callbell
// processes would need external state; add it when one machine really runs several at once.
type Registry struct {
	mu       sync.Mutex
	interval time.Duration
	limiters map[[sha256.Size]byte]*Limiter
}

// NewRegistry returns a registry whose limiters keep at least interval between two requests of one key.
func NewRegistry(interval time.Duration) *Registry {
	return &Registry{interval: interval, limiters: map[[sha256.Size]byte]*Limiter{}}
}

// For returns the limiter of one credential value, creating it on first use. The map is keyed by a
// digest, so it never holds a second copy of the credential itself.
func (r *Registry) For(key string) *Limiter {
	digest := digestOf(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.limiters[digest]; ok {
		return existing
	}
	created := New(r.interval, time.Now, Sleep)
	r.limiters[digest] = created
	return created
}

// Replace installs limiter for key and returns the function that restores the previous state. It is the
// seam a provider test uses to run several requests without sleeping; production code only calls For.
func (r *Registry) Replace(key string, limiter *Limiter) (restore func()) {
	digest := digestOf(key)
	r.mu.Lock()
	previous, existed := r.limiters[digest]
	r.limiters[digest] = limiter
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if existed {
			r.limiters[digest] = previous
			return
		}
		delete(r.limiters, digest)
	}
}

// digestOf is what the registry stores instead of the credential value itself.
func digestOf(key string) [sha256.Size]byte { return sha256.Sum256([]byte(key)) }
