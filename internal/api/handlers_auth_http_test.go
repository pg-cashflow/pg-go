package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type fakeAuthRefreshService struct {
	stubPIIAuthService
	rotateErr error
}

func (s *fakeAuthRefreshService) RotateRefreshToken(_ context.Context, token string) (string, string, *domain.User, error) {
	if s.rotateErr != nil {
		return "", "", nil, s.rotateErr
	}
	user := &domain.User{ID: uuid.New(), Role: domain.RoleOwner}
	return "fresh-access-token", "rotated-refresh-token", user, nil
}

// TestAuthHandlers_T4d tests AuthRefresh and public auth edge cases
func TestAuthHandlers_T4d(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"

	authSvc := &fakeAuthRefreshService{}
	h := &api.Handlers{
		Deps: api.Deps{
			JWTSecret: jwtSecret,
			Auth:      authSvc,
		},
	}

	router := gin.New()
	router.POST("/auth/refresh", h.AuthRefresh)

	t.Run("AuthRefresh: Missing cookie returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for missing refresh token, got %d", w.Code)
		}
	})

	t.Run("AuthRefresh: Valid refresh token cookie returns 200 and rotates cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		req.AddCookie(&http.Cookie{
			Name:  api.RefreshCookieName,
			Value: "valid-active-refresh-token",
		})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		cookieHeader := w.Header().Get("Set-Cookie")
		if !strings.Contains(cookieHeader, "rotated-refresh-token") {
			t.Fatalf("expected rotated-refresh-token in Set-Cookie, got: %s", cookieHeader)
		}
	})

	t.Run("AuthRefresh: Replayed token returns 401 with CodeAuthAccessRevoked and clears cookie", func(t *testing.T) {
		authSvc.rotateErr = auth.ErrReplayDetected
		defer func() { authSvc.rotateErr = nil }()

		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		req.AddCookie(&http.Cookie{
			Name:  api.RefreshCookieName,
			Value: "replayed-stale-refresh-token",
		})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for replayed token, got %d body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), string(apierr.CodeAuthAccessRevoked)) {
			t.Fatalf("expected CodeAuthAccessRevoked in body, got: %s", w.Body.String())
		}
		cookieHeader := w.Header().Get("Set-Cookie")
		if !strings.Contains(cookieHeader, "Max-Age=0") && !strings.Contains(cookieHeader, "expires=") {
			t.Fatalf("expected cookie cleared on replay, got Set-Cookie: %s", cookieHeader)
		}
	})
}
