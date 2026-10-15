package api

import (
	"strings"
	"sync"

	"golang.org/x/time/rate"
)

// firebasePhoneLimiter limits successful /auth/firebase exchanges per normalized phone.
type firebasePhoneLimiter struct {
	mu    sync.Mutex
	byKey map[string]*rate.Limiter
}

func newFirebasePhoneLimiter() *firebasePhoneLimiter {
	return &firebasePhoneLimiter{byKey: make(map[string]*rate.Limiter)}
}

func (l *firebasePhoneLimiter) allow(phone string) bool {
	phone = normalizePhoneKey(phone)
	if phone == "" {
		return true
	}
	l.mu.Lock()
	lim, ok := l.byKey[phone]
	if !ok {
		lim = rate.NewLimiter(rate.Limit(10.0/60.0), 15)
		l.byKey[phone] = lim
	}
	allowed := lim.Allow()
	l.mu.Unlock()
	return allowed
}

func normalizePhoneKey(phone string) string {
	return strings.TrimSpace(phone)
}

var defaultFirebasePhoneLimiter = newFirebasePhoneLimiter()
