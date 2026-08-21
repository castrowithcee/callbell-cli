package ratelimit

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

// clock is a controllable time source: every recorded sleep advances it, so a test proves the spacing
// without waiting for it.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	waited []time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waited = append(c.waited, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *clock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waited...)
}

// The first slot is free and every further slot of the same limiter is spaced by the interval.
func TestWaitSpacesSlotsByTheInterval(t *testing.T) {
	const interval = 500 * time.Millisecond
	c := &clock{now: time.Unix(0, 0)}
	limiter := New(interval, c.Now, c.Sleep)

	for i := 0; i < 3; i++ {
		if err := limiter.Wait(context.Background()); err != nil {
			t.Fatalf("Wait() = %v", err)
		}
	}
	if got := c.waits(); !reflect.DeepEqual(got, []time.Duration{interval, interval}) {
		t.Errorf("waits = %v, want two waits of %v", got, interval)
	}
}

// A cancelled request ends in the limiter instead of turning into a provider call.
func TestWaitReportsACancelledContext(t *testing.T) {
	limiter := New(time.Second, time.Now, Sleep)
	limiter.HoldFor(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Wait(ctx); err == nil {
		t.Error("Wait() accepted a cancelled request")
	}

	// A free slot reports the cancellation too, so no caller proceeds on a dead request.
	free := New(0, time.Now, Sleep)
	if err := free.Wait(ctx); err == nil {
		t.Error("Wait() accepted a cancelled request on a free slot")
	}
}

// HoldFor pushes the next slot out by the reported delay and never pulls an existing reservation in.
func TestHoldForDelaysTheNextSlot(t *testing.T) {
	c := &clock{now: time.Unix(0, 0)}
	limiter := New(0, c.Now, c.Sleep)

	limiter.HoldFor(0)
	limiter.HoldFor(-time.Second)
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() = %v", err)
	}
	if got := c.waits(); len(got) != 0 {
		t.Fatalf("waits = %v, want no wait after a hold of zero", got)
	}

	limiter.HoldFor(30 * time.Second)
	limiter.HoldFor(time.Second)
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() = %v", err)
	}
	if got := c.waits(); !reflect.DeepEqual(got, []time.Duration{30 * time.Second}) {
		t.Errorf("waits = %v, want the longer hold to survive the shorter one", got)
	}
}

// One key keeps one budget, two keys never share one, and the registry stores no copy of a key.
func TestRegistryKeepsOneBudgetPerKey(t *testing.T) {
	registry := NewRegistry(time.Second)
	const first = "canary-ratelimit-key-one-3f81"
	const second = "canary-ratelimit-key-two-5c02"

	if registry.For(first) != registry.For(first) {
		t.Error("one key does not keep one budget")
	}
	if registry.For(first) == registry.For(second) {
		t.Error("two keys share one budget")
	}
	for digest := range registry.limiters {
		if string(digest[:]) == first || string(digest[:]) == second {
			t.Error("the registry stored a key instead of its digest")
		}
	}
}

// Replace installs a limiter for one key and restores exactly the previous state afterwards.
func TestRegistryReplaceRestoresThePreviousState(t *testing.T) {
	registry := NewRegistry(time.Second)
	const key = "canary-ratelimit-key-replace-9a4d"

	free := New(0, time.Now, Sleep)
	restore := registry.Replace(key, free)
	if registry.For(key) != free {
		t.Fatal("Replace() did not install the limiter")
	}
	restore()
	if _, ok := registry.limiters[digestOf(key)]; ok {
		t.Error("restore() left an unknown key behind")
	}

	original := registry.For(key)
	restore = registry.Replace(key, free)
	restore()
	if registry.For(key) != original {
		t.Error("restore() did not put the original limiter back")
	}
}
