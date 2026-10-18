package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type fakeOnboardingSettingsRepo struct {
	settings map[uuid.UUID]*domain.PropertyOnboardingSettings
}

func (f *fakeOnboardingSettingsRepo) Get(ctx context.Context, propertyID uuid.UUID) (*domain.PropertyOnboardingSettings, error) {
	if s, ok := f.settings[propertyID]; ok {
		return s, nil
	}
	return nil, nil
}

func (f *fakeOnboardingSettingsRepo) Update(ctx context.Context, propertyID uuid.UUID, patch postgres.OnboardingSettingsPatch) (*domain.PropertyOnboardingSettings, error) {
	return f.Get(ctx, propertyID)
}

func TestRequireKYCVerified_Scenarios(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()

	makeContext := func(h *Handlers) (*gin.Context, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/test", nil)
		c.Set(auth.ContextTenantKey, &domain.Tenant{
			ID:         tenantID,
			PropertyID: propID,
			Status:     domain.TenantStatusActive,
		})
		return c, w
	}

	t.Run("503 when KYCSvc is nil", func(t *testing.T) {
		h := &Handlers{}
		c, w := makeContext(h)

		h.RequireKYCVerified()(c)

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", w.Code)
		}
	})

	t.Run("403 when KYC record is missing", func(t *testing.T) {
		h := &Handlers{
			Deps: Deps{
				KYCSvc: &fakeKYCSvc{
					statusFn: func(ctx context.Context, tid uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
						return nil, nil, nil
					},
				},
			},
		}
		c, w := makeContext(h)

		h.RequireKYCVerified()(c)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", w.Code)
		}
	})

	t.Run("403 when KYC status is pending", func(t *testing.T) {
		h := &Handlers{
			Deps: Deps{
				KYCSvc: &fakeKYCSvc{
					statusFn: func(ctx context.Context, tid uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
						return &domain.KYCVerification{
							ID:     uuid.New(),
							Status: domain.KYCStatusPending,
						}, nil, nil
					},
				},
			},
		}
		c, w := makeContext(h)

		h.RequireKYCVerified()(c)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", w.Code)
		}
	})

	t.Run("200 when KYC status is verified", func(t *testing.T) {
		now := time.Now()
		h := &Handlers{
			Deps: Deps{
				KYCSvc: &fakeKYCSvc{
					statusFn: func(ctx context.Context, tid uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
						return &domain.KYCVerification{
							ID:         uuid.New(),
							Status:     domain.KYCStatusVerified,
							VerifiedAt: &now,
						}, nil, nil
					},
				},
			},
		}
		w := httptest.NewRecorder()

		called := false
		handler := func(c *gin.Context) {
			called = true
			c.Status(http.StatusOK)
		}

		r := gin.New()
		r.GET("/test", func(c *gin.Context) {
			c.Set(auth.ContextTenantKey, &domain.Tenant{
				ID:         tenantID,
				PropertyID: propID,
				Status:     domain.TenantStatusActive,
			})
			h.RequireKYCVerified()(c)
			if !c.IsAborted() {
				handler(c)
			}
		})

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if !called {
			t.Fatalf("expected downstream handler to be called")
		}
	})
}

func TestRequireKYCIfPolicyEnforced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()

	t.Run("Policy disabled allows unverified tenant", func(t *testing.T) {
		h := &Handlers{
			Deps: Deps{
				OnboardingSettings: &fakeOnboardingSettingsRepo{
					settings: map[uuid.UUID]*domain.PropertyOnboardingSettings{
						propID: {
							PropertyID:          propID,
							RequireKYCBeforePay: false,
						},
					},
				},
				KYCSvc: &fakeKYCSvc{
					statusFn: func(ctx context.Context, tid uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
						return nil, nil, nil
					},
				},
			},
		}

		w := httptest.NewRecorder()
		r := gin.New()
		r.GET("/test", func(c *gin.Context) {
			c.Set(auth.ContextTenantKey, &domain.Tenant{
				ID:         tenantID,
				PropertyID: propID,
				Status:     domain.TenantStatusActive,
			})
			h.RequireKYCIfPolicyEnforced()(c)
			if !c.IsAborted() {
				c.Status(http.StatusOK)
			}
		})

		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("Policy enabled blocks unverified tenant", func(t *testing.T) {
		h := &Handlers{
			Deps: Deps{
				OnboardingSettings: &fakeOnboardingSettingsRepo{
					settings: map[uuid.UUID]*domain.PropertyOnboardingSettings{
						propID: {
							PropertyID:          propID,
							RequireKYCBeforePay: true,
						},
					},
				},
				KYCSvc: &fakeKYCSvc{
					statusFn: func(ctx context.Context, tid uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
						return &domain.KYCVerification{
							ID:     uuid.New(),
							Status: domain.KYCStatusPending,
						}, nil, nil
					},
				},
			},
		}

		w := httptest.NewRecorder()
		r := gin.New()
		r.GET("/test", func(c *gin.Context) {
			c.Set(auth.ContextTenantKey, &domain.Tenant{
				ID:         tenantID,
				PropertyID: propID,
				Status:     domain.TenantStatusActive,
			})
			h.RequireKYCIfPolicyEnforced()(c)
			if !c.IsAborted() {
				c.Status(http.StatusOK)
			}
		})

		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", w.Code)
		}
	})
}
