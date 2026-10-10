package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type isolationTenantRepo struct {
	tenants map[uuid.UUID]*domain.Tenant
}

func (r *isolationTenantRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	t, ok := r.tenants[id]
	if !ok {
		return nil, errors.New("tenant not found")
	}
	cp := *t
	return &cp, nil
}

func (r *isolationTenantRepo) GetByPhone(_ context.Context, _ string) (*domain.Tenant, error) {
	return nil, errors.New("not implemented")
}

func (r *isolationTenantRepo) ListByProperty(_ context.Context, propID uuid.UUID) ([]domain.Tenant, error) {
	var list []domain.Tenant
	for _, t := range r.tenants {
		if t.PropertyID == propID {
			list = append(list, *t)
		}
	}
	return list, nil
}

func (r *isolationTenantRepo) Update(_ context.Context, t *domain.Tenant) error {
	r.tenants[t.ID] = t
	return nil
}

func (r *isolationTenantRepo) GetIDPhoto(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return nil, nil
}

type isolationUserRepo struct {
	users map[uuid.UUID]*domain.User
}

func (r *isolationUserRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	u, ok := r.users[id]
	if !ok {
		return nil, errors.New("user not found")
	}
	cp := *u
	return &cp, nil
}

func (r *isolationUserRepo) IncrementTokenVersion(_ context.Context, id uuid.UUID) error {
	u, ok := r.users[id]
	if !ok {
		return errors.New("user not found")
	}
	u.TokenVersion++
	return nil
}

type isolationDueStore struct {
	dues map[uuid.UUID]*domain.Due
}

func (s *isolationDueStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	d, ok := s.dues[id]
	if !ok {
		return nil, errors.New("due not found")
	}
	cp := *d
	return &cp, nil
}

func (s *isolationDueStore) List(_ context.Context, _ postgres.DueListFilter) ([]domain.Due, error) {
	var list []domain.Due
	for _, d := range s.dues {
		list = append(list, *d)
	}
	return list, nil
}

func (s *isolationDueStore) ListByTenant(_ context.Context, tenantID uuid.UUID) ([]domain.Due, error) {
	var list []domain.Due
	for _, d := range s.dues {
		if d.TenantID == tenantID {
			list = append(list, *d)
		}
	}
	return list, nil
}

type isolationPaymentStore struct {
	payments []domain.Payment
}

func (s *isolationPaymentStore) ListByProperty(_ context.Context, _ uuid.UUID, _ *domain.MatchedBy) ([]domain.Payment, error) {
	return s.payments, nil
}

func (s *isolationPaymentStore) ListByTenant(_ context.Context, tenantID uuid.UUID) ([]domain.Payment, error) {
	var list []domain.Payment
	for _, p := range s.payments {
		if p.TenantID == tenantID {
			list = append(list, p)
		}
	}
	return list, nil
}

type isolationPropertyStore struct {
	props map[uuid.UUID]*domain.Property
}

func (s *isolationPropertyStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Property, error) {
	p, ok := s.props[id]
	if !ok {
		return nil, errors.New("property not found")
	}
	return p, nil
}

func (s *isolationPropertyStore) List(_ context.Context) ([]domain.Property, error) {
	return nil, nil
}

func (s *isolationPropertyStore) GetByOwnerPhone(_ context.Context, _ string) (*domain.Property, error) {
	return nil, nil
}

func (s *isolationPropertyStore) GetByInviteCode(_ context.Context, _ string) (*domain.Property, error) {
	return nil, nil
}

func (s *isolationPropertyStore) Create(_ context.Context, _ *domain.Property) error {
	return nil
}

// TestSamePropertyTenantIsolation proves that two tenants under the same property
// cannot access or mutate each other's financial records (dues, QR codes, checkout, reports, payments).
func TestSamePropertyTenantIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"

	propID := uuid.New()
	prop := &domain.Property{
		ID:        propID,
		Name:      "Greenwood PG",
		UPIVPA:    "owner@upi",
		OwnerName: "Owner Name",
	}

	tenantID_A := uuid.New()
	tenantID_B := uuid.New()

	userA := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleTenant,
		PropertyID:   &propID,
		TenantID:     &tenantID_A,
		TokenVersion: 1,
	}
	userB := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleTenant,
		PropertyID:   &propID,
		TenantID:     &tenantID_B,
		TokenVersion: 1,
	}

	tenantA := &domain.Tenant{
		ID:         tenantID_A,
		PropertyID: propID,
		Name:       "Tenant Alpha",
		Status:     domain.TenantStatusActive,
	}
	tenantB := &domain.Tenant{
		ID:         tenantID_B,
		PropertyID: propID,
		Name:       "Tenant Beta",
		Status:     domain.TenantStatusActive,
	}

	tokenA, err := auth.IssueToken(jwtSecret, userA)
	if err != nil {
		t.Fatalf("issue token A: %v", err)
	}
	tokenB, err := auth.IssueToken(jwtSecret, userB)
	if err != nil {
		t.Fatalf("issue token B: %v", err)
	}

	now := time.Now().UTC()
	dueA := &domain.Due{
		ID:             uuid.New(),
		PropertyID:     propID,
		TenantID:       tenantA.ID,
		Kind:           domain.DueKindRent,
		DueCode:        "RENT-A1",
		Amount:         1200000,
		OriginalAmount: 1200000,
		Status:         domain.DueStatusPending,
		PeriodStart:    now,
		PeriodEnd:      now.AddDate(0, 1, 0),
		DueDate:        now.AddDate(0, 0, 5),
	}
	dueB := &domain.Due{
		ID:             uuid.New(),
		PropertyID:     propID,
		TenantID:       tenantB.ID,
		Kind:           domain.DueKindRent,
		DueCode:        "RENT-B1",
		Amount:         1500000,
		OriginalAmount: 1500000,
		Status:         domain.DueStatusPending,
		PeriodStart:    now,
		PeriodEnd:      now.AddDate(0, 1, 0),
		DueDate:        now.AddDate(0, 0, 5),
	}

	paymentA := domain.Payment{
		ID:         uuid.New(),
		PropertyID: &propID,
		TenantID:   tenantA.ID,
		DueID:      dueA.ID,
		Amount:     1200000,
	}
	paymentB := domain.Payment{
		ID:         uuid.New(),
		PropertyID: &propID,
		TenantID:   tenantB.ID,
		DueID:      dueB.ID,
		Amount:     1500000,
	}

	tenantStore := &isolationTenantRepo{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantA.ID: tenantA,
			tenantB.ID: tenantB,
		},
	}
	userStore := &isolationUserRepo{
		users: map[uuid.UUID]*domain.User{
			userA.ID: userA,
			userB.ID: userB,
		},
	}
	dueStore := &isolationDueStore{
		dues: map[uuid.UUID]*domain.Due{
			dueA.ID: dueA,
			dueB.ID: dueB,
		},
	}
	paymentStore := &isolationPaymentStore{
		payments: []domain.Payment{paymentA, paymentB},
	}
	propertyStore := &isolationPropertyStore{
		props: map[uuid.UUID]*domain.Property{
			propID: prop,
		},
	}

	router := NewRouter(Deps{
		JWTSecret:      jwtSecret,
		PropertyStore:  propertyStore,
		TenantStore:    tenantStore,
		DueStore:       dueStore,
		PaymentStore:   paymentStore,
		AuthTenantRepo: tenantStore,
		AuthUserRepo:   userStore,
		Collector:      collector.New(nil, nil),
	})

	// 1. Tenant A calls GET /api/tenant/dues -> must only list Due A, never Due B.
	t.Run("GET /api/tenant/dues isolates records", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/tenant/dues", nil)
		req.Header.Set("Authorization", "Bearer "+tokenA)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			Dues []domain.Due `json:"dues"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("json unmarshal: %v", err)
		}
		if len(resp.Dues) != 1 {
			t.Fatalf("expected exactly 1 due for tenant A, got %d", len(resp.Dues))
		}
		if resp.Dues[0].ID != dueA.ID {
			t.Fatalf("expected due %s, got %s", dueA.ID, resp.Dues[0].ID)
		}
	})

	// 2. Tenant A calls GET /api/tenant/dues/:id/qr for Tenant B's due -> 404
	t.Run("GET /api/tenant/dues/:id/qr rejects cross-tenant due", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/tenant/dues/%s/qr", dueB.ID), nil)
		req.Header.Set("Authorization", "Bearer "+tokenA)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for tenant A reading tenant B's due QR, got %d body=%s", w.Code, w.Body.String())
		}
	})

	// 3. Tenant A calls GET /api/tenant/dues/:id/pay for Tenant B's due -> 404
	t.Run("GET /api/tenant/dues/:id/pay rejects cross-tenant due", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/tenant/dues/%s/pay", dueB.ID), nil)
		req.Header.Set("Authorization", "Bearer "+tokenA)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for tenant A paying tenant B's due, got %d body=%s", w.Code, w.Body.String())
		}
	})

	// 4. Tenant A calls POST /api/tenant/dues/:id/reports for Tenant B's due -> 404
	t.Run("POST /api/tenant/dues/:id/reports rejects cross-tenant report", func(t *testing.T) {
		body := strings.NewReader(`{"amount":1500000,"note":"attempted cross-tenant report"}`)
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tenant/dues/%s/reports", dueB.ID), body)
		req.Header.Set("Authorization", "Bearer "+tokenA)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for tenant A submitting report for tenant B's due, got %d body=%s", w.Code, w.Body.String())
		}
	})

	// 5. Tenant A calls GET /api/tenant/payments -> must only list Payment A, never Payment B.
	t.Run("GET /api/tenant/payments isolates payments", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/tenant/payments", nil)
		req.Header.Set("Authorization", "Bearer "+tokenA)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			Payments []domain.Payment `json:"payments"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("json unmarshal: %v", err)
		}
		if len(resp.Payments) != 1 {
			t.Fatalf("expected exactly 1 payment for tenant A, got %d", len(resp.Payments))
		}
		if resp.Payments[0].ID != paymentA.ID {
			t.Fatalf("expected payment %s, got %s", paymentA.ID, resp.Payments[0].ID)
		}
	})

	// 6. Invert check: Tenant B calls GET /api/tenant/dues/:id/qr for Tenant A's due -> 404
	t.Run("Invert: GET /api/tenant/dues/:id/qr rejects tenant B reading tenant A's due", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/tenant/dues/%s/qr", dueA.ID), nil)
		req.Header.Set("Authorization", "Bearer "+tokenB)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for tenant B reading tenant A's due QR, got %d body=%s", w.Code, w.Body.String())
		}
	})

	// 7. Positive control: Tenant A accesses own due pay intent -> 200 OK
	t.Run("Positive control: GET /api/tenant/dues/:id/pay accepts own due", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/tenant/dues/%s/pay", dueA.ID), nil)
		req.Header.Set("Authorization", "Bearer "+tokenA)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for tenant A reading own due pay, got %d body=%s", w.Code, w.Body.String())
		}
	})
}
