package api

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestRequireKYCOnboardingPassed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name      string
		nilSvc    bool
		setTenant bool
		status    *domain.KYCVerification
		statusErr error
		wantCode  int
	}{
		{"verified passes", false, true, &domain.KYCVerification{Status: domain.KYCStatusVerified}, nil, http.StatusOK},
		{"pending owner review passes", false, true, &domain.KYCVerification{Status: domain.KYCStatusPendingManualReview}, nil, http.StatusOK},
		{"no verification blocked", false, true, nil, nil, http.StatusForbidden},
		{"status lookup error blocked", false, true, nil, errors.New("db unavailable"), http.StatusForbidden},
		{"nil kyc service blocked", true, true, nil, nil, http.StatusForbidden},
		{"no tenant in context blocked", false, false, &domain.KYCVerification{Status: domain.KYCStatusVerified}, nil, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var svc KYCService
			if !tt.nilSvc {
				svc = &fakeKYCSvc{statusFn: func(_ context.Context, _ uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
					return tt.status, nil, tt.statusErr
				}}
			}
			h := &Handlers{Deps: Deps{KYCSvc: svc}}

			r := gin.New()
			r.Use(func(c *gin.Context) {
				if tt.setTenant {
					c.Set(auth.ContextTenantKey, &domain.Tenant{ID: uuid.New(), PropertyID: uuid.New()})
				}
				c.Next()
			})
			reached := false
			r.GET("/gated", h.RequireKYCOnboardingPassed(), func(c *gin.Context) {
				reached = true
				c.Status(http.StatusOK)
			})

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/gated", nil))

			if w.Code != tt.wantCode {
				t.Fatalf("want %d, got %d: %s", tt.wantCode, w.Code, w.Body.String())
			}
			if tt.wantCode != http.StatusOK && reached {
				t.Fatalf("handler ran after the gate denied the request")
			}
		})
	}
}

func TestRefreshCookieSecureFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		appEnv     string
		tls        bool
		forwarded  string
		wantSecure bool
	}{
		{"production always secure", "production", false, "", true},
		{"dev plain http not secure", "development", false, "", false},
		{"dev direct TLS secure", "development", true, "", true},
		{"dev behind proxy https secure", "development", false, "https", true},
		{"dev proxy says http not secure", "development", false, "http", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handlers{Deps: Deps{AppEnv: tt.appEnv}}
			req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}
			if tt.forwarded != "" {
				req.Header.Set("X-Forwarded-Proto", tt.forwarded)
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			h.setRefreshCookie(c, "opaque-token", time.Now().Add(time.Hour))

			var secure bool
			for _, ck := range w.Result().Cookies() {
				if ck.Name == RefreshCookieName {
					secure = ck.Secure
				}
			}
			if secure != tt.wantSecure {
				t.Fatalf("Secure: want %v, got %v (cookies: %v)", tt.wantSecure, secure, w.Header().Values("Set-Cookie"))
			}
		})
	}
}
