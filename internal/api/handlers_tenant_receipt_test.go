package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubReceiptPaymentStore struct {
	payments map[uuid.UUID]*domain.Payment
	err      error
}

func (s *stubReceiptPaymentStore) ListByProperty(_ context.Context, _ uuid.UUID, _ *domain.MatchedBy) ([]domain.Payment, error) {
	return nil, nil
}

func (s *stubReceiptPaymentStore) ListByTenant(_ context.Context, _ uuid.UUID) ([]domain.Payment, error) {
	return nil, nil
}

func (s *stubReceiptPaymentStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Payment, error) {
	if s.err != nil {
		return nil, s.err
	}
	p, ok := s.payments[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return p, nil
}

func setupTenantReceiptRouter(t *testing.T, store api.PaymentStore, tenant *domain.Tenant) (*gin.Engine, string) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	jwtSecret := "test_secret_tenant_receipt_123456"

	h := &api.Handlers{
		Deps: api.Deps{
			JWTSecret:    jwtSecret,
			PaymentStore: store,
		},
	}

	r.GET("/api/tenant/payments/:id/receipt", func(c *gin.Context) {
		if tenant != nil {
			c.Set(auth.ContextTenantKey, tenant)
		}
		h.TenantPaymentReceipt(c)
	})

	return r, jwtSecret
}

func TestTenantPaymentReceipt(t *testing.T) {
	tenantA := &domain.Tenant{
		ID:         uuid.New(),
		PropertyID: uuid.New(),
		Name:       "Tenant Alpha",
		Status:     domain.TenantStatusActive,
	}
	tenantB := &domain.Tenant{
		ID:         uuid.New(),
		PropertyID: uuid.New(),
		Name:       "Tenant Beta",
		Status:     domain.TenantStatusActive,
	}

	paymentAID := uuid.New()
	dueAID := uuid.New()
	propAID := tenantA.PropertyID
	upiTxn := "UPI1234567890"

	paymentA := &domain.Payment{
		ID:         paymentAID,
		PropertyID: &propAID,
		DueID:      dueAID,
		TenantID:   tenantA.ID,
		UPITxnID:   &upiTxn,
		Amount:     1500000,
		MatchedBy:  domain.MatchedByDueCode,
		MatchedAt:  time.Now().UTC().Truncate(time.Second),
	}

	store := &stubReceiptPaymentStore{
		payments: map[uuid.UUID]*domain.Payment{
			paymentAID: paymentA,
		},
	}

	tests := []struct {
		name           string
		currentTenant  *domain.Tenant
		paymentStore   api.PaymentStore
		pathParam      string
		expectedStatus int
		verifyBody     func(t *testing.T, body []byte)
	}{
		{
			name:           "Tenant can fetch own payment receipt",
			currentTenant:  tenantA,
			paymentStore:   store,
			pathParam:      paymentAID.String(),
			expectedStatus: http.StatusOK,
			verifyBody: func(t *testing.T, body []byte) {
				var res struct {
					Receipt struct {
						PaymentID   string `json:"payment_id"`
						DueID       string `json:"due_id"`
						TenantID    string `json:"tenant_id"`
						AmountPaise int64  `json:"amount_paise"`
						MatchedBy   string `json:"matched_by"`
						UPITxnID    string `json:"upi_txn_id"`
						PropertyID  string `json:"property_id"`
					} `json:"receipt"`
				}
				if err := json.Unmarshal(body, &res); err != nil {
					t.Fatalf("failed to unmarshal receipt json: %v", err)
				}
				if res.Receipt.PaymentID != paymentAID.String() {
					t.Errorf("expected payment_id %s, got %s", paymentAID, res.Receipt.PaymentID)
				}
				if res.Receipt.DueID != dueAID.String() {
					t.Errorf("expected due_id %s, got %s", dueAID, res.Receipt.DueID)
				}
				if res.Receipt.TenantID != tenantA.ID.String() {
					t.Errorf("expected tenant_id %s, got %s", tenantA.ID, res.Receipt.TenantID)
				}
				if res.Receipt.AmountPaise != 1500000 {
					t.Errorf("expected amount_paise 1500000, got %d", res.Receipt.AmountPaise)
				}
				if res.Receipt.UPITxnID != upiTxn {
					t.Errorf("expected upi_txn_id %s, got %s", upiTxn, res.Receipt.UPITxnID)
				}
			},
		},
		{
			name:           "Cross-tenant isolation: Tenant B cannot fetch Tenant A receipt",
			currentTenant:  tenantB,
			paymentStore:   store,
			pathParam:      paymentAID.String(),
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "Non-existent payment returns 404",
			currentTenant:  tenantA,
			paymentStore:   store,
			pathParam:      uuid.New().String(),
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "Malformed UUID returns 400",
			currentTenant:  tenantA,
			paymentStore:   store,
			pathParam:      "not-a-valid-uuid",
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "Store unavailable returns 503",
			currentTenant:  tenantA,
			paymentStore:   nil,
			pathParam:      paymentAID.String(),
			expectedStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router, _ := setupTenantReceiptRouter(t, tc.paymentStore, tc.currentTenant)

			req := httptest.NewRequest(http.MethodGet, "/api/tenant/payments/"+tc.pathParam+"/receipt", nil)
			w := httptest.NewRecorder()

			router.ServeHTTP(w, req)

			if w.Code != tc.expectedStatus {
				t.Fatalf("expected HTTP status %d, got %d. Body: %s", tc.expectedStatus, w.Code, w.Body.String())
			}
			if tc.verifyBody != nil {
				tc.verifyBody(t, w.Body.Bytes())
			}
		})
	}
}
