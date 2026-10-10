package cashfree

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseOrderSettlementsJSON_Flat2025(t *testing.T) {
	raw := `[
		{
			"cf_payment_id": "6183934088",
			"cf_settlement_id": "312048765",
			"settlement_currency": "INR",
			"order_id": "order_due_12345",
			"order_amount": 5500.00,
			"settlement_amount": 5393.80,
			"payment_time": "2025-01-15T10:24:37+05:30",
			"service_charge": 90.00,
			"service_tax": 16.20,
			"adjustment": 0.00,
			"settlement_id": "312048765",
			"transfer_id": "CB0312048765",
			"transfer_time": "2025-01-16T09:15:22+05:30",
			"status": "SUCCESS"
		}
	]`

	recs, err := ParseOrderSettlementsJSON([]byte(raw), "order_fallback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.CFSettlementID != "312048765" {
		t.Errorf("expected settlement id 312048765, got %s", r.CFSettlementID)
	}
	if r.CFPaymentID != "6183934088" {
		t.Errorf("expected payment id 6183934088, got %s", r.CFPaymentID)
	}
	if r.OrderID != "order_due_12345" {
		t.Errorf("expected order_id order_due_12345, got %s", r.OrderID)
	}
	if r.GrossAmountPaise != 550000 {
		t.Errorf("expected gross 550000 paise, got %d", r.GrossAmountPaise)
	}
	if r.NetAmountPaise != 539380 {
		t.Errorf("expected net 539380 paise, got %d", r.NetAmountPaise)
	}
	if r.ServiceChargePaise != 9000 {
		t.Errorf("expected fee 9000 paise, got %d", r.ServiceChargePaise)
	}
	if r.ServiceTaxPaise != 1620 {
		t.Errorf("expected tax 1620 paise, got %d", r.ServiceTaxPaise)
	}
	if r.AdjustmentPaise != 0 {
		t.Errorf("expected adj 0, got %d", r.AdjustmentPaise)
	}
	if r.UTR != "CB0312048765" {
		t.Errorf("expected utr CB0312048765, got %s", r.UTR)
	}
	if r.Status != "SUCCESS" {
		t.Errorf("expected status SUCCESS, got %s", r.Status)
	}
	if r.TransferTime.IsZero() {
		t.Errorf("expected transfer time parsed, got zero")
	}
}

func TestParseOrderSettlementsJSON_Nested2026(t *testing.T) {
	raw := `{
		"order_details": {
			"order_amount": 1000.00,
			"order_currency": "INR",
			"order_id": "order_nested_99"
		},
		"payment_details": {
			"cf_payment_id": 99887766,
			"payment_amount": 1000.00,
			"service_charge": 20.00,
			"service_tax": 3.60,
			"adjustment": 50.00,
			"settlement_amount": 926.40
		},
		"settlement_details": {
			"cf_settlement_id": 445566,
			"status": "SUCCESS",
			"settlement_utr": 1122334455,
			"settlement_processed_on": "2026-02-01T10:00:00+05:30"
		}
	}`

	recs, err := ParseOrderSettlementsJSON([]byte(raw), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.OrderID != "order_nested_99" {
		t.Errorf("expected order_id order_nested_99, got %s", r.OrderID)
	}
	if r.CFPaymentID != "99887766" {
		t.Errorf("expected cf_payment_id 99887766, got %s", r.CFPaymentID)
	}
	if r.CFSettlementID != "445566" {
		t.Errorf("expected cf_settlement_id 445566, got %s", r.CFSettlementID)
	}
	if r.GrossAmountPaise != 100000 {
		t.Errorf("expected gross 100000 paise, got %d", r.GrossAmountPaise)
	}
	if r.NetAmountPaise != 92640 {
		t.Errorf("expected net 92640 paise, got %d", r.NetAmountPaise)
	}
	if r.ServiceChargePaise != 2000 {
		t.Errorf("expected fee 2000 paise, got %d", r.ServiceChargePaise)
	}
	if r.ServiceTaxPaise != 360 {
		t.Errorf("expected tax 360 paise, got %d", r.ServiceTaxPaise)
	}
	if r.AdjustmentPaise != 5000 {
		t.Errorf("expected adj 5000 paise, got %d", r.AdjustmentPaise)
	}
	if r.UTR != "1122334455" {
		t.Errorf("expected utr 1122334455, got %s", r.UTR)
	}
}

func TestParseSettlementWebhook(t *testing.T) {
	raw := `{
		"data": {
			"settlement": {
				"settlement_id": 738,
				"status": "SUCCESS",
				"amount_settled": 97.94,
				"utr": 1644822317781212,
				"settled_on": "2025-02-14T12:35:19+05:30",
				"settlement_type": "STANDARD",
				"payment_amount": 100.00,
				"service_charge": 1.75,
				"service_tax": 0.31,
				"adjustment": 0.00,
				"settlement_initiated_on": "2025-02-14T12:35:17+05:30"
			}
		},
		"event_time": "2025-02-14T12:35:20+05:30",
		"type": "SETTLEMENT_SUCCESS"
	}`

	rec, err := ParseSettlementWebhook([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.CFSettlementID != "738" {
		t.Errorf("expected settlement id '738', got %s", rec.CFSettlementID)
	}
	if rec.UTR != "1644822317781212" {
		t.Errorf("expected UTR '1644822317781212', got %s", rec.UTR)
	}
	if rec.GrossAmountPaise != 10000 {
		t.Errorf("expected gross 10000, got %d", rec.GrossAmountPaise)
	}
	if rec.NetAmountPaise != 9794 {
		t.Errorf("expected net 9794, got %d", rec.NetAmountPaise)
	}
	if rec.ServiceChargePaise != 175 {
		t.Errorf("expected fee 175, got %d", rec.ServiceChargePaise)
	}
	if rec.ServiceTaxPaise != 31 {
		t.Errorf("expected tax 31, got %d", rec.ServiceTaxPaise)
	}
	if rec.AdjustmentPaise != 0 {
		t.Errorf("expected adj 0, got %d", rec.AdjustmentPaise)
	}
	if rec.SettledOn == nil {
		t.Errorf("expected SettledOn not nil")
	}
	if rec.SettlementInitiatedOn == nil {
		t.Errorf("expected SettlementInitiatedOn not nil")
	}
}

func TestClient_GetOrderSettlements_HTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-client-id") != "app_test" || r.Header.Get("x-client-secret") != "sec_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-api-version") != "2025-01-01" {
			http.Error(w, "bad version", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{
				"cf_payment_id": "111",
				"cf_settlement_id": "222",
				"order_id": "order_test",
				"order_amount": 500.00,
				"settlement_amount": 490.00,
				"service_charge": 8.47,
				"service_tax": 1.53,
				"adjustment": 0,
				"transfer_id": "UTR_TEST",
				"transfer_time": "2025-01-01T12:00:00Z",
				"status": "SUCCESS"
			}
		]`))
	}))
	defer ts.Close()

	client := NewClient(Config{
		AppID:      "app_test",
		SecretKey:  "sec_test",
		APIVersion: "2025-01-01",
	})
	client.baseOverride = ts.URL

	recs, err := client.GetOrderSettlements(context.Background(), "order_test")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if recs[0].GrossAmountPaise != 50000 || recs[0].NetAmountPaise != 49000 {
		t.Errorf("unexpected amounts: gross %d, net %d", recs[0].GrossAmountPaise, recs[0].NetAmountPaise)
	}
}

func TestCoercePaise_FailClosed(t *testing.T) {
	// Valid cases
	val, err := coercePaise("5500.50")
	if err != nil || val != 550050 {
		t.Fatalf("expected 550050, got %d, err %v", val, err)
	}
	val, err = coercePaise(100)
	if err != nil || val != 10000 {
		t.Fatalf("expected 10000, got %d, err %v", val, err)
	}
	val, err = coercePaise(int64(250))
	if err != nil || val != 25000 {
		t.Fatalf("expected 25000, got %d, err %v", val, err)
	}

	// Fail-closed invalid cases
	invalidInputs := []any{
		nil,
		"",
		"   ",
		"not-a-number",
		"-100.50",
		-50,
		int64(-20),
		float64(-1.5),
		struct{}{},
		[]int{1, 2},
	}
	for _, in := range invalidInputs {
		_, err := coercePaise(in)
		if err == nil {
			t.Errorf("expected error for invalid input %v (%T), got nil", in, in)
		}
	}
}

func TestParseSettlementWebhook_MissingAmount(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{
			name: "missing payment_amount",
			payload: `{
				"data": {
					"settlement": {
						"settlement_id": "738",
						"amount_settled": 97.94
					}
				}
			}`,
		},
		{
			name: "nil payment_amount",
			payload: `{
				"data": {
					"settlement": {
						"settlement_id": "738",
						"payment_amount": null,
						"amount_settled": 97.94
					}
				}
			}`,
		},
		{
			name: "missing amount_settled",
			payload: `{
				"data": {
					"settlement": {
						"settlement_id": "738",
						"payment_amount": 100.00
					}
				}
			}`,
		},
		{
			name: "nil amount_settled",
			payload: `{
				"data": {
					"settlement": {
						"settlement_id": "738",
						"payment_amount": 100.00,
						"amount_settled": null
					}
				}
			}`,
		},
		{
			name: "empty string payment_amount",
			payload: `{
				"data": {
					"settlement": {
						"settlement_id": "738",
						"payment_amount": "",
						"amount_settled": 97.94
					}
				}
			}`,
		},
		{
			name: "whitespace amount_settled",
			payload: `{
				"data": {
					"settlement": {
						"settlement_id": "738",
						"payment_amount": 100.00,
						"amount_settled": "   "
					}
				}
			}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := ParseSettlementWebhook([]byte(tc.payload))
			if err == nil {
				t.Fatalf("expected error for payload, got parsed record: %+v", rec)
			}
		})
	}
}
