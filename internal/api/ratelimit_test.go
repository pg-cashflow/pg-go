package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
