package api

import (
	"testing"
	"time"
)

func TestFirebasePhoneLimiter_AllowAndRateLimit(t *testing.T) {
	limiter := newFirebasePhoneLimiter()
	phone := "+919876543210"

	// 15 burst allowed
	for i := 0; i < 15; i++ {
		if !limiter.Allow(phone) {
			t.Fatalf("request %d should have been allowed", i+1)
		}
	}

	// 16th should be rejected immediately due to burst limit
	if limiter.Allow(phone) {
		t.Fatalf("request 16 should have been rejected")
	}
}

func TestFirebasePhoneLimiter_EvictionOnCapacity(t *testing.T) {
	limiter := &firebasePhoneLimiter{
		byKey:      make(map[string]*firebaseLimiterEntry),
		maxEntries: 3,
		ttl:        50 * time.Millisecond,
	}

	// Insert 3 entries
	limiter.Allow("+919000000001")
	limiter.Allow("+919000000002")
	limiter.Allow("+919000000003")

	if len(limiter.byKey) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(limiter.byKey))
	}

	// Wait for TTL to expire
	time.Sleep(60 * time.Millisecond)

	// Adding a 4th entry should trigger sweep and purge expired entries
	limiter.Allow("+919000000004")

	limiter.mu.Lock()
	count := len(limiter.byKey)
	limiter.mu.Unlock()

	// Expired entries should have been swept
	if count > 2 {
		t.Errorf("expected <= 2 entries after TTL sweep, got %d", count)
	}
}

func TestFirebasePhoneLimiter_OldestEvictionWhenFull(t *testing.T) {
	limiter := &firebasePhoneLimiter{
		byKey:      make(map[string]*firebaseLimiterEntry),
		maxEntries: 2,
		ttl:        1 * time.Hour, // Long TTL so sweep won't delete by age
	}

	limiter.Allow("+919000000001")
	time.Sleep(10 * time.Millisecond)
	limiter.Allow("+919000000002")

	// Now insert 3rd; capacity is 2 and none expired, so oldest (+919000000001) must be evicted
	time.Sleep(10 * time.Millisecond)
	limiter.Allow("+919000000003")

	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	if len(limiter.byKey) > 2 {
		t.Errorf("expected map bounded to 2 entries, got %d", len(limiter.byKey))
	}
	if _, found := limiter.byKey["+919000000001"]; found {
		t.Errorf("expected oldest key +919000000001 to be evicted")
	}
	if _, found := limiter.byKey["+919000000003"]; !found {
		t.Errorf("expected newest key +919000000003 to be present")
	}
}
