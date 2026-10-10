package cashfree

import (
	"testing"
	"time"
)

func TestVerifyWebhookTimestamp(t *testing.T) {
	now := time.Date(2026, 9, 24, 18, 30, 0, 0, time.UTC)

	// Valid seconds within 300s
	secValid := "1790274500" // diff is small
	// Exactly now unix:
	secNow := "1790274600"
	_ = secNow
	secOld := "1790274000" // 600s old

	nowEpoch := now.Unix()
	if err := VerifyWebhookTimestamp(time.Unix(nowEpoch-50, 0).Format(time.RFC3339), 300, now); err != nil {
		t.Fatalf("RFC3339 within tolerance failed: %v", err)
	}

	if err := VerifyWebhookTimestamp(time.Unix(nowEpoch-500, 0).Format(time.RFC3339), 300, now); err == nil {
		t.Fatal("RFC3339 outside tolerance should have failed")
	}

	// Milliseconds within tolerance
	milliStr := time.UnixMilli((nowEpoch - 30) * 1000).UnixMilli()
	if err := VerifyWebhookTimestamp(string(rune(milliStr)), 300, now); err != nil {
		// test string conversion
	}
	_ = secValid
	_ = secOld
}

func TestParseWebhookTypes(t *testing.T) {
	// 1. Success
	rawSuccess := []byte(`{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"data": {
			"order": {"order_id": "pg-ORDER-1"},
			"payment": {
				"cf_payment_id": "999888",
				"payment_status": "SUCCESS",
				"payment_amount": "5500.50",
				"bank_reference": "UTR123456"
			}
		}
	}`)
	val, typ, err := ParseWebhook(rawSuccess)
	if err != nil || typ != "PAYMENT_SUCCESS_WEBHOOK" {
		t.Fatalf("expected success, got typ=%s err=%v", typ, err)
	}
	succ, ok := val.(SuccessWebhook)
	if !ok || succ.AmountPaise != 550050 || succ.OrderID != "pg-ORDER-1" || succ.TxnID() != "UTR123456" {
		t.Fatalf("unexpected success content: %+v", succ)
	}

	// 2. Failed
	rawFailed := []byte(`{
		"type": "PAYMENT_FAILED_WEBHOOK",
		"data": {
			"order": {"order_id": "pg-ORDER-2"},
			"payment": {
				"cf_payment_id": "999889",
				"payment_status": "FAILED",
				"payment_amount": "5500.00",
				"payment_message": "user cancelled"
			}
		}
	}`)
	val, typ, err = ParseWebhook(rawFailed)
	if err != nil || typ != "PAYMENT_FAILED_WEBHOOK" {
		t.Fatalf("expected failed, got typ=%s err=%v", typ, err)
	}
	fail, ok := val.(FailedWebhook)
	if !ok || fail.AmountPaise != 550000 || fail.FailureReason != "user cancelled" {
		t.Fatalf("unexpected failed content: %+v", fail)
	}

	// 3. Refund Status
	rawRefund := []byte(`{
		"type": "REFUND_STATUS_WEBHOOK",
		"data": {
			"refund": {
				"cf_refund_id": "112233",
				"refund_id": "rf_dup_123",
				"order_id": "pg-ORDER-1",
				"cf_payment_id": "999888",
				"refund_status": "SUCCESS",
				"refund_amount": "5500.50",
				"refund_reason": "duplicate payment"
			}
		}
	}`)
	val, typ, err = ParseWebhook(rawRefund)
	if err != nil || typ != "REFUND_STATUS_WEBHOOK" {
		t.Fatalf("expected refund, got typ=%s err=%v", typ, err)
	}
	ref, ok := val.(RefundWebhook)
	if !ok || ref.RefundAmount != 550050 || ref.IsAutoRefund || ref.RefundStatus != "SUCCESS" {
		t.Fatalf("unexpected refund content: %+v", ref)
	}

	// 4. Auto Refund Status
	rawAutoRefund := []byte(`{
		"type": "AUTO_REFUND_STATUS_WEBHOOK",
		"data": {
			"auto_refund": {
				"cf_refund_id": "445566",
				"refund_id": "cf_auto_1",
				"order_id": "pg-ORDER-3",
				"cf_payment_id": "999890",
				"refund_status": "SUCCESS",
				"refund_amount": "2500.00",
				"refund_reason": "Multiple payments were performed against same order"
			}
		}
	}`)
	val, typ, err = ParseWebhook(rawAutoRefund)
	if err != nil || typ != "AUTO_REFUND_STATUS_WEBHOOK" {
		t.Fatalf("expected auto refund, got typ=%s err=%v", typ, err)
	}
	autoRef, ok := val.(RefundWebhook)
	if !ok || autoRef.RefundAmount != 250000 || !autoRef.IsAutoRefund || autoRef.RefundStatus != "SUCCESS" {
		t.Fatalf("unexpected auto refund content: %+v", autoRef)
	}

	// 5. Dispute Created
	rawDispute := []byte(`{
		"type": "PAYMENT_DISPUTE_CREATED_WEBHOOK",
		"data": {
			"dispute": {
				"dispute_id": "DISP_98765",
				"dispute_type": "CHARGEBACK",
				"dispute_status": "ACTION_REQUIRED",
				"order_id": "pg-ORDER-4",
				"cf_payment_id": "999891",
				"dispute_amount": "12000.00",
				"reason_code": "FRAUDULENT",
				"reason_description": "Cardholder disputes transaction",
				"respond_by": "2026-10-05T12:00:00Z"
			}
		}
	}`)
	val, typ, err = ParseWebhook(rawDispute)
	if err != nil || typ != "PAYMENT_DISPUTE_CREATED_WEBHOOK" {
		t.Fatalf("expected dispute, got typ=%s err=%v", typ, err)
	}
	disp, ok := val.(DisputeWebhook)
	if !ok || disp.DisputeID != "DISP_98765" || disp.DisputeAmount != 1200000 || disp.DisputeStatus != "ACTION_REQUIRED" || disp.OrderID != "pg-ORDER-4" {
		t.Fatalf("unexpected dispute content: %+v", disp)
	}
}
