package payment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/pg-cashflow/pg-go/internal/cashfree"
)

func TestCashfreeAdapter_ParseWebhook_PaymentSuccess(t *testing.T) {
	cfg := cashfree.Config{
		AppID:     "test-app",
		SecretKey: "test-secret",
	}
	adapter := NewCashfreeAdapter(cfg)

	raw := []byte(`{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"event_time": "2026-09-24T18:00:00Z",
		"data": {
			"order": {
				"order_id": "pg-DEC24-001-1727190000",
				"order_amount": 5500.00,
				"order_currency": "INR"
			},
			"payment": {
				"cf_payment_id": 99887766,
				"payment_status": "SUCCESS",
				"payment_amount": 5500.00,
				"payment_currency": "INR",
				"bank_reference": "UTR123456",
				"payment_time": "2026-09-24T18:00:00Z"
			}
		}
	}`)

	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(cfg.SecretKey))
	mac.Write([]byte(ts + string(raw)))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	headers := http.Header{}
	headers.Set("x-webhook-timestamp", ts)
	headers.Set("x-webhook-signature", sig)

	if err := adapter.VerifyWebhook(headers, raw); err != nil {
		t.Fatalf("expected valid signature, got: %v", err)
	}

	event, err := adapter.ParseWebhook(headers, raw)
	if err != nil {
		t.Fatalf("failed to parse webhook: %v", err)
	}

	if event.Provider != ProviderCashfree {
		t.Errorf("expected provider cashfree, got %s", event.Provider)
	}
	if event.EventType != EventTypePaymentSuccess {
		t.Errorf("expected EventTypePaymentSuccess, got %s", event.EventType)
	}
	if event.EventStatus != "SUCCESS" {
		t.Errorf("expected SUCCESS, got %s", event.EventStatus)
	}
	if event.AmountPaise != 550000 {
		t.Errorf("expected 550000 paise, got %d", event.AmountPaise)
	}
	if event.ProviderReferenceID != "99887766" {
		t.Errorf("expected 99887766, got %s", event.ProviderReferenceID)
	}
	if event.OrderID != "pg-DEC24-001-1727190000" {
		t.Errorf("expected order ID pg-DEC24-001-1727190000, got %s", event.OrderID)
	}
}

func TestCashfreeAdapter_ParseWebhook_AutoRefund(t *testing.T) {
	cfg := cashfree.Config{
		AppID:     "test-app",
		SecretKey: "test-secret",
	}
	adapter := NewCashfreeAdapter(cfg)

	raw := []byte(`{
		"type": "AUTO_REFUND_STATUS_WEBHOOK",
		"data": {
			"auto_refund": {
				"cf_refund_id": 88776655,
				"refund_id": "auto_ref_123",
				"order_id": "pg-DEC24-001-1727190000",
				"cf_payment_id": 99887766,
				"refund_status": "SUCCESS",
				"refund_amount": 5500.00,
				"refund_type": "PAYMENT_AUTO_REFUND",
				"refund_reason": "Multiple payments were performed against same order"
			}
		}
	}`)

	headers := http.Header{}
	event, err := adapter.ParseWebhook(headers, raw)
	if err != nil {
		t.Fatalf("failed to parse auto-refund webhook: %v", err)
	}

	if event.EventType != EventTypeAutoRefund {
		t.Errorf("expected EventTypeAutoRefund, got %s", event.EventType)
	}
	if event.EventStatus != "SUCCESS" {
		t.Errorf("expected SUCCESS, got %s", event.EventStatus)
	}
	if event.RefundID != "88776655" {
		t.Errorf("expected 88776655, got %s", event.RefundID)
	}
	if event.AmountPaise != 550000 {
		t.Errorf("expected 550000 paise, got %d", event.AmountPaise)
	}
}
