package cashfree

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

var (
	ErrWebhookTimestampDrift   = errors.New("cashfree: webhook timestamp drift exceeds tolerance")
	ErrInvalidWebhookTimestamp = errors.New("cashfree: invalid webhook timestamp")
)

// VerifyWebhookTimestamp validates that the x-webhook-timestamp header is within toleranceSec of now.
func VerifyWebhookTimestamp(tsHeader string, toleranceSec int, now time.Time) error {
	tsHeader = strings.TrimSpace(tsHeader)
	if tsHeader == "" {
		return fmt.Errorf("%w: missing timestamp header", ErrInvalidWebhookTimestamp)
	}

	var parsed time.Time
	// Try parsing as integer unix epoch (seconds or milliseconds)
	if epoch, err := strconv.ParseInt(tsHeader, 10, 64); err == nil {
		if epoch > 1e11 {
			// Milliseconds (13 digits)
			parsed = time.UnixMilli(epoch).UTC()
		} else {
			// Seconds (10 digits)
			parsed = time.Unix(epoch, 0).UTC()
		}
	} else if t, err := time.Parse(time.RFC3339, tsHeader); err == nil {
		parsed = t.UTC()
	} else {
		return fmt.Errorf("%w: cannot parse %q", ErrInvalidWebhookTimestamp, tsHeader)
	}

	diff := math.Abs(now.Sub(parsed).Seconds())
	if diff > float64(toleranceSec) {
		return fmt.Errorf("%w: diff=%.1fs, tolerance=%ds", ErrWebhookTimestampDrift, diff, toleranceSec)
	}
	return nil
}

// WebhookEnvelope represents the outer shell of every Cashfree webhook payload.
type WebhookEnvelope struct {
	Type      string          `json:"type"`
	RawData   json.RawMessage `json:"data"`
	EventTime string          `json:"event_time,omitempty"`
}

// SuccessWebhook is extracted from PAYMENT_SUCCESS_WEBHOOK.
type SuccessWebhook struct {
	Type          string
	OrderID       string
	CFPaymentID   string
	BankReference string
	AmountPaise   int64
	PaymentStatus string
	PaymentTime   string
}

func (w SuccessWebhook) TxnID() string {
	if strings.TrimSpace(w.BankReference) != "" {
		return strings.TrimSpace(w.BankReference)
	}
	return strings.TrimSpace(w.CFPaymentID)
}

// FailedWebhook is extracted from PAYMENT_FAILED_WEBHOOK.
type FailedWebhook struct {
	Type          string
	OrderID       string
	CFPaymentID   string
	AmountPaise   int64
	PaymentStatus string
	FailureReason string
}

// RefundWebhook is extracted from REFUND_STATUS_WEBHOOK or AUTO_REFUND_STATUS_WEBHOOK.
type RefundWebhook struct {
	Type          string
	IsAutoRefund  bool
	CFRefundID    string
	RefundID      string
	OrderID       string
	CFPaymentID   string
	RefundStatus  string
	RefundAmount  int64
	RefundType    string
	RefundReason  string
}

// DisputeWebhook is extracted from DISPUTE_CREATED_WEBHOOK, PAYMENT_DISPUTE_CREATED_WEBHOOK, or DISPUTE_STATUS_UPDATE_WEBHOOK.
type DisputeWebhook struct {
	Type              string
	DisputeID         string
	OrderID           string
	CFPaymentID       string
	DisputeType       string
	DisputeStatus     string
	DisputeAmount     int64 // in paise
	ReasonCode        string
	ReasonDescription string
	RespondBy         string
}

// ParseWebhook parses the raw payload and returns the identified typed struct or an error.
// If payload is invalid JSON, returns a descriptive error so caller can mark dead-letter and return HTTP 200.
func ParseWebhook(raw []byte) (any, string, error) {
	var env WebhookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, "", fmt.Errorf("cashfree webhook envelope json: %w", err)
	}
	evtType := strings.ToUpper(strings.TrimSpace(env.Type))

	switch evtType {
	case "PAYMENT_SUCCESS_WEBHOOK":
		var p struct {
			Order struct {
				OrderID string `json:"order_id"`
			} `json:"order"`
			Payment struct {
				CFPaymentID   json.Number `json:"cf_payment_id"`
				PaymentStatus string      `json:"payment_status"`
				PaymentAmount json.Number `json:"payment_amount"`
				BankReference string      `json:"bank_reference"`
				PaymentTime   string      `json:"payment_time"`
			} `json:"payment"`
		}
		if err := json.Unmarshal(env.RawData, &p); err != nil {
			return nil, evtType, fmt.Errorf("payment_success data json: %w", err)
		}
		paise, err := ParseRupeesToPaise(p.Payment.PaymentAmount.String())
		if err != nil {
			return nil, evtType, fmt.Errorf("payment_success amount parse: %w", err)
		}
		return SuccessWebhook{
			Type:          evtType,
			OrderID:       p.Order.OrderID,
			CFPaymentID:   p.Payment.CFPaymentID.String(),
			BankReference: p.Payment.BankReference,
			AmountPaise:   paise,
			PaymentStatus: p.Payment.PaymentStatus,
			PaymentTime:   p.Payment.PaymentTime,
		}, evtType, nil

	case "PAYMENT_FAILED_WEBHOOK":
		var p struct {
			Order struct {
				OrderID string `json:"order_id"`
			} `json:"order"`
			Payment struct {
				CFPaymentID    json.Number `json:"cf_payment_id"`
				PaymentStatus  string      `json:"payment_status"`
				PaymentAmount  json.Number `json:"payment_amount"`
				PaymentMessage string      `json:"payment_message"`
			} `json:"payment"`
			ErrorDetails struct {
				ErrorDescription string `json:"error_description"`
			} `json:"error_details"`
		}
		if err := json.Unmarshal(env.RawData, &p); err != nil {
			return nil, evtType, fmt.Errorf("payment_failed data json: %w", err)
		}
		paise, _ := ParseRupeesToPaise(p.Payment.PaymentAmount.String())
		reason := p.Payment.PaymentMessage
		if reason == "" {
			reason = p.ErrorDetails.ErrorDescription
		}
		return FailedWebhook{
			Type:          evtType,
			OrderID:       p.Order.OrderID,
			CFPaymentID:   p.Payment.CFPaymentID.String(),
			AmountPaise:   paise,
			PaymentStatus: p.Payment.PaymentStatus,
			FailureReason: reason,
		}, evtType, nil

	case "REFUND_STATUS_WEBHOOK":
		var p struct {
			Refund struct {
				CFRefundID   json.Number `json:"cf_refund_id"`
				RefundID     string      `json:"refund_id"`
				OrderID      string      `json:"order_id"`
				CFPaymentID  json.Number `json:"cf_payment_id"`
				RefundStatus string      `json:"refund_status"`
				RefundAmount json.Number `json:"refund_amount"`
				RefundType   string      `json:"refund_type"`
				RefundReason string      `json:"refund_reason"`
			} `json:"refund"`
		}
		if err := json.Unmarshal(env.RawData, &p); err != nil {
			return nil, evtType, fmt.Errorf("refund_status data json: %w", err)
		}
		paise, err := ParseRupeesToPaise(p.Refund.RefundAmount.String())
		if err != nil {
			return nil, evtType, fmt.Errorf("refund_status amount parse: %w", err)
		}
		return RefundWebhook{
			Type:         evtType,
			IsAutoRefund: false,
			CFRefundID:   p.Refund.CFRefundID.String(),
			RefundID:     p.Refund.RefundID,
			OrderID:      p.Refund.OrderID,
			CFPaymentID:  p.Refund.CFPaymentID.String(),
			RefundStatus: strings.ToUpper(strings.TrimSpace(p.Refund.RefundStatus)),
			RefundAmount: paise,
			RefundType:   p.Refund.RefundType,
			RefundReason: p.Refund.RefundReason,
		}, evtType, nil

	case "AUTO_REFUND_STATUS_WEBHOOK":
		var p struct {
			AutoRefund struct {
				CFRefundID   json.Number `json:"cf_refund_id"`
				RefundID     string      `json:"refund_id"`
				OrderID      string      `json:"order_id"`
				CFPaymentID  json.Number `json:"cf_payment_id"`
				RefundStatus string      `json:"refund_status"`
				RefundAmount json.Number `json:"refund_amount"`
				RefundType   string      `json:"refund_type"`
				RefundReason string      `json:"refund_reason"`
			} `json:"auto_refund"`
		}
		if err := json.Unmarshal(env.RawData, &p); err != nil {
			return nil, evtType, fmt.Errorf("auto_refund_status data json: %w", err)
		}
		paise, err := ParseRupeesToPaise(p.AutoRefund.RefundAmount.String())
		if err != nil {
			return nil, evtType, fmt.Errorf("auto_refund amount parse: %w", err)
		}
		return RefundWebhook{
			Type:         evtType,
			IsAutoRefund: true,
			CFRefundID:   p.AutoRefund.CFRefundID.String(),
			RefundID:     p.AutoRefund.RefundID,
			OrderID:      p.AutoRefund.OrderID,
			CFPaymentID:  p.AutoRefund.CFPaymentID.String(),
			RefundStatus: strings.ToUpper(strings.TrimSpace(p.AutoRefund.RefundStatus)),
			RefundAmount: paise,
			RefundType:   p.AutoRefund.RefundType,
			RefundReason: p.AutoRefund.RefundReason,
		}, evtType, nil

	case "DISPUTE_CREATED_WEBHOOK", "PAYMENT_DISPUTE_CREATED_WEBHOOK", "DISPUTE_STATUS_UPDATE_WEBHOOK":
		var p struct {
			Dispute struct {
				DisputeID         any         `json:"dispute_id"`
				DisputeType       string      `json:"dispute_type"`
				DisputeStatus     string      `json:"dispute_status"`
				OrderID           string      `json:"order_id"`
				CFPaymentID       any         `json:"cf_payment_id"`
				DisputeAmount     json.Number `json:"dispute_amount"`
				ReasonCode        string      `json:"reason_code"`
				ReasonDescription string      `json:"reason_description"`
				RespondBy         string      `json:"respond_by"`
			} `json:"dispute"`
		}
		if err := json.Unmarshal(env.RawData, &p); err != nil {
			return nil, evtType, fmt.Errorf("dispute data json: %w", err)
		}
		paise, _ := ParseRupeesToPaise(p.Dispute.DisputeAmount.String())
		return DisputeWebhook{
			Type:              evtType,
			DisputeID:         fmt.Sprintf("%v", p.Dispute.DisputeID),
			OrderID:           p.Dispute.OrderID,
			CFPaymentID:       fmt.Sprintf("%v", p.Dispute.CFPaymentID),
			DisputeType:       p.Dispute.DisputeType,
			DisputeStatus:     strings.ToUpper(strings.TrimSpace(p.Dispute.DisputeStatus)),
			DisputeAmount:     paise,
			ReasonCode:        p.Dispute.ReasonCode,
			ReasonDescription: p.Dispute.ReasonDescription,
			RespondBy:         p.Dispute.RespondBy,
		}, evtType, nil

	default:
		return nil, evtType, nil
	}
}

// ParseSuccessWebhook preserves backward compatibility for callers expecting the legacy function.
func ParseSuccessWebhook(raw []byte) (SuccessWebhook, bool, error) {
	val, evtType, err := ParseWebhook(raw)
	if err != nil {
		return SuccessWebhook{}, false, err
	}
	if evtType != "PAYMENT_SUCCESS_WEBHOOK" {
		return SuccessWebhook{Type: evtType}, false, nil
	}
	sw, ok := val.(SuccessWebhook)
	return sw, ok, nil
}
