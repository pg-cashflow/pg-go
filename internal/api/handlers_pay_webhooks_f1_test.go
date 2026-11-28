package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type spyOrderSettler struct {
	orderID     string
	cfPaymentID string
	txnID       string
	amountPaise int64
	calls       int
}

func (s *spyOrderSettler) SettleGatewayOrder(
	_ context.Context,
	orderID string,
	cfPaymentID string,
	txnID string,
	amountPaise int64,
	_ *domain.PaymentIntent,
	_ []byte,
) error {
	s.calls++
	s.orderID = orderID
	s.cfPaymentID = cfPaymentID
	s.txnID = txnID
	s.amountPaise = amountPaise
	return nil
}

type stubWebhookIntentStore struct {
	intent *domain.PaymentIntent
}

func (s *stubWebhookIntentStore) GetByOrderID(context.Context, string) (*domain.PaymentIntent, error) {
	return s.intent, nil
}
func (s *stubWebhookIntentStore) GetByCFPaymentID(context.Context, string) (*domain.PaymentIntent, error) {
	return nil, nil
}
func (s *stubWebhookIntentStore) GetDuesSnapshot(context.Context, uuid.UUID) ([]domain.PaymentIntentDue, error) {
	return nil, nil
}
func (s *stubWebhookIntentStore) MarkPaid(context.Context, uuid.UUID, string) error {
	return nil
}
func (s *stubWebhookIntentStore) LatestOpenForDue(context.Context, uuid.UUID) (*domain.PaymentIntent, error) {
	return nil, nil
}

// F1 regression: handlePaymentSuccessWebhook must pass BankReference separately (not TxnID() fallback
// to CFPaymentID) so bank_reference never falls back to cf_payment_id.
func TestPaymentSuccessWebhook_DoesNotFallbackBankRefToCFPaymentID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewReader([]byte("{}")))

	spy := &spyOrderSettler{}
	h := &Handlers{
		Deps: Deps{
			IntentStore: &stubWebhookIntentStore{
				intent: &domain.PaymentIntent{
					ID:              uuid.New(),
					DueID:           uuid.New(),
					ProviderOrderID: "order_f1_nobankref",
					AmountPaise:     10000,
					Status:          domain.IntentCreated,
				},
			},
			OrderSettler: spy,
		},
	}

	succ := cashfree.SuccessWebhook{
		OrderID:         "order_f1_nobankref",
		CFPaymentID:     "cf_pay_only_123",
		BankReference:   "",
		PaymentStatus:   "SUCCESS",
		PaymentCurrency: "INR",
		AmountPaise:     10000,
	}

	h.handlePaymentSuccessWebhook(c, succ, &domain.WebhookEvent{ID: uuid.New()}, "")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	if spy.calls != 1 {
		t.Fatalf("expected 1 SettleGatewayOrder call, got %d", spy.calls)
	}
	if spy.cfPaymentID != "cf_pay_only_123" {
		t.Fatalf("expected cfPaymentID cf_pay_only_123, got %q", spy.cfPaymentID)
	}
	if spy.txnID != "" {
		t.Fatalf("F1 regression: expected empty bank_reference when absent, got %q (fell back to cf_payment_id via TxnID())", spy.txnID)
	}
}
