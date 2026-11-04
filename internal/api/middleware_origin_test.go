package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequireSameOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newRouter := func(allowedOrigins []string) *gin.Engine {
		r := gin.New()
		r.POST("/api/auth/firebase", RequireSameOrigin(allowedOrigins), func(c *gin.Context) {
			c.String(http.StatusOK, "ok")
		})
		return r
	}

	t.Run("No Origin and no Sec-Fetch-Site: pass through (native app / curl)", func(t *testing.T) {
		r := newRouter(nil)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("Sec-Fetch-Site: same-origin → pass", func(t *testing.T) {
		r := newRouter(nil)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("Sec-Fetch-Site: none → pass (top-level navigation form)", func(t *testing.T) {
		r := newRouter(nil)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		req.Header.Set("Sec-Fetch-Site", "none")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("Sec-Fetch-Site: cross-site → 403", func(t *testing.T) {
		r := newRouter(nil)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Origin", "https://evil.example.com")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for cross-site, got %d", w.Code)
		}
	})

	t.Run("Sec-Fetch-Site: same-site → 403 (not same-origin)", func(t *testing.T) {
		r := newRouter(nil)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		req.Header.Set("Sec-Fetch-Site", "same-site")
		req.Header.Set("Origin", "https://sub.example.com")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for same-site, got %d", w.Code)
		}
	})

	t.Run("Origin matches Host header (no allowed-list) → pass", func(t *testing.T) {
		r := newRouter(nil)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		req.Host = "pg.example.com"
		req.Header.Set("Origin", "https://pg.example.com")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("Origin does not match Host header (no allowed-list) → 403", func(t *testing.T) {
		r := newRouter(nil)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		req.Host = "pg.example.com"
		req.Header.Set("Origin", "https://evil.example.com")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for origin/host mismatch, got %d", w.Code)
		}
	})

	t.Run("Origin null → 403", func(t *testing.T) {
		r := newRouter(nil)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		req.Header.Set("Origin", "null")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for null origin, got %d", w.Code)
		}
	})

	t.Run("Origin in allowed-list → pass", func(t *testing.T) {
		r := newRouter([]string{"https://app.pg.example.com"})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		req.Header.Set("Origin", "https://app.pg.example.com")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("Origin not in allowed-list → 403", func(t *testing.T) {
		r := newRouter([]string{"https://app.pg.example.com"})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		req.Header.Set("Origin", "https://attacker.example.com")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", w.Code)
		}
	})

	t.Run("Malformed Origin → 403", func(t *testing.T) {
		r := newRouter(nil)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/firebase", nil)
		req.Header.Set("Origin", "not-a-url")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for malformed origin, got %d", w.Code)
		}
	})
}

// TestAuthLogoutClearsOnRevokeError ensures AuthLogout returns 202 Accepted and clears
// the cookie when RevokeSession fails (truthful reporting of partial revocation failure).
func TestAuthLogoutClearsOnRevokeError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := &Handlers{
		Deps: Deps{
			AppEnv: "test",
			Auth: &stubAuthRevokeErrService{
				revokeErr: errors.New("database connection refused"),
			},
		},
	}
	r.POST("/auth/logout", h.AuthLogout)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: RefreshCookieName, Value: "some-token"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted from AuthLogout on revocation error, got %d", w.Code)
	}
	cookie := w.Header().Get("Set-Cookie")
	if cookie == "" {
		t.Fatal("expected Set-Cookie header to clear the refresh cookie")
	}
}

type stubAuthRevokeErrService struct {
	stubAuthService
	revokeErr error
}

func (s *stubAuthRevokeErrService) RevokeSession(_ context.Context, _ string) error {
	return s.revokeErr
}
