package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
)

type stubRouterMagicLink struct{}

func (s *stubRouterMagicLink) CreatePaymentToken(_ context.Context, _ uuid.UUID) (string, error) {
	return "", nil
}

func (s *stubRouterMagicLink) ResolveToken(_ context.Context, raw string) (*magiclink.DueView, error) {
	if raw == "expired-token" {
		return nil, magiclink.ErrTokenExpired
	}
	room := "101"
	return &magiclink.DueView{
		PropertyName: "Green PG",
		OwnerName:    "Ramesh",
		RoomNumber:   &room,
		AmountPaise:  1500000,
		Due:          domain.Due{DueCode: "0402", Status: domain.DueStatusPending},
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}, nil
}

func TestRedactingLogFormatter(t *testing.T) {
	buf := new(bytes.Buffer)
	r := gin.New()
	r.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		Output:    buf,
		Formatter: redactingLogFormatter,
	}))

	r.GET("/p/:token", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	secretToken := "secret-magic-token-xyz-12345"
	req := httptest.NewRequest(http.MethodGet, "/p/"+secretToken, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	logOutput := buf.String()
	if strings.Contains(logOutput, secretToken) {
		t.Fatalf("security violation: log output leaked magic link token %q: %s", secretToken, logOutput)
	}

	if !strings.Contains(logOutput, "/p/[REDACTED]") {
		t.Fatalf("expected log output to contain /p/[REDACTED], got: %s", logOutput)
	}
}

func TestRouterVerificationSuite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := Deps{
		JWTSecret: "test-secret-key-with-sufficient-length-32",
		MagicLink: &stubRouterMagicLink{},
	}
	r := NewRouter(deps)

	tests := []struct {
		name         string
		method       string
		path         string
		expectedCode int
		expectedType string
		bodyContains string
	}{
		{
			name:         "Root healthz",
			method:       "GET",
			path:         "/healthz",
			expectedCode: http.StatusOK,
			expectedType: "application/json",
			bodyContains: `{"status":"ok"}`,
		},
		{
			name:         "API healthz",
			method:       "GET",
			path:         "/api/healthz",
			expectedCode: http.StatusOK,
			expectedType: "application/json",
			bodyContains: `{"status":"ok"}`,
		},
		{
			name:         "Root SPA index",
			method:       "GET",
			path:         "/",
			expectedCode: http.StatusOK,
			expectedType: "text/html",
			bodyContains: "id=\"root\"",
		},
		{
			name:         "SPA Client Route Fallback",
			method:       "GET",
			path:         "/tenant/dashboard",
			expectedCode: http.StatusOK,
			expectedType: "text/html",
			bodyContains: "id=\"root\"",
		},
		{
			name:         "API Route Gated by Auth (Returns 401 JSON, NOT HTML)",
			method:       "GET",
			path:         "/api/owner/properties",
			expectedCode: http.StatusUnauthorized,
			expectedType: "application/json",
			bodyContains: "missing bearer token",
		},
		{
			name:         "Standalone HTML Payment Page (Not SPA, Not JSON)",
			method:       "GET",
			path:         "/p/valid-token",
			expectedCode: http.StatusOK,
			expectedType: "text/html",
			bodyContains: "Green PG",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.expectedCode {
				t.Fatalf("expected status %d, got %d (body: %s)", tt.expectedCode, w.Code, w.Body.String())
			}
			ct := w.Header().Get("Content-Type")
			if !strings.Contains(ct, tt.expectedType) {
				t.Fatalf("expected Content-Type containing %q, got %q", tt.expectedType, ct)
			}
			if !strings.Contains(w.Body.String(), tt.bodyContains) {
				t.Fatalf("expected body to contain %q, got %q", tt.bodyContains, w.Body.String())
			}
		})
	}
}

func TestLegacyAppShellEnabled(t *testing.T) {
	if legacyAppShellEnabled(Deps{AppEnv: "production"}) {
		t.Fatal("expected legacyAppShellEnabled to be false in production")
	}
	if legacyAppShellEnabled(Deps{AppEnv: "Production"}) {
		t.Fatal("expected legacyAppShellEnabled to be false in Production (case insensitive)")
	}
	if !legacyAppShellEnabled(Deps{AppEnv: "development"}) {
		t.Fatal("expected legacyAppShellEnabled to be true in development")
	}
	if !legacyAppShellEnabled(Deps{AppEnv: ""}) {
		t.Fatal("expected legacyAppShellEnabled to be true when AppEnv is empty")
	}
}

type countingAuthUserRepo struct {
	user  *domain.User
	calls int
}

func (c *countingAuthUserRepo) GetByID(_ context.Context, _ uuid.UUID) (*domain.User, error) {
	c.calls++
	return c.user, nil
}

func (c *countingAuthUserRepo) IsFamilyActive(_ context.Context, _ uuid.UUID) (bool, error) {
	return true, nil
}

func (c *countingAuthUserRepo) IncrementTokenVersion(_ context.Context, _ uuid.UUID) error {
	return nil
}

func TestAuthoritativeAuthWiredOnSensitiveRoutes_F2(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"
	uid := uuid.New()
	pid := uuid.New()
	user := &domain.User{
		ID:           uid,
		Role:         domain.RoleOwner,
		PropertyID:   &pid,
		TokenVersion: 1,
	}
	userRepo := &countingAuthUserRepo{user: user}
	r := NewRouter(Deps{
		JWTSecret:    secret,
		AuthUserRepo: userRepo,
		MagicLink:    &stubRouterMagicLink{},
	})

	tok, err := auth.IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	// 1. Warm the positive token-version cache with an ordinary read
	req1 := httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	req1.Header.Set("Authorization", "Bearer "+tok)
	r.ServeHTTP(httptest.NewRecorder(), req1)
	if userRepo.calls != 1 {
		t.Fatalf("expected 1 GetByID call on cold cache, got %d", userRepo.calls)
	}

	// 2. Second ordinary read uses cache (0 new DB calls)
	req2 := httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	r.ServeHTTP(httptest.NewRecorder(), req2)
	if userRepo.calls != 1 {
		t.Fatalf("expected cached read to make 0 new GetByID calls, got total %d", userRepo.calls)
	}

	// 3. Authoritative endpoints (/api/auth/identities, /api/auth/sessions) MUST bypass cache
	// even though they are GET requests not matching isFinancialMutation()!
	for _, path := range []string{"/api/auth/identities", "/api/auth/sessions"} {
		before := userRepo.calls
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		r.ServeHTTP(httptest.NewRecorder(), req)
		if userRepo.calls != before+1 {
			t.Fatalf("expected %s to bypass auth cache via RequireAuthoritativeAuth (before=%d, after=%d)", path, before, userRepo.calls)
		}
	}
}
