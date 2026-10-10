package cashfree_test

import (
	"testing"

	"github.com/pg-cashflow/pg-go/internal/cashfree"
)

// FuzzParseWebhook validates Gate 10: native Go fuzzing for untrusted Cashfree webhook deserialization.
// Invariant: ParseWebhook must never panic on arbitrary bytes and must return
// a typed struct or a descriptive error without crashing.
func FuzzParseWebhook(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_1"},"payment":{"cf_payment_id":"123","payment_amount":1500.00,"bank_reference":"UTR123"}}}`),
		[]byte(`{"type":"PAYMENT_FAILED_WEBHOOK","data":{"order":{"order_id":"ord_2"},"payment":{"cf_payment_id":"124","payment_amount":1500.00,"payment_status":"FAILED"}}}`),
		[]byte(`{"type":"AUTO_REFUND_STATUS_WEBHOOK","data":{"auto_refund":{"cf_refund_id":"ref_1","order_id":"ord_1","refund_status":"SUCCESS","refund_amount":500.00}}}`),
		[]byte(`{"type":"DISPUTE_CREATED_WEBHOOK","data":{"dispute_id":"disp_1","order_id":"ord_1","dispute_amount":1500.00,"dispute_status":"OPEN"}}}`),
		[]byte(`{}`),
		[]byte(`""`),
		[]byte(`null`),
		[]byte(`{"type":"UNKNOWN_FUTURE_EVENT","data":{}}`),
		[]byte(`{"type":12345}`),
		[]byte(`<xml>not json</xml>`),
		[]byte("\x00\x01\x02\xff\xfe\xfd"),
	}

	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		parsed, evtType, err := cashfree.ParseWebhook(payload)
		if err == nil && parsed != nil {
			if evtType == "" {
				t.Fatalf("expected non-empty event type when parsed is non-nil")
			}
		}
	})
}
