package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func signHMACSHA256(secret, message string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// TestWebhookReplayParity_Payments verifies payments webhook rejects stale timestamps even when tolerance is 0 or unconfigured.
func TestWebhookReplayParity_Payments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-pay-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_1"},"payment":{"cf_payment_id":123,"payment_status":"SUCCESS","payment_amount":100}}}`

	for _, tc := range []struct {
		name      string
		tolerance int
	}{
		{name: "configured tolerance 300", tolerance: 300},
		{name: "zero tolerance defaults to 300", tolerance: 0},
		{name: "negative tolerance defaults to 300", tolerance: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handlers{Deps: Deps{
				CashfreeSecret:      secret,
				WebhookToleranceSec: tc.tolerance,
			}}
			r := gin.New()
			r.POST("/webhooks/cashfree", h.CashfreeWebhook)

			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
			req.Header.Set("x-webhook-timestamp", oldTS)
			req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized for stale payments webhook (tolerance=%d), got %d", tc.tolerance, w.Code)
			}
		})
	}
}

// TestWebhookReplayParity_Payouts verifies payouts webhook rejects stale timestamps even when tolerance is 0 or unconfigured.
func TestWebhookReplayParity_Payouts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-payout-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"TRANSFER_SUCCESS","data":{"transfer_id":"tr_1","status":"SUCCESS"}}`

	for _, tc := range []struct {
		name      string
		tolerance int
	}{
		{name: "configured tolerance 300", tolerance: 300},
		{name: "zero tolerance defaults to 300", tolerance: 0},
		{name: "negative tolerance defaults to 300", tolerance: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handlers{Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				WebhookToleranceSec:         tc.tolerance,
			}}
			r := gin.New()
			r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewBufferString(body))
			req.Header.Set("x-webhook-timestamp", oldTS)
			req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized for stale payouts webhook (tolerance=%d), got %d", tc.tolerance, w.Code)
			}
		})
	}
}

// TestWebhookReplayParity_Settlement verifies settlement webhook rejects stale timestamps even when tolerance is 0 or unconfigured.
func TestWebhookReplayParity_Settlement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-settle-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"SETTLEMENT_SUCCESS","data":{"settlement_id":"set_1"}}`

	for _, tc := range []struct {
		name      string
		tolerance int
	}{
		{name: "configured tolerance 300", tolerance: 300},
		{name: "zero tolerance defaults to 300", tolerance: 0},
		{name: "negative tolerance defaults to 300", tolerance: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handlers{Deps: Deps{
				CashfreeSecret:      secret,
				WebhookToleranceSec: tc.tolerance,
			}}
			r := gin.New()
			r.POST("/webhooks/cashfree/settlements", h.CashfreeSettlementWebhook)

			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/settlements", bytes.NewBufferString(body))
			req.Header.Set("x-webhook-timestamp", oldTS)
			req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized for stale settlement webhook (tolerance=%d), got %d", tc.tolerance, w.Code)
			}
		})
	}
}

type retryMockGatewayRepo struct {
	stubGatewayRepo
	processed map[string]bool
}

func (m *retryMockGatewayRepo) HasProcessedEvent(_ context.Context, provider, eventType, providerRefID, _ string) (bool, error) {
	key := fmt.Sprintf("%s:%s:%s", provider, eventType, providerRefID)
	return m.processed[key], nil
}

// TestWebhookRetry_AlreadyProcessed_Returns200 verifies that authentic webhooks that were already
// processed return 200 OK even if the timestamp is older than the 300s tolerance (e.g. 600s/10m retry).
func TestWebhookRetry_AlreadyProcessed_Returns200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-pay-123"
	// Old timestamp: 600s ago (exceeds default 300s drift window)
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_retry_1"},"payment":{"cf_payment_id":998877,"payment_status":"SUCCESS","payment_amount":100}}}`

	repo := &retryMockGatewayRepo{
		processed: map[string]bool{
			"cashfree:PAYMENT_SETTLED:998877": true,
		},
	}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		WebhookToleranceSec: 300,
		GatewayPaymentRepo:  repo,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", oldTS)
	req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for already-processed retried webhook even with old timestamp, got %d", w.Code)
	}
}

type mockCFVerifierClient struct {
	cfID    string
	bankRef string
	amount  int64
	ok      bool
	err     error
}

func (m *mockCFVerifierClient) FetchSuccessfulPayment(_ context.Context, _ string) (string, string, int64, bool, error) {
	return m.cfID, m.bankRef, m.amount, m.ok, m.err
}
func (m *mockCFVerifierClient) CreateRefund(context.Context, string, string, int64, string, string) (*cashfree.RefundDetails, error) {
	return nil, nil
}
func (m *mockCFVerifierClient) FetchRefundStatus(context.Context, string, string) (*cashfree.RefundDetails, error) {
	return nil, nil
}
func (m *mockCFVerifierClient) CancelRefund(context.Context, string, string, string) (*cashfree.RefundDetails, error) {
	return nil, nil
}
func (m *mockCFVerifierClient) IsSandbox() bool { return true }

type mockParitySettler struct {
	settled bool
}

func (m *mockParitySettler) SettleGatewayOrder(_ context.Context, _ string, _ string, _ string, _ int64, _ *domain.PaymentIntent, _ []byte) error {
	m.settled = true
	return nil
}

type mockParityIntentStore struct {
	intent *domain.PaymentIntent
}

func (m *mockParityIntentStore) GetByOrderID(_ context.Context, _ string) (*domain.PaymentIntent, error) {
	return m.intent, nil
}
func (m *mockParityIntentStore) GetByCFPaymentID(context.Context, string) (*domain.PaymentIntent, error) {
	return m.intent, nil
}
func (m *mockParityIntentStore) MarkPaid(context.Context, uuid.UUID, string) error { return nil }
func (m *mockParityIntentStore) GetDuesSnapshot(context.Context, uuid.UUID) ([]domain.PaymentIntentDue, error) {
	return nil, nil
}
func (m *mockParityIntentStore) LatestOpenForDue(context.Context, uuid.UUID) (*domain.PaymentIntent, error) {
	return m.intent, nil
}

// TestWebhookRetry_TimestampDrift_VerifiedByGatewayAPI_Success verifies that when an unhandled webhook
// arrives with a stale timestamp (>300s, e.g. a Cashfree retry at 10m or 30m), but Cashfree's live REST
// API confirms the payment is valid and captured, the webhook is accepted and settled.
func TestWebhookRetry_TimestampDrift_VerifiedByGatewayAPI_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-pay-123"
	// Old timestamp: 600s ago (exceeds default 300s drift window)
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_verify_retry"},"payment":{"cf_payment_id":554433,"payment_status":"SUCCESS","payment_amount":100,"payment_currency":"INR"}}}`

	repo := &retryMockGatewayRepo{
		processed: map[string]bool{}, // NOT processed yet
	}
	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: "ord_verify_retry",
		AmountPaise:     10000,
	}
	cfClient := &mockCFVerifierClient{
		cfID:    "554433",
		bankRef: "UPI_BANK_REF_99",
		amount:  10000,
		ok:      true,
	}
	settler := &mockParitySettler{}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		WebhookToleranceSec: 300,
		GatewayPaymentRepo:  repo,
		IntentStore:         &mockParityIntentStore{intent: intent},
		CashfreeClient:      cfClient,
		OrderSettler:        settler,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", oldTS)
	req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for delayed retry verified by gateway API, got %d", w.Code)
	}
	if !settler.settled {
		t.Fatalf("expected payment to be settled via OrderSettler")
	}
}

// TestWebhookRetry_TimestampDrift_PaymentIDMismatch_FailsClosed verifies that when a stale webhook
// arrives with a payment ID that does NOT match the successful payment ID returned by Cashfree API,
// the fallback rejects the webhook with HTTP 401 rather than settling an unverified payment ID.
func TestWebhookRetry_TimestampDrift_PaymentIDMismatch_FailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-pay-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	// Webhook claims cf_payment_id: 112233
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_mismatch"},"payment":{"cf_payment_id":112233,"payment_status":"SUCCESS","payment_amount":100,"payment_currency":"INR"}}}`

	repo := &retryMockGatewayRepo{processed: map[string]bool{}}
	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: "ord_mismatch",
		AmountPaise:     10000,
	}
	// Gateway reports a DIFFERENT payment ID: 998877
	cfClient := &mockCFVerifierClient{
		cfID:    "998877",
		bankRef: "UPI_BANK_REF_DIFF",
		amount:  10000,
		ok:      true,
	}
	settler := &mockParitySettler{}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		WebhookToleranceSec: 300,
		GatewayPaymentRepo:  repo,
		IntentStore:         &mockParityIntentStore{intent: intent},
		CashfreeClient:      cfClient,
		OrderSettler:        settler,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", oldTS)
	req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for payment ID mismatch in stale webhook fallback, got %d", w.Code)
	}
	if settler.settled {
		t.Fatalf("invariant violated: mismatched payment must not be settled")
	}
}

// TestWebhookRetry_TimestampDrift_AmountMismatch_FailsClosed verifies that when amount does not match
// between webhook and gateway live confirmation, the fallback fails closed.
func TestWebhookRetry_TimestampDrift_AmountMismatch_FailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-pay-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_amount_mismatch"},"payment":{"cf_payment_id":554433,"payment_status":"SUCCESS","payment_amount":100,"payment_currency":"INR"}}}`

	repo := &retryMockGatewayRepo{processed: map[string]bool{}}
	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: "ord_amount_mismatch",
		AmountPaise:     10000,
	}
	// Gateway reports same ID but DIFFERENT amount (20000 paise vs 10000 paise)
	cfClient := &mockCFVerifierClient{
		cfID:    "554433",
		bankRef: "UPI_BANK_REF_99",
		amount:  20000,
		ok:      true,
	}
	settler := &mockParitySettler{}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		WebhookToleranceSec: 300,
		GatewayPaymentRepo:  repo,
		IntentStore:         &mockParityIntentStore{intent: intent},
		CashfreeClient:      cfClient,
		OrderSettler:        settler,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", oldTS)
	req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for amount mismatch in stale webhook fallback, got %d", w.Code)
	}
	if settler.settled {
		t.Fatalf("invariant violated: mismatched amount must not be settled")
	}
}

// TestWebhookRetry_TimestampDrift_BlankPaymentID_FailsClosed verifies that blank payment IDs fail closed.
func TestWebhookRetry_TimestampDrift_BlankPaymentID_FailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-pay-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_blank_id"},"payment":{"cf_payment_id":"","payment_status":"SUCCESS","payment_amount":100,"payment_currency":"INR"}}}`

	repo := &retryMockGatewayRepo{processed: map[string]bool{}}
	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: "ord_blank_id",
		AmountPaise:     10000,
	}
	cfClient := &mockCFVerifierClient{
		cfID:   "",
		amount: 10000,
		ok:     true,
	}
	settler := &mockParitySettler{}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		WebhookToleranceSec: 300,
		GatewayPaymentRepo:  repo,
		IntentStore:         &mockParityIntentStore{intent: intent},
		CashfreeClient:      cfClient,
		OrderSettler:        settler,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", oldTS)
	req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for blank payment ID, got %d", w.Code)
	}
	if settler.settled {
		t.Fatalf("invariant violated: blank payment ID must not be settled")
	}
}

// TestWebhookRetry_TimestampDrift_MultipleSuccessfulPayments_FailsClosed verifies that if Cashfree returns
// ambiguous captures (multiple distinct successful payments), the fallback rejects with 401.
func TestWebhookRetry_TimestampDrift_MultipleSuccessfulPayments_FailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-pay-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_ambiguous"},"payment":{"cf_payment_id":554433,"payment_status":"SUCCESS","payment_amount":100,"payment_currency":"INR"}}}`

	repo := &retryMockGatewayRepo{processed: map[string]bool{}}
	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: "ord_ambiguous",
		AmountPaise:     10000,
	}
	cfClient := &mockCFVerifierClient{
		cfID:   "",
		amount: 0,
		ok:     false,
		err:    fmt.Errorf("multiple distinct successful payments"),
	}
	settler := &mockParitySettler{}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		WebhookToleranceSec: 300,
		GatewayPaymentRepo:  repo,
		IntentStore:         &mockParityIntentStore{intent: intent},
		CashfreeClient:      cfClient,
		OrderSettler:        settler,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", oldTS)
	req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for ambiguous captures, got %d", w.Code)
	}
	if settler.settled {
		t.Fatalf("invariant violated: ambiguous captures must not be settled")
	}
}

// TestWebhookRetry_TimestampDrift_GatewayError_FailsClosed verifies that if Cashfree returns an error or timeout,
// the fallback fails closed with 401.
func TestWebhookRetry_TimestampDrift_GatewayError_FailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-pay-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_timeout"},"payment":{"cf_payment_id":554433,"payment_status":"SUCCESS","payment_amount":100,"payment_currency":"INR"}}}`

	repo := &retryMockGatewayRepo{processed: map[string]bool{}}
	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: "ord_timeout",
		AmountPaise:     10000,
	}
	cfClient := &mockCFVerifierClient{
		cfID:   "",
		amount: 0,
		ok:     false,
		err:    context.DeadlineExceeded,
	}
	settler := &mockParitySettler{}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		WebhookToleranceSec: 300,
		GatewayPaymentRepo:  repo,
		IntentStore:         &mockParityIntentStore{intent: intent},
		CashfreeClient:      cfClient,
		OrderSettler:        settler,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", oldTS)
	req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized on gateway error, got %d", w.Code)
	}
	if settler.settled {
		t.Fatalf("invariant violated: gateway error must not be settled")
	}
}
