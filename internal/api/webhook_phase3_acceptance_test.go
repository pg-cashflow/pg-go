package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func signTestHMAC(secret, message string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type phase3MockGatewayRepo struct {
	mu             sync.Mutex
	processedMap   map[string]bool
	unmatchedCount int
	webhookEvents  []*domain.WebhookEvent
	unmatchedRows  []map[string]any
}

func newPhase3MockGatewayRepo() *phase3MockGatewayRepo {
	return &phase3MockGatewayRepo{
		processedMap: make(map[string]bool),
	}
}

func (m *phase3MockGatewayRepo) Create(context.Context, *domain.Payment) error { return nil }
func (m *phase3MockGatewayRepo) GetByID(context.Context, uuid.UUID) (*domain.Payment, error) {
	return nil, nil
}
func (m *phase3MockGatewayRepo) GetByUPITxnID(context.Context, string) (*domain.Payment, error) {
	return nil, nil
}
func (m *phase3MockGatewayRepo) GetByCFPaymentID(context.Context, string) (*domain.Payment, error) {
	return nil, nil
}
func (m *phase3MockGatewayRepo) RecordProcessedEvent(_ context.Context, provider, eventType, providerRefID, eventStatus string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%s:%s:%s:%s", provider, eventType, providerRefID, eventStatus)
	if m.processedMap[key] {
		return false, nil
	}
	m.processedMap[key] = true
	return true, nil
}
func (m *phase3MockGatewayRepo) DeleteProcessedEvent(_ context.Context, provider, eventType, providerRefID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.processedMap {
		if strings.HasPrefix(k, fmt.Sprintf("%s:%s:%s:", provider, eventType, providerRefID)) {
			delete(m.processedMap, k)
		}
	}
	return nil
}
func (m *phase3MockGatewayRepo) HasProcessedEvent(_ context.Context, provider, eventType, providerRefID, eventStatus string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%s:%s:%s:%s", provider, eventType, providerRefID, eventStatus)
	return m.processedMap[key], nil
}
func (m *phase3MockGatewayRepo) CreateAllocation(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (m *phase3MockGatewayRepo) ListAllocationsByPayment(context.Context, uuid.UUID) ([]domain.PaymentAllocation, error) {
	return nil, nil
}
func (m *phase3MockGatewayRepo) CreateWebhookEvent(_ context.Context, evt *domain.WebhookEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	evt.ID = uuid.New()
	m.webhookEvents = append(m.webhookEvents, evt)
	return nil
}
func (m *phase3MockGatewayRepo) UpdateWebhookEventStatus(_ context.Context, id uuid.UUID, status string, errMsg *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.webhookEvents {
		if e.ID == id {
			e.ProcessingStatus = status
			e.ErrorMessage = errMsg
		}
	}
	return nil
}
func (m *phase3MockGatewayRepo) RecordUnmatchedReceipt(_ context.Context, orderID, cfPaymentID string, intentID *uuid.UUID, amountPaise int64, failureReason string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unmatchedCount++
	m.unmatchedRows = append(m.unmatchedRows, map[string]any{
		"order_id":       orderID,
		"cf_payment_id":  cfPaymentID,
		"amount_paise":   amountPaise,
		"failure_reason": failureReason,
	})
	return nil
}
func (m *phase3MockGatewayRepo) GetRefundByID(context.Context, uuid.UUID) (*domain.GatewayRefund, error) {
	return nil, nil
}
func (m *phase3MockGatewayRepo) GetRefundByCFRefundID(context.Context, string) (*domain.GatewayRefund, error) {
	return nil, nil
}
func (m *phase3MockGatewayRepo) GetRefundByReference(context.Context, string) (*domain.GatewayRefund, error) {
	return nil, nil
}
func (m *phase3MockGatewayRepo) GetRefundByPaymentAndIdempotency(context.Context, uuid.UUID, string) (*domain.GatewayRefund, error) {
	return nil, nil
}
func (m *phase3MockGatewayRepo) GetPaymentRefundedPaise(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}
func (m *phase3MockGatewayRepo) ListRefundsByPayment(context.Context, uuid.UUID) ([]domain.GatewayRefund, error) {
	return nil, nil
}
func (m *phase3MockGatewayRepo) CreateOrUpdateRefund(context.Context, *domain.GatewayRefund) error {
	return nil
}
func (m *phase3MockGatewayRepo) CreateRefundAllocation(context.Context, *domain.RefundAllocation) error {
	return nil
}
func (m *phase3MockGatewayRepo) GetDueNetPaidPaise(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}
func (m *phase3MockGatewayRepo) GetDueUnreservedPaidPaise(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}
func (m *phase3MockGatewayRepo) GetPaymentDueRefundedPaise(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (int64, error) {
	return 0, nil
}
func (m *phase3MockGatewayRepo) ListStaleNonTerminalRefunds(context.Context, time.Time) ([]domain.GatewayRefund, error) {
	return nil, nil
}

type phase3MockOrderSettler struct {
	mu           sync.Mutex
	settleCalls  int
	settledOrder string
	failErr      error
}

func (s *phase3MockOrderSettler) SettleGatewayOrder(_ context.Context, orderID, _ string, _ string, _ int64, _ *domain.PaymentIntent, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settleCalls++
	if s.failErr != nil {
		return s.failErr
	}
	s.settledOrder = orderID
	return nil
}

type phase3MockIntentStore struct {
	intent *domain.PaymentIntent
}

func (s *phase3MockIntentStore) Create(context.Context, *domain.PaymentIntent) error { return nil }
func (s *phase3MockIntentStore) GetByID(context.Context, uuid.UUID) (*domain.PaymentIntent, error) {
	return s.intent, nil
}
func (s *phase3MockIntentStore) GetByOrderID(_ context.Context, orderID string) (*domain.PaymentIntent, error) {
	if s.intent != nil && s.intent.ProviderOrderID == orderID {
		return s.intent, nil
	}
	return nil, nil
}
func (s *phase3MockIntentStore) GetByCFPaymentID(context.Context, string) (*domain.PaymentIntent, error) {
	return s.intent, nil
}
func (s *phase3MockIntentStore) MarkPaid(context.Context, uuid.UUID, string) error { return nil }
func (s *phase3MockIntentStore) MarkFailed(context.Context, uuid.UUID) error        { return nil }
func (s *phase3MockIntentStore) MarkPending(context.Context, uuid.UUID) error       { return nil }
func (s *phase3MockIntentStore) SaveDuesSnapshot(context.Context, uuid.UUID, []domain.PaymentIntentDue) error {
	return nil
}
func (s *phase3MockIntentStore) GetDuesSnapshot(context.Context, uuid.UUID) ([]domain.PaymentIntentDue, error) {
	return nil, nil
}
func (s *phase3MockIntentStore) LatestOpenForDue(context.Context, uuid.UUID) (*domain.PaymentIntent, error) {
	return s.intent, nil
}

// TestPhase3_PGAndPayoutsSecretRotation verifies that both PG and Payouts webhook handlers
// support candidate secret rotation, testing candidate secrets in chronological order (oldest first).
func TestPhase3_PGAndPayoutsSecretRotation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secretOld = "secret_active_v1_oldest"
	const secretNew = "secret_active_v2_newest"

	t.Run("PG_Webhook_AcceptsOldestActiveSecretDuringRotation", func(t *testing.T) {
		repo := newPhase3MockGatewayRepo()
		settler := &phase3MockOrderSettler{}
		intent := &domain.PaymentIntent{
			ID:              uuid.New(),
			ProviderOrderID: "order_rot_1",
			AmountPaise:     50000,
			Status:          domain.IntentCreated,
		}

		h := &Handlers{Deps: Deps{
			CashfreeSecret: secretNew,
			CashfreeSecrets: map[string]string{
				"v1": secretOld,
				"v2": secretNew,
			},
			GatewayPaymentRepo:  repo,
			OrderSettler:        settler,
			IntentStore:         &phase3MockIntentStore{intent: intent},
			WebhookToleranceSec: 300,
		}}
		r := gin.New()
		r.POST("/webhooks/cashfree", h.CashfreeWebhook)

		body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"order_rot_1","order_currency":"INR"},"payment":{"cf_payment_id":991,"payment_status":"SUCCESS","payment_amount":500.00,"payment_currency":"INR"}}}`
		ts := fmt.Sprintf("%d", time.Now().Unix())

		// Request signed with old secret (v1)
		reqOld := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		reqOld.Header.Set("x-webhook-timestamp", ts)
		reqOld.Header.Set("x-webhook-signature", signTestHMAC(secretOld, ts+body))
		wOld := httptest.NewRecorder()
		r.ServeHTTP(wOld, reqOld)

		if wOld.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for PG webhook signed with oldest active secret, got %d", wOld.Code)
		}

		// Request signed with new secret (v2)
		reqNew := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		reqNew.Header.Set("x-webhook-timestamp", ts)
		reqNew.Header.Set("x-webhook-signature", signTestHMAC(secretNew, ts+body))
		wNew := httptest.NewRecorder()
		r.ServeHTTP(wNew, reqNew)

		if wNew.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for PG webhook signed with newer active secret, got %d", wNew.Code)
		}

		// Request signed with untrusted secret -> 401 Unauthorized
		reqBad := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		reqBad.Header.Set("x-webhook-timestamp", ts)
		reqBad.Header.Set("x-webhook-signature", signTestHMAC("untrusted_secret_key", ts+body))
		wBad := httptest.NewRecorder()
		r.ServeHTTP(wBad, reqBad)

		if wBad.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized for PG webhook with invalid signature, got %d", wBad.Code)
		}
	})

	t.Run("Payouts_Webhook_AcceptsOldestActiveSecretDuringRotation", func(t *testing.T) {
		itemID := uuid.New()
		repo := newPhase3MockGatewayRepo()
		mockPayoutStore := &mockPayoutWebhookStore{
			getItemFunc: func(context.Context, uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{
					ID:          itemID,
					Status:      domain.PayoutProcessing,
					AmountPaise: 1000,
				}, nil
			},
			updateStatusFunc: func(context.Context, pgx.Tx, uuid.UUID, domain.PayoutItemStatus, *string, *string, *time.Time, *string) error {
				return nil
			},
		}

		h := &Handlers{Deps: Deps{
			CashfreePayoutWebhookSecret: secretNew,
			CashfreePayoutWebhookSecrets: map[string]string{
				"v1": secretOld,
				"v2": secretNew,
			},
			GatewayPaymentRepo:  repo,
			PayoutWebhookRepo:   mockPayoutStore,
			WebhookToleranceSec: 300,
		}}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		body := fmt.Sprintf(`{"event":"TRANSFER_FAILED","data":{"transfer_id":"pgo_%s","status":"FAILED"}}`, itemID)
		ts := fmt.Sprintf("%d", time.Now().Unix())

		// Signed with oldest active secret (v1)
		reqOld := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewBufferString(body))
		reqOld.Header.Set("x-webhook-timestamp", ts)
		reqOld.Header.Set("x-webhook-signature", signTestHMAC(secretOld, ts+body))
		wOld := httptest.NewRecorder()
		r.ServeHTTP(wOld, reqOld)

		if wOld.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for Payouts webhook signed with oldest active secret, got %d", wOld.Code)
		}

		// Signed with new secret (v2)
		reqNew := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewBufferString(body))
		reqNew.Header.Set("x-webhook-timestamp", ts)
		reqNew.Header.Set("x-webhook-signature", signTestHMAC(secretNew, ts+body))
		wNew := httptest.NewRecorder()
		r.ServeHTTP(wNew, reqNew)

		if wNew.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for Payouts webhook signed with newer active secret, got %d", wNew.Code)
		}

		// Signed with untrusted secret -> 401
		reqBad := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewBufferString(body))
		reqBad.Header.Set("x-webhook-timestamp", ts)
		reqBad.Header.Set("x-webhook-signature", signTestHMAC("untrusted_secret_key", ts+body))
		wBad := httptest.NewRecorder()
		r.ServeHTTP(wBad, reqBad)

		if wBad.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized for Payouts webhook with invalid signature, got %d", wBad.Code)
		}
	})
}

// TestPhase3_MissingAndMalformedHeaders verifies fail-closed security when headers are missing or malformed.
func TestPhase3_MissingAndMalformedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec_phase3_test"
	repo := newPhase3MockGatewayRepo()

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		GatewayPaymentRepo:  repo,
		WebhookToleranceSec: 300,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_1"},"payment":{"cf_payment_id":123,"payment_status":"SUCCESS","payment_amount":100}}}`
	validTS := fmt.Sprintf("%d", time.Now().Unix())
	validSig := signTestHMAC(secret, validTS+body)

	t.Run("Missing_Signature_Header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		req.Header.Set("x-webhook-timestamp", validTS)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for missing signature, got %d", w.Code)
		}
	})

	t.Run("Missing_Timestamp_Header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		req.Header.Set("x-webhook-signature", validSig)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for missing timestamp, got %d", w.Code)
		}
	})

	t.Run("Expired_Timestamp_Header", func(t *testing.T) {
		expiredTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		req.Header.Set("x-webhook-timestamp", expiredTS)
		req.Header.Set("x-webhook-signature", signTestHMAC(secret, expiredTS+body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for expired timestamp, got %d", w.Code)
		}
	})

	t.Run("Malformed_Timestamp_Header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		req.Header.Set("x-webhook-timestamp", "not_a_valid_timestamp")
		req.Header.Set("x-webhook-signature", signTestHMAC(secret, "not_a_valid_timestamp"+body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for malformed timestamp, got %d", w.Code)
		}
	})
}

// TestPhase3_DuplicateDeliveryWithIdempotencyHeader verifies that redelivery of a webhook
// carrying x-idempotency-header is deduplicated at the front door with zero duplicate mutations.
func TestPhase3_DuplicateDeliveryWithIdempotencyHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec_phase3_idemp"
	repo := newPhase3MockGatewayRepo()
	settler := &phase3MockOrderSettler{}

	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: "order_idemp_1",
		AmountPaise:     100000,
		Status:          domain.IntentCreated,
	}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		GatewayPaymentRepo:  repo,
		OrderSettler:        settler,
		IntentStore:         &phase3MockIntentStore{intent: intent},
		WebhookToleranceSec: 300,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"order_idemp_1","order_currency":"INR"},"payment":{"cf_payment_id":77711,"payment_status":"SUCCESS","payment_amount":1000.00,"payment_currency":"INR"}}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signTestHMAC(secret, ts+body)
	idempHeader := "evt_cashfree_pg_delivery_uuid_001"

	// 1. First delivery
	req1 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req1.Header.Set("x-webhook-timestamp", ts)
	req1.Header.Set("x-webhook-signature", sig)
	req1.Header.Set("x-idempotency-header", idempHeader)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on first delivery, got %d", w1.Code)
	}
	if settler.settleCalls != 1 {
		t.Fatalf("expected 1 settlement call after first delivery, got %d", settler.settleCalls)
	}

	// Verify idempotency record was persisted
	has, err := repo.HasProcessedEvent(context.Background(), "cashfree", "WEBHOOK_IDEMPOTENCY", idempHeader, "")
	if err != nil || !has {
		t.Fatalf("expected WEBHOOK_IDEMPOTENCY row recorded for %q, got has=%v, err=%v", idempHeader, has, err)
	}

	// 2. Duplicate delivery (at-least-once retry from gateway)
	req2 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req2.Header.Set("x-webhook-timestamp", ts)
	req2.Header.Set("x-webhook-signature", sig)
	req2.Header.Set("x-idempotency-header", idempHeader)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on duplicate delivery, got %d", w2.Code)
	}
	// Zero duplicate settlement calls!
	if settler.settleCalls != 1 {
		t.Fatalf("expected settlement calls to remain exactly 1 after duplicate delivery, got %d", settler.settleCalls)
	}
}

// TestPhase3_PayoutTransferCompletion_SentToBeneficiaryVsAcknowledged verifies that
// TRANSFER_SUCCESS with SENT_TO_BENEFICIARY is NOT treated as completed, while
// TRANSFER_ACKNOWLEDGED with COMPLETED/ACKNOWLEDGED is recognized as final settlement.
func TestPhase3_PayoutTransferCompletion_SentToBeneficiaryVsAcknowledged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec_payout_completion"
	itemID := uuid.New()
	repo := newPhase3MockGatewayRepo()

	var updatedStatus domain.PayoutItemStatus
	mockPayoutStore := &mockPayoutWebhookStore{
		getItemFunc: func(context.Context, uuid.UUID) (*domain.PayoutItem, error) {
			return &domain.PayoutItem{
				ID:          itemID,
				Status:      domain.PayoutProcessing,
				AmountPaise: 250000,
			}, nil
		},
		updateStatusFunc: func(_ context.Context, _ pgx.Tx, _ uuid.UUID, status domain.PayoutItemStatus, _, _ *string, _ *time.Time, _ *string) error {
			updatedStatus = status
			return nil
		},
	}

	h := &Handlers{Deps: Deps{
		CashfreePayoutWebhookSecret: secret,
		GatewayPaymentRepo:          repo,
		PayoutWebhookRepo:           mockPayoutStore,
		WebhookToleranceSec:         300,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

	t.Run("TRANSFER_SUCCESS_With_SENT_TO_BENEFICIARY_DoesNotCompletePayout", func(t *testing.T) {
		body := fmt.Sprintf(`{
			"event": "TRANSFER_SUCCESS",
			"event_time": "%s",
			"data": {
				"transfer_id": "pgo_%s",
				"cf_transfer_id": "cf_tr_12345",
				"status": "SUCCESS",
				"status_code": "SENT_TO_BENEFICIARY",
				"status_description": "Transfer is sent to beneficiary bank",
				"transfer_amount": 2500.00
			}
		}`, time.Now().Format(time.RFC3339), itemID)

		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signTestHMAC(secret, ts+body)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewBufferString(body))
		req.Header.Set("x-webhook-timestamp", ts)
		req.Header.Set("x-webhook-signature", sig)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK acknowledged to gateway, got %d", w.Code)
		}
		// Invariant: Status must NOT have been updated to PayoutSucceeded
		if updatedStatus == domain.PayoutSucceeded {
			t.Fatalf("CRITICAL VIOLATION: SENT_TO_BENEFICIARY marked payout item as Succeeded")
		}
	})

	t.Run("TRANSFER_ACKNOWLEDGED_With_COMPLETED_CompletesPayout", func(t *testing.T) {
		body := fmt.Sprintf(`{
			"event": "TRANSFER_ACKNOWLEDGED",
			"event_time": "%s",
			"data": {
				"transfer_id": "pgo_%s",
				"cf_transfer_id": "cf_tr_12345",
				"status": "SUCCESS",
				"status_code": "COMPLETED",
				"status_description": "Transfer completed successfully",
				"transfer_amount": 2500.00,
				"utr": "UTR998877665544",
				"acknowledged": 1
			}
		}`, time.Now().Format(time.RFC3339), itemID)

		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signTestHMAC(secret, ts+body)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewBufferString(body))
		req.Header.Set("x-webhook-timestamp", ts)
		req.Header.Set("x-webhook-signature", sig)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", w.Code)
		}
		if updatedStatus != domain.PayoutSucceeded {
			t.Fatalf("expected payout item updated to Succeeded, got %q", updatedStatus)
		}
	})
}

// TestPhase3_PaymentGateway_AmountAndCurrencyMismatchQuarantine verifies that payments
// with mismatched currency or mismatched amount are quarantined to unmatched receipts.
func TestPhase3_PaymentGateway_AmountAndCurrencyMismatchQuarantine(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec_mismatch_test"
	repo := newPhase3MockGatewayRepo()
	settler := &phase3MockOrderSettler{}

	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: "order_mismatch_1",
		AmountPaise:     50000, // 500.00 INR
		Status:          domain.IntentCreated,
	}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		GatewayPaymentRepo:  repo,
		OrderSettler:        settler,
		IntentStore:         &phase3MockIntentStore{intent: intent},
		WebhookToleranceSec: 300,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	t.Run("Non_INR_Currency_RejectedAndQuarantined", func(t *testing.T) {
		body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"order_mismatch_1","order_currency":"USD"},"payment":{"cf_payment_id":111,"payment_status":"SUCCESS","payment_amount":500.00,"payment_currency":"USD"}}}`
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signTestHMAC(secret, ts+body)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		req.Header.Set("x-webhook-timestamp", ts)
		req.Header.Set("x-webhook-signature", sig)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", w.Code)
		}
		// Zero settlement calls
		if settler.settleCalls != 0 {
			t.Fatalf("expected 0 settlement calls for USD currency anomaly, got %d", settler.settleCalls)
		}
	})

	t.Run("Amount_Mismatch_QuarantinedToUnmatchedReceipts", func(t *testing.T) {
		// Incoming amount: 400.00 (40000 paise), expected: 50000 paise
		body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"order_mismatch_1","order_currency":"INR"},"payment":{"cf_payment_id":222,"payment_status":"SUCCESS","payment_amount":400.00,"payment_currency":"INR"}}}`
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signTestHMAC(secret, ts+body)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		req.Header.Set("x-webhook-timestamp", ts)
		req.Header.Set("x-webhook-signature", sig)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", w.Code)
		}
		// Zero settlement calls
		if settler.settleCalls != 0 {
			t.Fatalf("expected 0 settlement calls on amount mismatch, got %d", settler.settleCalls)
		}
		// Exactly 1 unmatched receipt recorded
		if repo.unmatchedCount != 1 {
			t.Fatalf("expected 1 unmatched receipt recorded, got %d", repo.unmatchedCount)
		}
		if repo.unmatchedRows[0]["failure_reason"] != "amount_mismatch" {
			t.Fatalf("expected failure_reason 'amount_mismatch', got %v", repo.unmatchedRows[0]["failure_reason"])
		}
	})
}

// TestPhase3_ConcurrentDuplicateStampedeWithIdempotencyHeader verifies that when multiple
// concurrent deliveries with the identical x-idempotency-header arrive simultaneously,
// exactly one execution registers the event and duplicate state mutation is prevented.
func TestPhase3_ConcurrentDuplicateStampedeWithIdempotencyHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec_stampede"
	repo := newPhase3MockGatewayRepo()
	settler := &phase3MockOrderSettler{}

	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: "order_stampede_1",
		AmountPaise:     100000,
		Status:          domain.IntentCreated,
	}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		GatewayPaymentRepo:  repo,
		OrderSettler:        settler,
		IntentStore:         &phase3MockIntentStore{intent: intent},
		WebhookToleranceSec: 300,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"order_stampede_1","order_currency":"INR"},"payment":{"cf_payment_id":998811,"payment_status":"SUCCESS","payment_amount":1000.00,"payment_currency":"INR"}}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signTestHMAC(secret, ts+body)
	idempHeader := "concurrent_evt_idemp_" + uuid.New().String()

	const concurrency = 10
	var wg sync.WaitGroup
	var successCount int64

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
			req.Header.Set("x-webhook-timestamp", ts)
			req.Header.Set("x-webhook-signature", sig)
			req.Header.Set("x-idempotency-header", idempHeader)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code == http.StatusOK {
				atomic.AddInt64(&successCount, 1)
			}
		}()
	}
	wg.Wait()

	if successCount != concurrency {
		t.Fatalf("expected all %d requests to return 200 OK, got %d", concurrency, successCount)
	}
	// Exactly 1 settlement call occurred
	if settler.settleCalls != 1 {
		t.Fatalf("expected exactly 1 settlement call under concurrent stampede, got %d", settler.settleCalls)
	}
}

func TestCashfreeWebhook_Phase3_RetryAfterTransientFailureSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test_secret_for_phase3_retry_test"
	repo := newPhase3MockGatewayRepo()
	settler := &phase3MockOrderSettler{
		failErr: errors.New("transient database connection failure"),
	}

	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		ProviderOrderID: "order_transient_1",
		AmountPaise:     100000,
		Status:          domain.IntentCreated,
	}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:      secret,
		GatewayPaymentRepo:  repo,
		OrderSettler:        settler,
		IntentStore:         &phase3MockIntentStore{intent: intent},
		WebhookToleranceSec: 300,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"order_transient_1","order_currency":"INR"},"payment":{"cf_payment_id":998822,"payment_status":"SUCCESS","payment_amount":1000.00,"payment_currency":"INR"}}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signTestHMAC(secret, ts+body)
	idempHeader := "retry_evt_idemp_" + uuid.New().String()

	// First attempt: settler returns 500 error
	req1 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req1.Header.Set("x-webhook-timestamp", ts)
	req1.Header.Set("x-webhook-signature", sig)
	req1.Header.Set("x-idempotency-header", idempHeader)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	if w1.Code != http.StatusInternalServerError {
		t.Fatalf("expected initial attempt to fail with 500, got %d", w1.Code)
	}
	if settler.settleCalls != 1 {
		t.Fatalf("expected 1 settle call, got %d", settler.settleCalls)
	}

	// Fix transient failure
	settler.mu.Lock()
	settler.failErr = nil
	settler.mu.Unlock()

	// Second attempt: Cashfree retries with SAME idempotency header.
	// Since first attempt failed with 500, idempotency reservation must have been rolled back.
	req2 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req2.Header.Set("x-webhook-timestamp", ts)
	req2.Header.Set("x-webhook-signature", sig)
	req2.Header.Set("x-idempotency-header", idempHeader)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected retry after transient failure to return 200 OK, got %d", w2.Code)
	}
	if settler.settleCalls != 2 {
		t.Fatalf("expected retry to invoke settler (total 2 calls), got %d", settler.settleCalls)
	}
	if settler.settledOrder != "order_transient_1" {
		t.Fatalf("expected settled order 'order_transient_1', got %q", settler.settledOrder)
	}

	// Third attempt: Duplicate delivery of the already-settled event with same idempotency header.
	// This must be suppressed as duplicate and return 200 OK without calling settler again.
	req3 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req3.Header.Set("x-webhook-timestamp", ts)
	req3.Header.Set("x-webhook-signature", sig)
	req3.Header.Set("x-idempotency-header", idempHeader)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("expected duplicate delivery to return 200 OK, got %d", w3.Code)
	}
	if settler.settleCalls != 2 {
		t.Fatalf("expected settler calls to remain 2, got %d", settler.settleCalls)
	}
}
