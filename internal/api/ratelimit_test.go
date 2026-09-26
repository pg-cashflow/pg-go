package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestIPRateLimit(t *testing.T) {
	r := gin.New()
	// 2 requests burst, 1 req/sec replenish
	r.GET("/limited", ipRateLimit(1.0, 2), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/limited", nil)
		req.RemoteAddr = "192.0.2.1:12345"
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}

	// 3rd request should exceed burst limit and get 429
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/limited", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request: expected 429 Too Many Requests, got %d", w.Code)
	}

	// A different IP should succeed
	wOther := httptest.NewRecorder()
	reqOther := httptest.NewRequest(http.MethodGet, "/limited", nil)
	reqOther.RemoteAddr = "192.0.2.2:12345"
	r.ServeHTTP(wOther, reqOther)
	if wOther.Code != http.StatusOK {
		t.Fatalf("different IP: expected 200, got %d", wOther.Code)
	}
}

func TestIPRateLimit_TTLSweepAndCapacity(t *testing.T) {
	r := gin.New()
	// Max 2 entries, 20ms TTL
	r.GET("/bounded", ipRateLimitBounded(10.0, 10, 2, 20*time.Millisecond), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// Send from IP 1
	w1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/bounded", nil)
	req1.RemoteAddr = "10.0.0.1:1234"
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("IP 1 failed: %d", w1.Code)
	}

	// Send from IP 2 (now at capacity: 2 entries)
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/bounded", nil)
	req2.RemoteAddr = "10.0.0.2:1234"
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("IP 2 failed: %d", w2.Code)
	}

	// Sleep 25ms to let entries expire past TTL
	time.Sleep(25 * time.Millisecond)

	// Send from IP 3: should trigger sweep and succeed
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/bounded", nil)
	req3.RemoteAddr = "10.0.0.3:1234"
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("IP 3 failed after sweep: %d", w3.Code)
	}

	// Send from IP 4 immediately without sleeping (exceeds maxEntries=2 without expiry):
	// should evict oldest and succeed without panicking or growing unbounded
	w4 := httptest.NewRecorder()
	req4 := httptest.NewRequest(http.MethodGet, "/bounded", nil)
	req4.RemoteAddr = "10.0.0.4:1234"
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("IP 4 failed with capacity eviction: %d", w4.Code)
	}
}
