package cashfree

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCashfreeContract_2025_vs_2026_Webhooks validates Cashfree 2025-01-01 and 2026-01-01
// webhook schema payloads, verifying correct polymorphic extraction and paise coercion.
func TestCashfreeContract_2025_vs_2026_Webhooks(t *testing.T) {
	// 1. PAYMENT_SUCCESS_WEBHOOK (2025-01-01 flat amount string vs 2026-01-01 nested float)
	t.Run("PAYMENT_SUCCESS_WEBHOOK 2025-01-01 format", func(t *testing.T) {
		raw := []byte(`{
			"type": "PAYMENT_SUCCESS_WEBHOOK",
			"event_time": "2025-01-15T10:00:00+05:30",
			"data": {
				"order": {
					"order_id": "pg-ORDER-2025-1",
					"order_amount": "5000.00",
					"order_currency": "INR"
				},
				"payment": {
					"cf_payment_id": "12345678",
					"payment_status": "SUCCESS",
					"payment_amount": "5000.00",
					"payment_currency": "INR",
					"payment_time": "2025-01-15T10:00:00+05:30",
					"bank_reference": "UTR20250115001"
				}
			}
		}`)
		payload, typ, err := ParseWebhook(raw)
		if err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if typ != "PAYMENT_SUCCESS_WEBHOOK" {
			t.Fatalf("expected PAYMENT_SUCCESS_WEBHOOK, got %s", typ)
		}
		succ, ok := payload.(SuccessWebhook)
		if !ok {
			t.Fatalf("expected SuccessWebhook type, got %T", payload)
		}
		if succ.OrderID != "pg-ORDER-2025-1" {
			t.Errorf("expected order pg-ORDER-2025-1, got %s", succ.OrderID)
		}
		if succ.CFPaymentID != "12345678" {
			t.Errorf("expected payment 12345678, got %s", succ.CFPaymentID)
		}
		if succ.AmountPaise != 500000 {
			t.Errorf("expected 500000 paise, got %d", succ.AmountPaise)
		}
		if succ.TxnID() != "UTR20250115001" {
			t.Errorf("expected UTR UTR20250115001, got %s", succ.TxnID())
		}
	})

	t.Run("PAYMENT_SUCCESS_WEBHOOK 2026-01-01 numeric amount format", func(t *testing.T) {
		raw := []byte(`{
			"type": "PAYMENT_SUCCESS_WEBHOOK",
			"event_time": "2026-01-01T12:00:00+05:30",
			"data": {
				"order": {
					"order_id": "pg-ORDER-2026-1",
					"order_amount": 7500.50,
					"order_currency": "INR"
				},
				"payment": {
					"cf_payment_id": "99887766",
					"payment_status": "SUCCESS",
					"payment_amount": 7500.50,
					"payment_currency": "INR",
					"bank_reference": "UTR20260101999"
				}
			}
		}`)
		payload, typ, err := ParseWebhook(raw)
		if err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if typ != "PAYMENT_SUCCESS_WEBHOOK" {
			t.Fatalf("expected PAYMENT_SUCCESS_WEBHOOK, got %s", typ)
		}
		succ, ok := payload.(SuccessWebhook)
		if !ok {
			t.Fatalf("expected SuccessWebhook type, got %T", payload)
		}
		if succ.AmountPaise != 750050 {
			t.Errorf("expected 750050 paise, got %d", succ.AmountPaise)
		}
		if succ.TxnID() != "UTR20260101999" {
			t.Errorf("expected UTR UTR20260101999, got %s", succ.TxnID())
		}
	})

	// 2. REFUND_STATUS_WEBHOOK
	t.Run("REFUND_STATUS_WEBHOOK 2025/2026 standard refund", func(t *testing.T) {
		raw := []byte(`{
			"type": "REFUND_STATUS_WEBHOOK",
			"event_time": "2026-01-10T15:30:00+05:30",
			"data": {
				"refund": {
					"cf_refund_id": "98765432",
					"refund_id": "rf_unapplied_001",
					"order_id": "pg-ORDER-REF-1",
					"cf_payment_id": "11223344",
					"refund_status": "SUCCESS",
					"refund_amount": "3000.00",
					"refund_currency": "INR",
					"refund_reason": "owner resolving held receipt"
				}
			}
		}`)
		payload, typ, err := ParseWebhook(raw)
		if err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if typ != "REFUND_STATUS_WEBHOOK" {
			t.Fatalf("expected REFUND_STATUS_WEBHOOK, got %s", typ)
		}
		ref, ok := payload.(RefundWebhook)
		if !ok {
			t.Fatalf("expected RefundWebhook type, got %T", payload)
		}
		if ref.RefundID != "rf_unapplied_001" {
			t.Errorf("expected refund_id rf_unapplied_001, got %s", ref.RefundID)
		}
		if ref.RefundAmount != 300000 {
			t.Errorf("expected 300000 paise, got %d", ref.RefundAmount)
		}
		if ref.RefundStatus != "SUCCESS" {
			t.Errorf("expected status SUCCESS, got %s", ref.RefundStatus)
		}
		if ref.IsAutoRefund {
			t.Errorf("expected isAutoRefund false, got true")
		}
	})

	// 3. AUTO_REFUND_STATUS_WEBHOOK
	t.Run("AUTO_REFUND_STATUS_WEBHOOK gateway initiated refund", func(t *testing.T) {
		raw := []byte(`{
			"type": "AUTO_REFUND_STATUS_WEBHOOK",
			"event_time": "2026-01-11T09:00:00+05:30",
			"data": {
				"auto_refund": {
					"cf_refund_id": "55667788",
					"refund_id": "cf_auto_unapplied",
					"order_id": "pg-ORDER-AUTO-1",
					"cf_payment_id": "99881122",
					"refund_status": "SUCCESS",
					"refund_amount": 1500.00,
					"refund_reason": "payment after order closed"
				}
			}
		}`)
		payload, typ, err := ParseWebhook(raw)
		if err != nil {
			t.Fatalf("parse failed: %v", err)
		}
		if typ != "AUTO_REFUND_STATUS_WEBHOOK" {
			t.Fatalf("expected AUTO_REFUND_STATUS_WEBHOOK, got %s", typ)
		}
		ref, ok := payload.(RefundWebhook)
		if !ok {
			t.Fatalf("expected RefundWebhook type, got %T", payload)
		}
		if !ref.IsAutoRefund {
			t.Errorf("expected isAutoRefund true, got false")
		}
		if ref.RefundAmount != 150000 {
			t.Errorf("expected 150000 paise, got %d", ref.RefundAmount)
		}
	})
}

// TestCashfreeContract_RefundClient_2025_and_2026 validates client refund requests and responses.
func TestCashfreeContract_RefundClient_2025_and_2026(t *testing.T) {
	// Subtest 1: 2025-01-01 API version header and single-object response
	t.Run("CreateRefund 2025-01-01 version contract", func(t *testing.T) {
		orderID := "order_contract_2025"
		refundID := "rf_test_2025"
		amountPaise := int64(250000)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Fatalf("expected POST, got %s", r.Method)
			}
			if r.URL.Path != "/orders/"+orderID+"/refunds" {
				t.Fatalf("expected /orders/%s/refunds, got %s", orderID, r.URL.Path)
			}
			if r.Header.Get("x-api-version") != "2025-01-01" {
				t.Errorf("expected x-api-version 2025-01-01, got %s", r.Header.Get("x-api-version"))
			}
			if r.Header.Get("x-idempotency-key") != "idem_test_2025" {
				t.Errorf("expected x-idempotency-key idem_test_2025, got %s", r.Header.Get("x-idempotency-key"))
			}

			var reqBody CreateRefundRequest
			if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
				t.Fatalf("decode req body failed: %v", err)
			}
			if reqBody.RefundAmount.String() != "2500.00" {
				t.Errorf("expected 2500.00, got %s", reqBody.RefundAmount.String())
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"cf_refund_id": "123456789",
				"refund_id": "` + refundID + `",
				"order_id": "` + orderID + `",
				"cf_payment_id": "987654321",
				"refund_status": "SUCCESS",
				"refund_amount": "2500.00",
				"refund_type": "STANDARD",
				"refund_reason": "resolving unapplied receipt"
			}`))
		}))
		defer server.Close()

		client := NewClient(Config{
			AppID:      "test_app",
			SecretKey:  "test_secret",
			APIVersion: "2025-01-01",
		}).WithBaseURL(server.URL)

		details, err := client.CreateRefund(context.Background(), orderID, refundID, amountPaise, "resolving unapplied receipt", "idem_test_2025")
		if err != nil {
			t.Fatalf("CreateRefund failed: %v", err)
		}
		if details.CFRefundID != "123456789" {
			t.Errorf("expected cf_refund_id 123456789, got %s", details.CFRefundID)
		}
		if details.AmountPaise != 250000 {
			t.Errorf("expected 250000 paise, got %d", details.AmountPaise)
		}
		if details.RefundStatus != "SUCCESS" {
			t.Errorf("expected SUCCESS status, got %s", details.RefundStatus)
		}
	})

	// Subtest 2: 2026-01-01 API version header and array response
	t.Run("CreateRefund 2026-01-01 version contract with array response", func(t *testing.T) {
		orderID := "order_contract_2026"
		refundID := "rf_test_2026"
		amountPaise := int64(100000)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("x-api-version") != "2026-01-01" {
				t.Errorf("expected x-api-version 2026-01-01, got %s", r.Header.Get("x-api-version"))
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[
				{
					"cf_refund_id": 123456789,
					"refund_id": "` + refundID + `",
					"order_id": "` + orderID + `",
					"cf_payment_id": 987654321,
					"refund_status": "SUCCESS",
					"refund_amount": 1000.00,
					"refund_type": "STANDARD",
					"refund_reason": "owner refunded unapplied"
				}
			]`))
		}))
		defer server.Close()

		client := NewClient(Config{
			AppID:      "test_app",
			SecretKey:  "test_secret",
			APIVersion: "2026-01-01",
		}).WithBaseURL(server.URL)

		details, err := client.CreateRefund(context.Background(), orderID, refundID, amountPaise, "owner refunded unapplied", "idem_test_2026")
		if err != nil {
			t.Fatalf("CreateRefund failed: %v", err)
		}
		if details.CFRefundID != "123456789" {
			t.Errorf("expected cf_refund_id 123456789, got %s", details.CFRefundID)
		}
		if details.CFPaymentID != "987654321" {
			t.Errorf("expected cf_payment_id 987654321, got %s", details.CFPaymentID)
		}
		if details.AmountPaise != 100000 {
			t.Errorf("expected 100000 paise, got %d", details.AmountPaise)
		}
	})
}
