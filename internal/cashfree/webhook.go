package cashfree

import (
	"encoding/json"
	"fmt"
)

// SuccessWebhook is the subset of PAYMENT_SUCCESS_WEBHOOK used to settle a due.
type SuccessWebhook struct {
	Type          string
	OrderID       string
	CFPaymentID   string
	BankReference string
	AmountPaise   int
}

// ParseSuccessWebhook reads a raw Cashfree webhook body.
// ok is false for non-success types (failed/dropped) — caller should return 200 and skip settle.
func ParseSuccessWebhook(raw []byte) (SuccessWebhook, bool, error) {
	var payload struct {
		Type string `json:"type"`
		Data struct {
			Order struct {
				OrderID string `json:"order_id"`
			} `json:"order"`
			Payment struct {
				CFPaymentID   json.Number `json:"cf_payment_id"`
				PaymentStatus string      `json:"payment_status"`
				PaymentAmount float64     `json:"payment_amount"`
				BankReference string      `json:"bank_reference"`
			} `json:"payment"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return SuccessWebhook{}, false, fmt.Errorf("cashfree webhook json: %w", err)
	}
	out := SuccessWebhook{
		Type:          payload.Type,
		OrderID:       payload.Data.Order.OrderID,
		CFPaymentID:   payload.Data.Payment.CFPaymentID.String(),
		BankReference: payload.Data.Payment.BankReference,
		AmountPaise:   int(payload.Data.Payment.PaymentAmount*100 + 0.5),
	}
	if payload.Type != "PAYMENT_SUCCESS_WEBHOOK" {
		return out, false, nil
	}
	return out, true, nil
}

// TxnID prefers bank UTR, then Cashfree payment id — stored in payments.upi_txn_id.
func (w SuccessWebhook) TxnID() string {
	if w.BankReference != "" {
		return w.BankReference
	}
	return w.CFPaymentID
}
