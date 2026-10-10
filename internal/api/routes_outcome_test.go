package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/gamification"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type outcomeAuthStub struct {
	AuthService
}

func (outcomeAuthStub) RequestOTP(_ context.Context, _ string) error {
	return nil
}

type outcomeJoinStub struct {
	JoinService
}

func (outcomeJoinStub) RotateInvite(_ context.Context, _ uuid.UUID) (string, error) {
	return "INV-ROTATED-99", nil
}

type outcomeEventStore struct{}

func (outcomeEventStore) List(_ context.Context, _ postgres.EventFilter) ([]domain.Event, error) {
	return []domain.Event{}, nil
}

type outcomePaymentStore struct {
	PaymentService
}

func (outcomePaymentStore) BuildSummary(_ context.Context, _ uuid.UUID, _ string) (*payment.ReconciliationSummary, error) {
	return &payment.ReconciliationSummary{
		RentCollected: 5000000,
	}, nil
}

type outcomeGamificationStore struct {
	gamification.Store
}

func (outcomeGamificationStore) GetSettings(_ context.Context, propID uuid.UUID) (*domain.PropertyGamificationSettings, error) {
	return &domain.PropertyGamificationSettings{PropertyID: propID}, nil
}

func (outcomeGamificationStore) ListRooms(_ context.Context, _ uuid.UUID) ([]domain.Room, error) {
	return []domain.Room{}, nil
}

func (outcomeGamificationStore) ListMealRSVPsByDate(_ context.Context, _ uuid.UUID, _ time.Time) ([]domain.MealRSVP, error) {
	return []domain.MealRSVP{}, nil
}

func (outcomeGamificationStore) GetTenantMealRSVP(_ context.Context, _ uuid.UUID, _ time.Time, _ string) (*domain.MealRSVP, error) {
	return nil, nil
}

func (outcomeGamificationStore) CreateReferral(_ context.Context, ref *domain.Referral) error {
	ref.ID = uuid.New()
	return nil
}

func (outcomeGamificationStore) ListReferralsByTenant(_ context.Context, _ uuid.UUID) ([]domain.Referral, error) {
	return []domain.Referral{}, nil
}

// TestUnreferencedRoutesOutcome verifies HTTP request outcomes for previously unreferenced routes.
func TestUnreferencedRoutesOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"
	propID := uuid.New()

	ownerUser := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	managerUser := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleManager,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	tenantID := uuid.New()
	tenantUser := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleTenant,
		PropertyID:   &propID,
		TenantID:     &tenantID,
		TokenVersion: 1,
	}
	tenantObj := &domain.Tenant{
		ID:         tenantID,
		PropertyID: propID,
		Status:     domain.TenantStatusActive,
	}

	ownerToken, _ := auth.IssueToken(jwtSecret, ownerUser)
	managerToken, _ := auth.IssueToken(jwtSecret, managerUser)
	tenantToken, _ := auth.IssueToken(jwtSecret, tenantUser)

	finStore := finance.NewMemoryStore()
	finSvc := finance.NewService(finStore, nil)

	users := map[uuid.UUID]*domain.User{
		ownerUser.ID:   ownerUser,
		managerUser.ID: managerUser,
		tenantUser.ID:  tenantUser,
	}
	uStore := &isolationUserRepo{users: users}
	tStore := &isolationTenantRepo{tenants: map[uuid.UUID]*domain.Tenant{tenantID: tenantObj}}
	pStore := &isolationPropertyStore{props: map[uuid.UUID]*domain.Property{
		propID: {ID: propID, Name: "Test Property"},
	}}

	router := NewRouter(Deps{
		JWTSecret:         jwtSecret,
		Auth:              outcomeAuthStub{},
		Joins:             outcomeJoinStub{},
		Finance:           finSvc,
		FinanceEnabled:    true,
		Payments:          outcomePaymentStore{},
		EventStore:        outcomeEventStore{},
		GamificationStore: outcomeGamificationStore{},
		Gamification:      gamification.NewService(outcomeGamificationStore{}, nil, nil, nil, nil),
		PropertyStore:     pStore,
		TenantStore:       tStore,
		DueStore:          &isolationDueStore{dues: map[uuid.UUID]*domain.Due{}},
		PaymentStore:      &isolationPaymentStore{},
		AuthTenantRepo:    tStore,
		AuthUserRepo:      uStore,
	})

	doReq := func(method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
		var bodyReader *strings.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			bodyReader = strings.NewReader(string(b))
		} else {
			bodyReader = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, bodyReader)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 1. POST /api/auth/otp/request
	t.Run("POST /api/auth/otp/request with valid phone returns 200", func(t *testing.T) {
		w := doReq(http.MethodPost, "/api/auth/otp/request", "", map[string]string{"phone": "+919876543210"}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("POST /api/auth/otp/request with missing phone returns 400", func(t *testing.T) {
		w := doReq(http.MethodPost, "/api/auth/otp/request", "", map[string]string{}, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
		}
	})

	// 2. POST /api/auth/refresh without cookie returns 401
	t.Run("POST /api/auth/refresh missing cookie returns 401", func(t *testing.T) {
		w := doReq(http.MethodPost, "/api/auth/refresh", "", nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
		}
	})

	// 3. POST /api/owner/invite/rotate
	t.Run("POST /api/owner/invite/rotate rotates code", func(t *testing.T) {
		w := doReq(http.MethodPost, "/api/owner/invite/rotate", ownerToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "INV-ROTATED-99") {
			t.Fatalf("expected rotated invite code, got %s", w.Body.String())
		}
	})

	// 4. Finance endpoints
	t.Run("GET /api/owner/finance/summary returns 200", func(t *testing.T) {
		w := doReq(http.MethodGet, "/api/owner/finance/summary", ownerToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("GET /api/owner/finance/ledger returns 200", func(t *testing.T) {
		w := doReq(http.MethodGet, "/api/owner/finance/ledger", ownerToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("GET /api/owner/finance/advances returns 200", func(t *testing.T) {
		w := doReq(http.MethodGet, "/api/owner/finance/advances", ownerToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("GET /api/owner/finance/imports returns 200", func(t *testing.T) {
		w := doReq(http.MethodGet, "/api/owner/finance/imports", ownerToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("GET /api/owner/finance/variance-bridge returns 200", func(t *testing.T) {
		w := doReq(http.MethodGet, "/api/owner/finance/variance-bridge", ownerToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("POST /api/owner/finance/reimbursements requires Idempotency-Key", func(t *testing.T) {
		w := doReq(http.MethodPost, "/api/owner/finance/reimbursements", ownerToken, map[string]any{
			"manager_user_id": managerUser.ID,
			"amount_paise":    50000,
		}, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing idempotency key, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("GET /api/owner/finance/policies returns 200", func(t *testing.T) {
		w := doReq(http.MethodGet, "/api/owner/finance/policies", ownerToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("GET /api/manager/finance/today returns 200", func(t *testing.T) {
		w := doReq(http.MethodGet, "/api/manager/finance/today", managerToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("POST /api/manager/finance/meal-prep with valid body returns 200", func(t *testing.T) {
		w := doReq(http.MethodPost, "/api/manager/finance/meal-prep", managerToken, map[string]any{
			"meal_date":      "2026-10-10T00:00:00Z",
			"meal_slot":      "lunch",
			"prepared_count": 25,
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	// 5. Tenant routes
	t.Run("POST /api/tenant/hazards requires description and category", func(t *testing.T) {
		w := doReq(http.MethodPost, "/api/tenant/hazards", tenantToken, map[string]any{}, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing hazard fields, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("POST /api/tenant/referrals requires phone", func(t *testing.T) {
		w := doReq(http.MethodPost, "/api/tenant/referrals", tenantToken, map[string]any{"name": "Friend"}, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing phone, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("GET /api/tenant/meal-rsvp returns 200", func(t *testing.T) {
		w := doReq(http.MethodGet, "/api/tenant/meal-rsvp", tenantToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("POST /api/tenant/meal-rsvp validates date format", func(t *testing.T) {
		w := doReq(http.MethodPost, "/api/tenant/meal-rsvp", tenantToken, map[string]any{
			"date":      "invalid-date",
			"slot":      "dinner",
			"attending": true,
		}, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid date, got %d body=%s", w.Code, w.Body.String())
		}
	})

	// 6. Owner reconciliation & events
	t.Run("GET /api/owner/reconciliation returns 200", func(t *testing.T) {
		w := doReq(http.MethodGet, "/api/owner/reconciliation", ownerToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("GET /api/owner/events returns 200", func(t *testing.T) {
		w := doReq(http.MethodGet, "/api/owner/events", ownerToken, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})
}
