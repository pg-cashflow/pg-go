package timeutil

import (
	"sync"
	"time"
)

// Clock provides the current time. It allows injecting deterministic clocks in tests.
type Clock interface {
	Now() time.Time
}

// SystemClock returns the current UTC wall clock time.
type SystemClock struct{}

// Now returns time.Now().UTC().
func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}

// System is the default production Clock instance.
var System = SystemClock{}

// FixedClock returns a fixed instant in UTC.
type FixedClock struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFixedClock initializes a FixedClock set to the given instant, normalized to UTC.
func NewFixedClock(t time.Time) *FixedClock {
	return &FixedClock{now: t.UTC()}
}

// Now returns the current fixed instant in UTC.
func (f *FixedClock) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

// Set updates the fixed instant, normalized to UTC.
func (f *FixedClock) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}

// Advance advances the fixed instant by duration d.
func (f *FixedClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
