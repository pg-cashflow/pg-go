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
			bodyContains: "<title>pg-app</title>",
		},
		{
			name:         "SPA Client Route Fallback",
			method:       "GET",
			path:         "/tenant/dashboard",
			expectedCode: http.StatusOK,
			expectedType: "text/html",
			bodyContains: "<title>pg-app</title>",
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
