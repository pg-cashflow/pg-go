package api

import (
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type firebaseLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// firebasePhoneLimiter limits successful /auth/firebase exchanges per normalized phone.
// Bounded capacity with TTL sweep and oldest entry eviction prevents unbounded memory growth.
type firebasePhoneLimiter struct {
	mu         sync.Mutex
	byKey      map[string]*firebaseLimiterEntry
	maxEntries int
	ttl        time.Duration
}

func newFirebasePhoneLimiter() *firebasePhoneLimiter {
	return &firebasePhoneLimiter{
		byKey:      make(map[string]*firebaseLimiterEntry),
		maxEntries: 10000,
		ttl:        10 * time.Minute,
	}
}

func (l *firebasePhoneLimiter) allow(phone string) bool {
	phone = normalizePhoneKey(phone)
	if phone == "" {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.byKey[phone]
	if !ok {
		if len(l.byKey) >= l.maxEntries {
			for k, v := range l.byKey {
				if now.Sub(v.lastSeen) > l.ttl {
					delete(l.byKey, k)
				}
			}
		}
		if len(l.byKey) >= l.maxEntries {
			var oldestKey string
			var oldestTime time.Time
			for k, v := range l.byKey {
				if oldestKey == "" || v.lastSeen.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.lastSeen
				}
			}
			if oldestKey != "" {
				delete(l.byKey, oldestKey)
			}
		}
		e = &firebaseLimiterEntry{
			limiter:  rate.NewLimiter(rate.Limit(10.0/60.0), 15),
			lastSeen: now,
		}
		l.byKey[phone] = e
	} else {
		e.lastSeen = now
	}
	return e.limiter.Allow()
}

// Allow implements auth.PhoneLimiter.
func (l *firebasePhoneLimiter) Allow(phone string) bool {
	return l.allow(phone)
}

func normalizePhoneKey(phone string) string {
	return strings.TrimSpace(phone)
}

var defaultFirebasePhoneLimiter = newFirebasePhoneLimiter()

// DefaultFirebasePhoneLimiter is the package-level phone limiter instance for auth flows.
var DefaultFirebasePhoneLimiter = defaultFirebasePhoneLimiter
