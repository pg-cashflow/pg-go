package cashfree

import (
	"encoding/json"
	"testing"
)

// TestProviderID_String_AlphanumericRefundID is the primary regression test for
// the Cashfree integration defect: cf_refund_id is documented as an alphanumeric
// string (e.g. "refund_40564529"), but was previously decoded as json.Number,
// which silently coerces non-numeric strings to empty or errors out.
func TestProviderID_String_AlphanumericRefundID(t *testing.T) {
	t.Run("string_alphanumeric", func(t *testing.T) {
		// Cashfree's published example: cf_refund_id: "refund_40564529"
		// https://github.com/cashfree/cashfree-pg/blob/main/docs/Refunds.md
		payload := `{"cf_refund_id":"refund_40564529"}`
		var v struct {
			CFRefundID ProviderID `json:"cf_refund_id"`
		}
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatalf("json.Number would fail here; ProviderID must succeed: %v", err)
		}
		if v.CFRefundID.String() != "refund_40564529" {
			t.Fatalf("got %q want %q", v.CFRefundID.String(), "refund_40564529")
		}
	})

	t.Run("string_numeric_as_string", func(t *testing.T) {
		// Some responses send numeric IDs as JSON strings "12345"
		payload := `{"cf_refund_id":"12345"}`
		var v struct {
			CFRefundID ProviderID `json:"cf_refund_id"`
		}
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatalf("string-encoded number must decode: %v", err)
		}
		if v.CFRefundID.String() != "12345" {
			t.Fatalf("got %q", v.CFRefundID.String())
		}
	})

	t.Run("bare_number", func(t *testing.T) {
		// Legacy Cashfree responses send cf_payment_id as a bare number
		payload := `{"cf_payment_id":999888777}`
		var v struct {
			CFPaymentID ProviderID `json:"cf_payment_id"`
		}
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatalf("bare number must decode: %v", err)
		}
		if v.CFPaymentID.String() != "999888777" {
			t.Fatalf("got %q", v.CFPaymentID.String())
		}
	})

	t.Run("null", func(t *testing.T) {
		payload := `{"cf_refund_id":null}`
		var v struct {
			CFRefundID ProviderID `json:"cf_refund_id"`
		}
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatalf("null must decode as empty: %v", err)
		}
		if v.CFRefundID.String() != "" {
			t.Fatalf("null must yield empty string, got %q", v.CFRefundID.String())
		}
	})

	t.Run("empty_string", func(t *testing.T) {
		payload := `{"cf_refund_id":""}`
		var v struct {
			CFRefundID ProviderID `json:"cf_refund_id"`
		}
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatalf("empty string must decode: %v", err)
		}
		if v.CFRefundID.String() != "" {
			t.Fatalf("empty string must yield empty, got %q", v.CFRefundID.String())
		}
	})

	t.Run("roundtrip_marshal", func(t *testing.T) {
		id := ProviderID("refund_40564529")
		b, err := json.Marshal(id)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(b) != `"refund_40564529"` {
			t.Fatalf("marshal produced %s", b)
		}
	})

	t.Run("refund_webhook_alphanumeric_cf_refund_id", func(t *testing.T) {
		// Simulate a real REFUND_STATUS_WEBHOOK payload with the documented
		// alphanumeric cf_refund_id. Before the fix this caused a JSON decode
		// error because the struct used json.Number.
		raw := []byte(`{
			"type": "REFUND_STATUS_WEBHOOK",
			"data": {
				"refund": {
					"cf_refund_id": "refund_40564529",
					"refund_id": "ref-merchant-001",
					"order_id": "pg-ORDER-XYZ",
					"cf_payment_id": 998877,
					"refund_status": "SUCCESS",
					"refund_amount": "500.00",
					"refund_type": "MERCHANT_INITIATED",
					"refund_reason": "tenant request"
				}
			}
		}`)
		val, evtType, err := ParseWebhook(raw)
		if err != nil {
			t.Fatalf("ParseWebhook with alphanumeric cf_refund_id must succeed, got: %v", err)
		}
		if evtType != "REFUND_STATUS_WEBHOOK" {
			t.Fatalf("wrong event type: %s", evtType)
		}
		rw, ok := val.(RefundWebhook)
		if !ok {
			t.Fatalf("expected RefundWebhook, got %T", val)
		}
		if rw.CFRefundID != "refund_40564529" {
			t.Fatalf("CFRefundID: got %q want %q", rw.CFRefundID, "refund_40564529")
		}
		if rw.RefundID != "ref-merchant-001" {
			t.Fatalf("RefundID: got %q", rw.RefundID)
		}
		if rw.RefundAmount != 50000 {
			t.Fatalf("RefundAmount paise: got %d want 50000", rw.RefundAmount)
		}
		if rw.RefundStatus != "SUCCESS" {
			t.Fatalf("RefundStatus: got %q", rw.RefundStatus)
		}
	})

	t.Run("auto_refund_webhook_alphanumeric_cf_refund_id", func(t *testing.T) {
		raw := []byte(`{
			"type": "AUTO_REFUND_STATUS_WEBHOOK",
			"data": {
				"auto_refund": {
					"cf_refund_id": "refund_99887766",
					"refund_id": "auto-ref-002",
					"order_id": "pg-ORDER-AUTO",
					"cf_payment_id": "778899",
					"refund_status": "PENDING",
					"refund_amount": "1500.50",
					"refund_type": "AUTO",
					"refund_reason": "auto refund"
				}
			}
		}`)
		val, evtType, err := ParseWebhook(raw)
		if err != nil {
			t.Fatalf("ParseWebhook auto_refund with alphanumeric cf_refund_id: %v", err)
		}
		if evtType != "AUTO_REFUND_STATUS_WEBHOOK" {
			t.Fatalf("wrong event type: %s", evtType)
		}
		rw, ok := val.(RefundWebhook)
		if !ok {
			t.Fatalf("expected RefundWebhook, got %T", val)
		}
		if rw.CFRefundID != "refund_99887766" {
			t.Fatalf("CFRefundID: got %q want %q", rw.CFRefundID, "refund_99887766")
		}
		if !rw.IsAutoRefund {
			t.Fatal("IsAutoRefund must be true")
		}
		if rw.RefundAmount != 150050 {
			t.Fatalf("RefundAmount paise: got %d want 150050", rw.RefundAmount)
		}
	})
}
