package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockUnappliedStore struct {
	receipts map[uuid.UUID]*domain.UnmatchedGatewayReceipt
}

func newMockUnappliedStore() *mockUnappliedStore {
	return &mockUnappliedStore{
		receipts: make(map[uuid.UUID]*domain.UnmatchedGatewayReceipt),
	}
}

func (m *mockUnappliedStore) GetUnmatchedReceiptByID(ctx context.Context, id uuid.UUID) (*domain.UnmatchedGatewayReceipt, error) {
	r, ok := m.receipts[id]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (m *mockUnappliedStore) GetUnmatchedReceiptByPaymentID(ctx context.Context, paymentID uuid.UUID) (*domain.UnmatchedGatewayReceipt, error) {
	for _, r := range m.receipts {
		if r.PaymentID != nil && *r.PaymentID == paymentID {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *mockUnappliedStore) GetUnmatchedReceiptByProviderPaymentID(ctx context.Context, provider, providerPaymentID string) (*domain.UnmatchedGatewayReceipt, error) {
	for _, r := range m.receipts {
		if r.Provider == provider && r.ProviderPaymentID == providerPaymentID {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *mockUnappliedStore) ListUnmatchedReceipts(ctx context.Context, propertyID uuid.UUID, status string) ([]domain.UnmatchedGatewayReceipt, error) {
	var res []domain.UnmatchedGatewayReceipt
	for _, r := range m.receipts {
		if r.PropertyID != nil && *r.PropertyID == propertyID {
			if status != "" && r.Status != status {
				continue
			}
			res = append(res, *r)
		}
	}
	return res, nil
}

func (m *mockUnappliedStore) ListUnmatchedReceiptsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.UnmatchedGatewayReceipt, error) {
	var res []domain.UnmatchedGatewayReceipt
	for _, r := range m.receipts {
		if r.TenantID != nil && *r.TenantID == tenantID {
			res = append(res, *r)
		}
	}
	return res, nil
}

func (m *mockUnappliedStore) ResolveUnmatchedReceiptAsCredit(ctx context.Context, receiptID, resolvedBy uuid.UUID, idempotencyKey, notes string) (*domain.Payment, error) {
	r, ok := m.receipts[receiptID]
	if !ok {
		return nil, errors.New("receipt not found")
	}
	if r.Status == domain.ReceiptStatusResolved {
		return nil, fmt.Errorf("receipt %s is already resolved", receiptID)
	}
	now := time.Now()
	action := domain.ReceiptActionCredit
	r.Status = domain.ReceiptStatusResolved
	r.ResolvedAt = &now
	r.ResolvedBy = &resolvedBy
	r.ResolutionAction = &action
	r.ResolutionNotes = &notes
	r.ResolutionIdempotencyKey = &idempotencyKey

	payID := uuid.New()
	r.PaymentID = &payID

	return &domain.Payment{
		ID:         payID,
		PropertyID: r.PropertyID,
		TenantID:   *r.TenantID,
		Amount:     r.AmountPaise,
		MatchedBy:  domain.MatchedByCashfree,
		Provider:   r.Provider,
		CreatedAt:  now,
	}, nil
}

func (m *mockUnappliedStore) MarkUnmatchedReceiptResolving(ctx context.Context, receiptID uuid.UUID, idempotencyKey, notes string) error {
	r, ok := m.receipts[receiptID]
	if !ok {
		return errors.New("receipt not found")
	}
	if r.Status == domain.ReceiptStatusResolved {
		return fmt.Errorf("receipt %s is already resolved", receiptID)
	}
	r.Status = domain.ReceiptStatusResolving
	r.ResolutionIdempotencyKey = &idempotencyKey
	r.ResolutionNotes = &notes
	return nil
}

func (m *mockUnappliedStore) LinkUnmatchedReceiptRefund(ctx context.Context, receiptID, refundID uuid.UUID) error {
	r, ok := m.receipts[receiptID]
	if !ok {
		return errors.New("receipt not found")
	}
	action := domain.ReceiptActionRefund
	now := time.Now()
	r.ResolutionRefundID = &refundID
	r.ResolutionAction = &action
	r.Status = domain.ReceiptStatusResolved
	r.ResolvedAt = &now
	return nil
}

func (m *mockUnappliedStore) LinkUnmatchedReceiptPayment(ctx context.Context, receiptID, paymentID uuid.UUID) error {
	r, ok := m.receipts[receiptID]
	if !ok {
		return errors.New("receipt not found")
	}
	r.PaymentID = &paymentID
	return nil
}

func (m *mockUnappliedStore) MarkUnmatchedReceiptHeldNotified(ctx context.Context, receiptID uuid.UUID) error {
	r, ok := m.receipts[receiptID]
	if !ok {
		return errors.New("receipt not found")
	}
	now := time.Now()
	r.HeldNotifiedAt = &now
	return nil
}

func (m *mockUnappliedStore) MarkUnmatchedReceiptResolutionNotified(ctx context.Context, receiptID uuid.UUID) error {
	r, ok := m.receipts[receiptID]
	if !ok {
		return errors.New("receipt not found")
	}
	now := time.Now()
	r.ResolutionNotifiedAt = &now
	return nil
}

type mockUnappliedUserStore struct {
	users []domain.User
}

func (m *mockUnappliedUserStore) Create(ctx context.Context, user *domain.User) error {
	return nil
}
func (m *mockUnappliedUserStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	for _, u := range m.users {
		if u.ID == id {
			return &u, nil
		}
	}
	return nil, errors.New("user not found")
}
func (m *mockUnappliedUserStore) GetByPhone(ctx context.Context, phone string) (*domain.User, error) {
	return nil, nil
}
func (m *mockUnappliedUserStore) GetByPropertyAndRole(ctx context.Context, propertyID uuid.UUID, role domain.Role) ([]domain.User, error) {
	return m.users, nil
}
func (m *mockUnappliedUserStore) UpdatePhone(ctx context.Context, id uuid.UUID, phone string) error {
	return nil
}
func (m *mockUnappliedUserStore) LinkFirebaseUID(ctx context.Context, id uuid.UUID, firebaseUID string) error {
	return nil
}
func (m *mockUnappliedUserStore) SetPropertyID(ctx context.Context, id uuid.UUID, propertyID uuid.UUID) error {
	return nil
}

type mockUnappliedAuth struct {
	AuthService
}

func (m *mockUnappliedAuth) VerifyStepUpOTPSpecific(ctx context.Context, phone, otp, purpose string, batchID *uuid.UUID) error {
	if otp == "123456" {
		return nil
	}
	return auth.ErrInvalidOTP
}

func setupUnappliedRouter(h *Handlers, propID, userID uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.Recovery())

	owner := r.Group("/api/owner")
	owner.Use(func(c *gin.Context) {
		claims := &auth.Claims{
			UserID:     userID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		}
		c.Set("claims", claims)
		c.Next()
	})

	owner.GET("/payments/unapplied-receipts", h.OwnerListUnappliedReceipts)
	owner.GET("/payments/unapplied-receipts/:id", h.OwnerGetUnappliedReceipt)
	owner.POST("/payments/unapplied-receipts/:id/credit", h.OwnerApplyUnappliedReceiptCredit)
	owner.POST("/payments/unapplied-receipts/:id/refund", h.OwnerRefundUnappliedReceipt)

	return r
}

func TestOwnerUnappliedReceipts_Workflow(t *testing.T) {
	propID := uuid.New()
	otherPropID := uuid.New()
	userID := uuid.New()
	tenantID := uuid.New()

	receiptStore := newMockUnappliedStore()
	userStore := &mockUnappliedUserStore{
		users: []domain.User{
			{ID: userID, PropertyID: &propID, Role: domain.RoleOwner, Phone: "+919876543210"},
		},
	}
	authSvc := &mockUnappliedAuth{}

	h := &Handlers{Deps: Deps{
		UnmatchedReceipts:  receiptStore,
		GatewayPaymentRepo: newMockRefundPaymentRepo(),
		TenantStore:        &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propID}},
		UserStore:          userStore,
		Auth:               authSvc,
	}}

	receiptID := uuid.New()
	payIntentID := uuid.New()
	heldReceipt := &domain.UnmatchedGatewayReceipt{
		ID:                receiptID,
		PropertyID:        &propID,
		TenantID:          &tenantID,
		PaymentIntentID:   &payIntentID,
		Provider:          "cashfree",
		ProviderPaymentID: "cf_pay_test_123",
		AmountPaise:       500000,
		FailureReason:     "amount_mismatch",
		Status:            domain.ReceiptStatusHeld,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	receiptStore.receipts[receiptID] = heldReceipt

	foreignReceiptID := uuid.New()
	receiptStore.receipts[foreignReceiptID] = &domain.UnmatchedGatewayReceipt{
		ID:                foreignReceiptID,
		PropertyID:        &otherPropID,
		TenantID:          &tenantID,
		Provider:          "cashfree",
		ProviderPaymentID: "cf_pay_foreign_999",
		AmountPaise:       300000,
		FailureReason:     "unknown_order",
		Status:            domain.ReceiptStatusHeld,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}

	router := setupUnappliedRouter(h, propID, userID)

	t.Run("List unapplied receipts scoped to caller property", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/owner/payments/unapplied-receipts", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Receipts []domain.UnmatchedGatewayReceipt `json:"unapplied_receipts"`
			Count    int                              `json:"count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}

		if resp.Count != 1 || len(resp.Receipts) != 1 {
			t.Fatalf("expected 1 receipt for property, got %d", len(resp.Receipts))
		}
		if resp.Receipts[0].ID != receiptID {
			t.Fatalf("expected receipt ID %s, got %s", receiptID, resp.Receipts[0].ID)
		}
	})

	t.Run("Get unapplied receipt by ID - IDOR guard on foreign property", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/owner/payments/unapplied-receipts/"+foreignReceiptID.String(), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for foreign property receipt, got %d", rec.Code)
		}
	})

	t.Run("Get unapplied receipt by ID - success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/owner/payments/unapplied-receipts/"+receiptID.String(), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rec.Code)
		}

		var resp struct {
			Receipt domain.UnmatchedGatewayReceipt `json:"unapplied_receipt"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if resp.Receipt.ID != receiptID {
			t.Fatalf("expected receipt ID %s, got %s", receiptID, resp.Receipt.ID)
		}
	})

	t.Run("Credit resolution - requires Idempotency-Key", func(t *testing.T) {
		body := map[string]any{
			"notes": "converting mismatch to advance credit",
			"step_up_auth": map[string]string{
				"otp": "123456",
			},
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/unapplied-receipts/"+receiptID.String()+"/credit", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for missing Idempotency-Key, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("Credit resolution - requires step-up auth", func(t *testing.T) {
		body := map[string]any{
			"notes": "converting mismatch to advance credit",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/unapplied-receipts/"+receiptID.String()+"/credit", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem_test_credit_001")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized for missing step-up auth, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("Credit resolution - success with step-up auth", func(t *testing.T) {
		body := map[string]any{
			"notes": "converting mismatch to advance credit",
			"step_up_auth": map[string]string{
				"otp": "123456",
			},
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/unapplied-receipts/"+receiptID.String()+"/credit", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem_test_credit_001")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if resp["status"] != "resolved" {
			t.Fatalf("expected status resolved, got %v", resp["status"])
		}

		// Receipt in store is now resolved
		updated, _ := receiptStore.GetUnmatchedReceiptByID(context.Background(), receiptID)
		if updated.Status != domain.ReceiptStatusResolved {
			t.Fatalf("expected store status resolved, got %s", updated.Status)
		}
	})

	t.Run("Credit resolution - idempotent replay with same key returns 200", func(t *testing.T) {
		body := map[string]any{
			"notes": "replay call",
			"step_up_auth": map[string]string{
				"otp": "123456",
			},
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/unapplied-receipts/"+receiptID.String()+"/credit", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem_test_credit_001")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on idempotent replay, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("Credit resolution - already resolved with different key returns 400", func(t *testing.T) {
		body := map[string]any{
			"notes": "duplicate resolve attempt",
			"step_up_auth": map[string]string{
				"otp": "123456",
			},
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/unapplied-receipts/"+receiptID.String()+"/credit", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem_test_credit_002")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on already resolved receipt with different key, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

type mockTenantPaymentStore struct {
	payments []domain.Payment
}

func (m *mockTenantPaymentStore) ListByProperty(ctx context.Context, propertyID uuid.UUID, matchedBy *domain.MatchedBy) ([]domain.Payment, error) {
	return m.payments, nil
}

func (m *mockTenantPaymentStore) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Payment, error) {
	return m.payments, nil
}

func (m *mockTenantPaymentStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	for _, p := range m.payments {
		if p.ID == id {
			return &p, nil
		}
	}
	return nil, nil
}

func TestTenantPayments_HeldUnappliedStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	tenantID := uuid.New()

	receiptStore := newMockUnappliedStore()
	payStore := &mockTenantPaymentStore{}

	heldReceiptID := uuid.New()
	receiptStore.receipts[heldReceiptID] = &domain.UnmatchedGatewayReceipt{
		ID:                heldReceiptID,
		PropertyID:        &propID,
		TenantID:          &tenantID,
		Provider:          "cashfree",
		ProviderPaymentID: "cf_pay_mismatch_888",
		AmountPaise:       600000,
		FailureReason:     "amount_mismatch",
		Status:            domain.ReceiptStatusHeld,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}

	h := &Handlers{Deps: Deps{
		PaymentStore:      payStore,
		UnmatchedReceipts: receiptStore,
		TenantStore:       &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propID}},
	}}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		claims := &auth.Claims{
			UserID:     tenantID,
			PropertyID: &propID,
			Role:       domain.RoleTenant,
		}
		c.Set("claims", claims)
		c.Set("tenant", &domain.Tenant{ID: tenantID, PropertyID: propID})
		c.Next()
	})
	r.GET("/api/tenant/payments", h.TenantPayments)

	req := httptest.NewRequest(http.MethodGet, "/api/tenant/payments", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Payments []struct {
			ID            uuid.UUID `json:"id"`
			Amount        int64     `json:"amount"`
			IsUnapplied   bool      `json:"is_unapplied"`
			UIStatus      string    `json:"ui_status"`
			StatusMessage string    `json:"status_message"`
		} `json:"payments"`
		HeldReceipts []domain.UnmatchedGatewayReceipt `json:"held_receipts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(resp.Payments) != 1 {
		t.Fatalf("expected 1 unapplied payment in list, got %d", len(resp.Payments))
	}
	if resp.Payments[0].UIStatus != "HELD_UNAPPLIED" {
		t.Errorf("expected ui_status HELD_UNAPPLIED, got %q", resp.Payments[0].UIStatus)
	}
	if resp.Payments[0].StatusMessage != "Payment received. Owner is resolving." {
		t.Errorf("expected status_message %q, got %q", "Payment received. Owner is resolving.", resp.Payments[0].StatusMessage)
	}
	if !resp.Payments[0].IsUnapplied {
		t.Errorf("expected is_unapplied true, got false")
	}
}
