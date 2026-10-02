package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSecurityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(SecurityHeaders("production"))
	r.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("expected X-Content-Type-Options: nosniff, got %q", got)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("expected X-Frame-Options: DENY, got %q", got)
	}
	if got := w.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Errorf("expected Referrer-Policy: strict-origin-when-cross-origin, got %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy-Report-Only"); !strings.Contains(got, "default-src 'self'") {
		t.Errorf("expected CSP header with default-src 'self', got %q", got)
	}
	if got := w.Header().Get("Strict-Transport-Security"); !strings.Contains(got, "max-age=31536000") {
		t.Errorf("expected HSTS header in production, got %q", got)
	}
}

func TestSpoofedXForwardedForUntrusted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Trust no proxies
	_ = r.SetTrustedProxies(nil)

	var detectedIP string
	r.GET("/whoami", func(c *gin.Context) {
		detectedIP = c.ClientIP()
		c.String(http.StatusOK, detectedIP)
	})

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.195, 198.51.100.1")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Because proxy is untrusted, ClientIP() must evaluate to RemoteAddr IP (192.0.2.1), NOT spoofed X-Forwarded-For
	if detectedIP != "192.0.2.1" {
		t.Errorf("expected detected IP 192.0.2.1, got %q (spoofed header was trusted!)", detectedIP)
	}
}

func TestMaxBodyBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(MaxBodyBytes(100)) // Limit to 100 bytes
	r.POST("/upload", func(c *gin.Context) {
		buf := make([]byte, 200)
		_, err := c.Request.Body.Read(buf)
		if err != nil {
			c.String(http.StatusRequestEntityTooLarge, "body too large: "+err.Error())
			return
		}
		c.String(http.StatusOK, "read ok")
	})

	// Send 150 bytes (exceeds 100 limit)
	largeBody := bytes.Repeat([]byte("A"), 150)
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(largeBody))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 Request Entity Too Large, got %d", w.Code)
	}
}
