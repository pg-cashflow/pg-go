package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// In-memory test doubles for owner sensitive route testing

type fakeOwnerTenantStore struct {
	tenants  map[uuid.UUID]*domain.Tenant
	idPhotos map[uuid.UUID][]byte
}

func (s *fakeOwnerTenantStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	t, ok := s.tenants[id]
	if !ok {
		return nil, fmt.Errorf("tenant not found")
	}
	cp := *t
	return &cp, nil
}
func (s *fakeOwnerTenantStore) ListByProperty(_ context.Context, pid uuid.UUID) ([]domain.Tenant, error) {
	var list []domain.Tenant
	for _, t := range s.tenants {
		if t.PropertyID == pid {
			list = append(list, *t)
		}
	}
	return list, nil
}
func (s *fakeOwnerTenantStore) Update(_ context.Context, t *domain.Tenant) error {
	s.tenants[t.ID] = t
	return nil
}
func (s *fakeOwnerTenantStore) GetIDPhoto(_ context.Context, id uuid.UUID) ([]byte, error) {
	b, ok := s.idPhotos[id]
	if !ok || len(b) == 0 {
		return nil, nil
	}
	return b, nil
}

type fakeOwnerKYCService struct {
	api.KYCService
	photos map[uuid.UUID][]byte
}

func (k *fakeOwnerKYCService) GetAttestedPhoto(_ context.Context, tenantID uuid.UUID) ([]byte, error) {
	b, ok := k.photos[tenantID]
	if !ok || len(b) == 0 {
		return nil, fmt.Errorf("no kyc photo")
	}
	return b, nil
}

type fakeOwnerDueStore struct {
	dues map[uuid.UUID]*domain.Due
}

func (s *fakeOwnerDueStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	d, ok := s.dues[id]
	if !ok {
		return nil, fmt.Errorf("due not found")
	}
	cp := *d
	return &cp, nil
}
func (s *fakeOwnerDueStore) List(_ context.Context, _ postgres.DueListFilter) ([]domain.Due, error) {
	var list []domain.Due
	for _, d := range s.dues {
		list = append(list, *d)
	}
	return list, nil
}
func (s *fakeOwnerDueStore) ListByTenant(_ context.Context, tid uuid.UUID) ([]domain.Due, error) {
	var list []domain.Due
	for _, d := range s.dues {
		if d.TenantID == tid {
			list = append(list, *d)
		}
	}
	return list, nil
}

type fakeOwnerBillingService struct {
	waivedDues map[uuid.UUID]bool
	failWaive  error
}

func (b *fakeOwnerBillingService) WaiveDue(_ context.Context, dueID uuid.UUID) (*domain.Due, error) {
	if b.failWaive != nil {
		return nil, b.failWaive
	}
	b.waivedDues[dueID] = true
	return &domain.Due{ID: dueID, Status: domain.DueStatusWaived}, nil
}
func (b *fakeOwnerBillingService) Prorate(_ context.Context, _ uuid.UUID, _ time.Time) (*domain.Due, error) {
	return nil, nil
}

type fakeOwnerMagicLinkService struct {
	createdTokens map[uuid.UUID]string
}

func (m *fakeOwnerMagicLinkService) CreatePaymentToken(_ context.Context, dueID uuid.UUID) (string, error) {
	token := "/pay/test-token-" + dueID.String()[:8]
	m.createdTokens[dueID] = token
	return token, nil
}
func (m *fakeOwnerMagicLinkService) ResolveToken(_ context.Context, _ string) (*magiclink.DueView, error) {
	return nil, nil
}

type fakeOwnerUserStore struct {
	users map[uuid.UUID]*domain.User
}

func (s *fakeOwnerUserStore) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	u, ok := s.users[id]
	if !ok {
		return nil, fmt.Errorf("user not found")
	}
	cp := *u
	return &cp, nil
}
func (s *fakeOwnerUserStore) GetByPhone(_ context.Context, _ string) (*domain.User, error) {
	return nil, nil
}
func (s *fakeOwnerUserStore) GetByPropertyAndRole(_ context.Context, pid uuid.UUID, role domain.Role) ([]domain.User, error) {
	var list []domain.User
	for _, u := range s.users {
		if u.PropertyID != nil && *u.PropertyID == pid && u.Role == role {
			list = append(list, *u)
		}
	}
	return list, nil
}
func (s *fakeOwnerUserStore) Create(_ context.Context, _ *domain.User) error { return nil }
func (s *fakeOwnerUserStore) SetPropertyID(_ context.Context, _, _ uuid.UUID) error { return nil }

type fakeOwnerAuthService struct {
	stubPIIAuthService
}

func (s *fakeOwnerAuthService) VerifyStepUpOTPSpecific(_ context.Context, _, _, _ string, _ *uuid.UUID) error {
	return nil
}

// TestOwnerSensitiveRoutes_T4a tests the high-risk money-moving and PII endpoints:
// OwnerTenantKYCPhoto, TenantIDPhoto, WaiveDue, DueToken, PostReimbursement
func TestOwnerSensitiveRoutes_T4a(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "jwt-secret-for-owner-sensitive-routes-32bytes"

	propOwner := uuid.New()
	propForeign := uuid.New()

	ownerPhone := "+919876500001"
	ownerUser := &domain.User{
		ID:         uuid.New(),
		Phone:      ownerPhone,
		PropertyID: &propOwner,
		Role:       domain.RoleOwner,
	}
	tenantUser := &domain.User{
		ID:         uuid.New(),
		PropertyID: &propOwner,
		Role:       domain.RoleTenant,
	}

	ownerToken, _ := auth.IssueToken(jwtSecret, ownerUser)
	tenantToken, _ := auth.IssueToken(jwtSecret, tenantUser)

	userStore := &fakeOwnerUserStore{
		users: map[uuid.UUID]*domain.User{
			ownerUser.ID: ownerUser,
		},
	}
	authSvc := &fakeOwnerAuthService{}

	// Tenants: one belonging to owner's property, one foreign
	ownerTenantID := uuid.New()
	foreignTenantID := uuid.New()
	phone := "+919876543210"

	tenantStore := &fakeOwnerTenantStore{
		tenants: map[uuid.UUID]*domain.Tenant{
			ownerTenantID:   {ID: ownerTenantID, PropertyID: propOwner, Name: "Owner Tenant", Phone: &phone},
			foreignTenantID: {ID: foreignTenantID, PropertyID: propForeign, Name: "Foreign Tenant", Phone: &phone},
		},
		idPhotos: map[uuid.UUID][]byte{
			ownerTenantID:   []byte("valid-owner-tenant-id-photo-bytes"),
			foreignTenantID: []byte("foreign-tenant-id-photo-bytes"),
		},
	}

	kycSvc := &fakeOwnerKYCService{
		photos: map[uuid.UUID][]byte{
			ownerTenantID:   []byte("valid-owner-tenant-kyc-photo-bytes"),
			foreignTenantID: []byte("foreign-tenant-kyc-photo-bytes"),
		},
	}

	dueOwnerID := uuid.New()
	dueForeignID := uuid.New()
	dueStore := &fakeOwnerDueStore{
		dues: map[uuid.UUID]*domain.Due{
			dueOwnerID:   {ID: dueOwnerID, PropertyID: propOwner, TenantID: ownerTenantID, Amount: 100000, Status: domain.DueStatusPending},
			dueForeignID: {ID: dueForeignID, PropertyID: propForeign, TenantID: foreignTenantID, Amount: 200000, Status: domain.DueStatusPending},
		},
	}

	billingSvc := &fakeOwnerBillingService{waivedDues: make(map[uuid.UUID]bool)}
	magicSvc := &fakeOwnerMagicLinkService{createdTokens: make(map[uuid.UUID]string)}
	financeStore := finance.NewMemoryStore()
	financeSvc := finance.NewService(financeStore, nil)

	h := &api.Handlers{
		Deps: api.Deps{
			JWTSecret:        jwtSecret,
			TenantStore:      tenantStore,
			UserStore:        userStore,
			Auth:             authSvc,
			KYCSvc:           kycSvc,
			DueStore:         dueStore,
			Billing:          billingSvc,
			MagicLink:        magicSvc,
			MagicLinkBaseURL: "https://pg.example.com",
			Finance:          financeSvc,
			FinanceEnabled:   true,
		},
	}

	r := gin.New()
	ownerGroup := r.Group("/owner", auth.RequireOwner(jwtSecret, nil))
	ownerGroup.GET("/tenants/:id/kyc/photo", h.OwnerTenantKYCPhoto)
	ownerGroup.GET("/tenants/:id/id-photo", h.TenantIDPhoto)
	ownerGroup.POST("/dues/:id/waive", h.WaiveDue)
	ownerGroup.POST("/dues/:id/token", h.DueToken)
	ownerGroup.POST("/finance/reimbursements", h.PostReimbursement)

	// 1. OwnerTenantKYCPhoto
	t.Run("OwnerTenantKYCPhoto: Happy path 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/owner/tenants/"+ownerTenantID.String()+"/kyc/photo", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), []byte("valid-owner-tenant-kyc-photo-bytes")) {
			t.Fatalf("unexpected kyc photo body: %s", w.Body.String())
		}
	})

	t.Run("OwnerTenantKYCPhoto: Foreign property returns 404 (IDOR guard)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/owner/tenants/"+foreignTenantID.String()+"/kyc/photo", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for foreign property tenant, got %d", w.Code)
		}
	})

	t.Run("OwnerTenantKYCPhoto: Tenant role returns 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/owner/tenants/"+ownerTenantID.String()+"/kyc/photo", nil)
		req.Header.Set("Authorization", "Bearer "+tenantToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for non-owner, got %d", w.Code)
		}
	})

	// 2. TenantIDPhoto
	t.Run("TenantIDPhoto: Happy path 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/owner/tenants/"+ownerTenantID.String()+"/id-photo", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("TenantIDPhoto: Foreign property returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/owner/tenants/"+foreignTenantID.String()+"/id-photo", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for foreign property tenant, got %d", w.Code)
		}
	})

	// 3. WaiveDue
	t.Run("WaiveDue: Foreign property returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/owner/dues/"+dueForeignID.String()+"/waive", bytes.NewBufferString("{}"))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for foreign due, got %d", w.Code)
		}
	})

	t.Run("WaiveDue: No step-up reauth returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/owner/dues/"+dueOwnerID.String()+"/waive", bytes.NewBufferString("{}"))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 without step-up auth, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("WaiveDue: Non-waivable returns 400 with step-up auth", func(t *testing.T) {
		billingSvc.failWaive = billing.ErrDueNotWaivable
		req := httptest.NewRequest(http.MethodPost, "/owner/dues/"+dueOwnerID.String()+"/waive", bytes.NewBufferString(`{"otp":"123456"}`))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for non-waivable due, got %d body=%s", w.Code, w.Body.String())
		}
		billingSvc.failWaive = nil
	})

	// 4. DueToken
	t.Run("DueToken: Happy path 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/owner/dues/"+dueOwnerID.String()+"/token", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		var res map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal DueToken response: %v", err)
		}
		if res["url"] == "" || res["path"] == "" {
			t.Fatalf("expected payment token url and path, got: %+v", res)
		}
	})

	t.Run("DueToken: Foreign property returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/owner/dues/"+dueForeignID.String()+"/token", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for foreign due token mint, got %d", w.Code)
		}
	})

	// 5. PostReimbursement
	t.Run("PostReimbursement: Happy path 201", func(t *testing.T) {
		mgrID := uuid.New()
		ctx := context.Background()
		// Seed an expense paid by manager so advance outstanding exists
		exp, _, err := financeSvc.CreateExpense(ctx, finance.CreateExpenseInput{
			PropertyID:    propOwner,
			CategoryCode:  "vendor",
			AmountPaise:   100000,
			ActorID:       mgrID,
			ActorRole:     string(domain.RoleManager),
			IdempotencyKey: "exp-seed-1",
		})
		if err != nil {
			t.Fatalf("seed expense failed: %v", err)
		}
		_, err = financeSvc.PayExpense(ctx, finance.PayExpenseInput{
			ExpenseID:      exp.ID,
			PropertyID:     propOwner,
			ActorID:        mgrID,
			ActorRole:      domain.PayerManager,
			AmountPaise:    100000,
			Method:         "cash",
			IdempotencyKey: "pay-seed-1",
		})
		if err != nil {
			t.Fatalf("pay expense by manager failed: %v", err)
		}

		body := fmt.Sprintf(`{"manager_user_id":"%s","amount_paise":50000}`, mgrID)
		req := httptest.NewRequest(http.MethodPost, "/owner/finance/reimbursements", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("Idempotency-Key", "idem-reimburse-001")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201 Created, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("PostReimbursement: Missing Idempotency-Key returns 400", func(t *testing.T) {
		mgrID := uuid.New()
		body := fmt.Sprintf(`{"manager_user_id":"%s","amount_paise":50000}`, mgrID)
		req := httptest.NewRequest(http.MethodPost, "/owner/finance/reimbursements", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 when missing Idempotency-Key, got %d", w.Code)
		}
	})
}
