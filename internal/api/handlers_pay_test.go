package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

type stubIntentStore struct {
	byOrder   map[string]*domain.PaymentIntent
	byCF      map[string]*domain.PaymentIntent
	paid      string
	lookupErr error
}

func (s *stubIntentStore) GetByOrderID(_ context.Context, orderID string) (*domain.PaymentIntent, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	p := s.byOrder[orderID]
	if p == nil {
		return nil, pgx.ErrNoRows
	}
	return p, nil
}
func (s *stubIntentStore) GetByCFPaymentID(_ context.Context, cfID string) (*domain.PaymentIntent, error) {
	p := s.byCF[cfID]
	if p == nil {
		return nil, payment.ErrDueNotOpen
	}
	return p, nil
}
func (s *stubIntentStore) MarkPaid(_ context.Context, id uuid.UUID, cfPaymentID string) error {
	s.paid = cfPaymentID
	return nil
}

type stubPay struct {
	n         int
	txn       string
	settleErr error
}

func (s *stubPay) MatchPayment(context.Context, uuid.UUID, string, int, time.Time, string) (*domain.Payment, error) {
	panic("unused")
}
func (s *stubPay) ManualMatch(context.Context, uuid.UUID, int, string, uuid.UUID) (*domain.Payment, error) {
	panic("unused")
}
func (s *stubPay) MarkCashPaid(context.Context, uuid.UUID, int, uuid.UUID, string) (*domain.Payment, error) {
	panic("unused")
}
func (s *stubPay) SettleDeposit(context.Context, uuid.UUID, int64, string) error { panic("unused") }
func (s *stubPay) BuildSummary(context.Context, uuid.UUID, string) (*payment.ReconciliationSummary, error) {
	panic("unused")
}
func (s *stubPay) GatewaySettle(_ context.Context, dueID uuid.UUID, amountPaise int, txnID string) (*domain.Payment, error) {
	s.n++
	s.txn = txnID
	if s.settleErr != nil {
		return nil, s.settleErr
	}
	return &domain.Payment{DueID: dueID, Amount: amountPaise, MatchedBy: domain.MatchedByCashfree}, nil
}

func TestCashfreeWebhookSettlesSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dueID := uuid.New()
	intent := &domain.PaymentIntent{ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-ABC-1", AmountPaise: 1050}
	intents := &stubIntentStore{
		byOrder: map[string]*domain.PaymentIntent{"pg-ABC-1": intent},
		byCF:    map[string]*domain.PaymentIntent{},
	}
	pay := &stubPay{}
	h := &Handlers{Deps: Deps{
		CashfreeSecret: "whsec",
		IntentStore:    intents,
		Payments:       pay,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"pg-ABC-1"},"payment":{"cf_payment_id":"9","payment_amount":10.5,"bank_reference":"UTR1"}}}`
	ts := "100"
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", sign("whsec", ts+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if pay.n != 1 || pay.txn != "UTR1" || intents.paid != "9" {
		t.Fatalf("pay=%+v paid=%s", pay, intents.paid)
	}
}

func TestCashfreeWebhookRejectsBadHMAC(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{Deps: Deps{CashfreeSecret: "whsec"}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(`{}`))
	req.Header.Set("x-webhook-timestamp", "1")
	req.Header.Set("x-webhook-signature", "nope")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestCashfreeWebhookDormantWithoutSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{Deps: Deps{}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(`{"type":"PAYMENT_SUCCESS_WEBHOOK"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestCashfreeWebhookUnknownOrderOK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pay := &stubPay{}
	h := &Handlers{Deps: Deps{
		CashfreeSecret: "whsec",
		IntentStore:    &stubIntentStore{byOrder: map[string]*domain.PaymentIntent{}, byCF: map[string]*domain.PaymentIntent{}},
		Payments:       pay,
	}}
	code := postSignedWebhook(t, h, `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"missing"},"payment":{"cf_payment_id":"9","payment_amount":10.5,"bank_reference":"UTR1"}}}`)
	if code != http.StatusOK || pay.n != 0 {
		t.Fatalf("code=%d n=%d", code, pay.n)
	}
}

func TestCashfreeWebhookLookupError500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{Deps: Deps{
		CashfreeSecret: "whsec",
		IntentStore:    &stubIntentStore{lookupErr: errors.New("db down")},
		Payments:       &stubPay{},
	}}
	code := postSignedWebhook(t, h, `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"pg-ABC-1"},"payment":{"cf_payment_id":"9","payment_amount":10.5,"bank_reference":"UTR1"}}}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("code=%d", code)
	}
}

func TestCashfreeWebhookAmountMismatchDoesNotMarkPaid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dueID := uuid.New()
	intent := &domain.PaymentIntent{ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-ABC-1", AmountPaise: 9999}
	intents := &stubIntentStore{
		byOrder: map[string]*domain.PaymentIntent{"pg-ABC-1": intent},
		byCF:    map[string]*domain.PaymentIntent{},
	}
	pay := &stubPay{}
	h := &Handlers{Deps: Deps{CashfreeSecret: "whsec", IntentStore: intents, Payments: pay}}
	code := postSignedWebhook(t, h, `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"pg-ABC-1"},"payment":{"cf_payment_id":"9","payment_amount":10.5,"bank_reference":"UTR1"}}}`)
	if code != http.StatusOK || pay.n != 0 || intents.paid != "" {
		t.Fatalf("code=%d n=%d paid=%s", code, pay.n, intents.paid)
	}
}

func TestCashfreeWebhookDueNotOpenDoesNotMarkPaid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dueID := uuid.New()
	intent := &domain.PaymentIntent{ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-ABC-1", AmountPaise: 1050}
	intents := &stubIntentStore{
		byOrder: map[string]*domain.PaymentIntent{"pg-ABC-1": intent},
		byCF:    map[string]*domain.PaymentIntent{},
	}
	pay := &stubPay{settleErr: payment.ErrDueNotOpen}
	h := &Handlers{Deps: Deps{CashfreeSecret: "whsec", IntentStore: intents, Payments: pay}}
	code := postSignedWebhook(t, h, `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"pg-ABC-1"},"payment":{"cf_payment_id":"9","payment_amount":10.5,"bank_reference":"UTR1"}}}`)
	if code != http.StatusOK || intents.paid != "" {
		t.Fatalf("code=%d paid=%s", code, intents.paid)
	}
}

func postSignedWebhook(t *testing.T, h *Handlers, body string) int {
	t.Helper()
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)
	ts := "100"
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", sign("whsec", ts+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func sign(secret, msg string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
