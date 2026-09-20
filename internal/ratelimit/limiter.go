// Package ratelimit provides a minimal in-memory sliding-window rate
// limiter. It keeps no disk state — everything is lost on restart, which is
// fine for its only use: bounding abuse attempts, not enforcing a durable
// quota.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter allows up to Max events per key within a sliding time Window.
type Limiter struct {
	max    int
	window time.Duration

	mu   sync.Mutex
	hits map[string][]time.Time
}

// New creates a Limiter allowing up to max events per key in window. A
// non-positive max disables limiting (Allow always returns true).
func New(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, hits: make(map[string][]time.Time)}
}

// Allow reports whether one more event for key is permitted right now, and
// records it if so.
func (l *Limiter) Allow(key string) bool {
	if l.max <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	kept := recent(l.hits[key], now.Add(-l.window))
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// GC drops keys with no events inside the window as of now, bounding memory
// for keys (e.g. IPs) that stop appearing. Allow alone never reclaims memory
// for idle keys, so callers should invoke GC periodically.
func (l *Limiter) GC(now time.Time) {
	cutoff := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, times := range l.hits {
		kept := recent(times, cutoff)
		if len(kept) == 0 {
			delete(l.hits, k)
		} else {
			l.hits[k] = kept
		}
	}
}

// recent filters times to those after cutoff, reusing times' backing array.
func recent(times []time.Time, cutoff time.Time) []time.Time {
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}
