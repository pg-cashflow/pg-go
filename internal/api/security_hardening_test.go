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
	if got := w.Header().Get("Permissions-Policy"); !strings.Contains(got, "camera=(self)") {
		t.Errorf("expected Permissions-Policy to permit camera=(self), got %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
		t.Errorf("expected CSP header with default-src 'self', got %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); strings.Contains(got, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("expected CSP script-src to omit 'unsafe-inline', got %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); strings.Contains(got, "fonts.googleapis.com") {
		t.Errorf("expected CSP to omit fonts.googleapis.com (fonts are self-hosted), got %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); strings.Contains(got, "fonts.gstatic.com") {
		t.Errorf("expected CSP to omit fonts.gstatic.com (fonts are self-hosted), got %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); strings.Contains(got, "sandbox.cashfree.com") {
		t.Errorf("expected production CSP to omit sandbox.cashfree.com, got %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); strings.Contains(got, "payments-test.cashfree.com") {
		t.Errorf("expected production CSP to omit payments-test.cashfree.com, got %q", got)
	}
	if got := w.Header().Get("Strict-Transport-Security"); !strings.Contains(got, "max-age=31536000") {
		t.Errorf("expected HSTS header in production, got %q", got)
	}
}

func TestSecurityHeaders_Development(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(SecurityHeaders("development"))
	r.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox.cashfree.com") {
		t.Errorf("expected development CSP to contain sandbox.cashfree.com, got %q", got)
	}
	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("expected development to omit HSTS, got %q", got)
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

// TestNoStoreAPI verifies that the NoStoreAPI middleware sets the required
// Cache-Control, Pragma and Expires headers on every response — including
// error responses — so private API payloads are never stored by browsers,
// CDNs or shared proxies.
func TestNoStoreAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name   string
		status int
	}{
		{"200 success", http.StatusOK},
		{"400 bad request", http.StatusBadRequest},
		{"401 unauthorized", http.StatusUnauthorized},
		{"403 forbidden", http.StatusForbidden},
		{"404 not found", http.StatusNotFound},
		{"500 internal error", http.StatusInternalServerError},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(NoStoreAPI())
			r.GET("/api/test", func(c *gin.Context) {
				c.JSON(tc.status, gin.H{"ok": true})
			})

			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			cc := w.Header().Get("Cache-Control")
			if !strings.Contains(cc, "no-store") {
				t.Errorf("status %d: expected Cache-Control to contain no-store, got %q", tc.status, cc)
			}
			if !strings.Contains(cc, "private") {
				t.Errorf("status %d: expected Cache-Control to contain private, got %q", tc.status, cc)
			}
			if got := w.Header().Get("Pragma"); got != "no-cache" {
				t.Errorf("status %d: expected Pragma: no-cache, got %q", tc.status, got)
			}
			if got := w.Header().Get("Expires"); got != "0" {
				t.Errorf("status %d: expected Expires: 0, got %q", tc.status, got)
			}
		})
	}
}

// TestNoStoreAPI_APIGroupOnly verifies that NoStoreAPI is wired to the /api
// group and does not affect static-asset or landing-page routes.
func TestNoStoreAPI_APIGroupOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	// Simulate: /api group gets NoStoreAPI; root does not.
	api := r.Group("/api", NoStoreAPI())
	api.GET("/data", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.GET("/landing", func(c *gin.Context) { c.String(http.StatusOK, "public") })

	// /api/data must have no-store.
	req1 := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if cc := w1.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("/api route: expected Cache-Control no-store, got %q", cc)
	}

	// /landing must NOT have Cache-Control set by NoStoreAPI.
	req2 := httptest.NewRequest(http.MethodGet, "/landing", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if cc := w2.Header().Get("Cache-Control"); strings.Contains(cc, "no-store") {
		t.Errorf("/landing route: expected no Cache-Control no-store, got %q", cc)
	}
}
