package api

import (
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
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type mockVerifyDueStore struct {
	dues map[uuid.UUID]*domain.Due
}

func (m *mockVerifyDueStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	if d, ok := m.dues[id]; ok {
		return d, nil
	}
	return nil, errors.New("due not found")
}

func (m *mockVerifyDueStore) GetByIDForUpdate(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	return m.GetByID(context.Background(), id)
}

func (m *mockVerifyDueStore) Update(_ context.Context, _ *domain.Due) error {
	return nil
}

func (m *mockVerifyDueStore) ListByTenant(_ context.Context, _ uuid.UUID) ([]domain.Due, error) {
	return nil, nil
}

func (m *mockVerifyDueStore) List(_ context.Context, _ postgres.DueListFilter) ([]domain.Due, error) {
	return nil, nil
}

type mockVerifyIntentStore struct {
	intent   *domain.PaymentIntent
	snapshot []domain.PaymentIntentDue
	paidCFID string
}

func (m *mockVerifyIntentStore) GetByOrderID(_ context.Context, _ string) (*domain.PaymentIntent, error) {
	return m.intent, nil
}
func (m *mockVerifyIntentStore) GetByCFPaymentID(_ context.Context, _ string) (*domain.PaymentIntent, error) {
	return m.intent, nil
}
func (m *mockVerifyIntentStore) LatestOpenForDue(_ context.Context, _ uuid.UUID) (*domain.PaymentIntent, error) {
	return m.intent, nil
}
func (m *mockVerifyIntentStore) GetDuesSnapshot(_ context.Context, _ uuid.UUID) ([]domain.PaymentIntentDue, error) {
	return m.snapshot, nil
}
func (m *mockVerifyIntentStore) MarkPaid(_ context.Context, _ uuid.UUID, cfPaymentID string) error {
	m.paidCFID = cfPaymentID
	return nil
}
func (m *mockVerifyIntentStore) Create(_ context.Context, _ *domain.PaymentIntent) error {
	return nil
}

type mockVerifyCFClient struct {
	cfID    string
	bankRef string
	amount  int64
	ok      bool
}

func (m *mockVerifyCFClient) FetchSuccessfulPayment(_ context.Context, _ string) (string, string, int64, bool, error) {
	return m.cfID, m.bankRef, m.amount, m.ok, nil
}

func (m *mockVerifyCFClient) CreateRefund(_ context.Context, _, _ string, _ int64, _, _ string) (*cashfree.RefundDetails, error) {
	return nil, nil
}

func (m *mockVerifyCFClient) FetchRefundStatus(_ context.Context, _, _ string) (*cashfree.RefundDetails, error) {
	return nil, nil
}

type mockVerifyPayService struct {
	settledDues []uuid.UUID
	settledTxns []string
	settleErr   error
}

func (m *mockVerifyPayService) GatewaySettle(_ context.Context, dueID uuid.UUID, _ int64, txnID string, _ ...string) (*domain.Payment, error) {
	if m.settleErr != nil {
		return nil, m.settleErr
	}
	m.settledDues = append(m.settledDues, dueID)
	m.settledTxns = append(m.settledTxns, txnID)
	return &domain.Payment{ID: uuid.New(), DueID: dueID}, nil
}

func (m *mockVerifyPayService) VerifyPayment(_ context.Context, _ payment.VerifyPaymentInput) (*domain.Payment, error) {
	return nil, nil
}

func (m *mockVerifyPayService) CorrectPayment(_ context.Context, _ payment.CorrectPaymentInput) (*domain.FinancialCorrection, error) {
	return nil, nil
}

func (m *mockVerifyPayService) MarkCashPaid(context.Context, uuid.UUID, int64, uuid.UUID, string) (*domain.Payment, error) {
	panic("unused")
}
func (m *mockVerifyPayService) SettleDeposit(context.Context, uuid.UUID, int64, string) error {
	panic("unused")
}
func (m *mockVerifyPayService) MatchPayment(context.Context, uuid.UUID, string, int64, time.Time, string) (*domain.Payment, error) {
	panic("unused")
}
func (m *mockVerifyPayService) SuggestMatch(context.Context, uuid.UUID, int64, time.Time, string) (*payment.MatchResult, error) {
	panic("unused")
}
func (m *mockVerifyPayService) ManualMatch(context.Context, uuid.UUID, int64, string, uuid.UUID) (*domain.Payment, error) {
	panic("unused")
}
func (m *mockVerifyPayService) BuildSummary(context.Context, uuid.UUID, string) (*payment.ReconciliationSummary, error) {
	panic("unused")
}

func TestTenantDuePayVerify_MultiDueBatchSettlement(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tenantID := uuid.New()
	propID := uuid.New()
	due1ID := uuid.New()
	due2ID := uuid.New()
	intentID := uuid.New()

	due1 := &domain.Due{
		ID:         due1ID,
		TenantID:   tenantID,
		PropertyID: propID,
		Amount:     300000,
		Status:     domain.DueStatusPending,
	}
	due2 := &domain.Due{
		ID:         due2ID,
		TenantID:   tenantID,
		PropertyID: propID,
		Amount:     200000,
		Status:     domain.DueStatusPending,
	}

	dueStore := &mockVerifyDueStore{
		dues: map[uuid.UUID]*domain.Due{
			due1ID: due1,
			due2ID: due2,
		},
	}

	intentStore := &mockVerifyIntentStore{
		intent: &domain.PaymentIntent{
			ID:              intentID,
			ProviderOrderID: "order_batch_123",
			Status:          domain.IntentCreated,
		},
		snapshot: []domain.PaymentIntentDue{
			{DueID: due1ID, AmountPaise: 300000},
			{DueID: due2ID, AmountPaise: 200000},
		},
	}

	cfClient := &mockVerifyCFClient{
		cfID:    "cf_pay_999",
		bankRef: "bank_ref_888",
		amount:  500000,
		ok:      true,
	}

	paySvc := &mockVerifyPayService{}

	h := &Handlers{
		Deps: Deps{
			DueStore:       dueStore,
			IntentStore:    intentStore,
			CashfreeClient: cfClient,
			Payments:       paySvc,
		},
	}

	r := gin.New()
	r.POST("/tenant/dues/:id/pay/verify", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, &domain.Tenant{
			ID:         tenantID,
			PropertyID: propID,
			Status:     domain.TenantStatusActive,
		})
		h.TenantDuePayVerify(c)
	})

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/tenant/dues/%s/pay/verify", due1ID), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp["paid"] {
		t.Fatalf("expected paid=true, got false")
	}

	// Verify both dues were settled
	if len(paySvc.settledDues) != 2 {
		t.Fatalf("expected 2 settled dues, got %d", len(paySvc.settledDues))
	}
	if paySvc.settledDues[0] != due1ID || paySvc.settledDues[1] != due2ID {
		t.Errorf("settled dues mismatch: %v", paySvc.settledDues)
	}

	// Verify distinct transaction IDs were used to prevent idempotency early exit
	if paySvc.settledTxns[0] == paySvc.settledTxns[1] {
		t.Errorf("expected distinct txn IDs for batch dues, got identical: %s", paySvc.settledTxns[0])
	}

	expectedTxn1 := fmt.Sprintf("bank_ref_888:due:%s", due1ID)
	expectedTxn2 := fmt.Sprintf("bank_ref_888:due:%s", due2ID)
	if paySvc.settledTxns[0] != expectedTxn1 || paySvc.settledTxns[1] != expectedTxn2 {
		t.Errorf("unexpected txn IDs: got %v, want [%s, %s]", paySvc.settledTxns, expectedTxn1, expectedTxn2)
	}

	// Verify intent was marked paid
	if intentStore.paidCFID != "cf_pay_999" {
		t.Errorf("expected intent marked paid with cf_pay_999, got %q", intentStore.paidCFID)
	}
}
