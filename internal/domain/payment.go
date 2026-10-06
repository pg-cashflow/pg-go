package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type MatchedBy string

const (
	MatchedByDueCode          MatchedBy = "due_code"
	MatchedByAmountDateWindow MatchedBy = "amount_date_window"
	MatchedByManual           MatchedBy = "manual"
	MatchedByCash             MatchedBy = "cash"
	MatchedByCashfree         MatchedBy = "cashfree"
)

type Payment struct {
	ID                uuid.UUID  `json:"id"`
	PropertyID        *uuid.UUID `json:"property_id,omitempty"`
	DueID             uuid.UUID  `json:"due_id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	UPITxnID          *string    `json:"upi_txn_id,omitempty"`
	CFPaymentID       *string    `json:"cf_payment_id,omitempty"`
	ProviderPaymentID *string    `json:"provider_payment_id,omitempty"`
	Provider          string     `json:"provider,omitempty"`
	Amount            int64      `json:"amount"` // in paise
	MatchedBy         MatchedBy  `json:"matched_by"`
	RecordedBy        *uuid.UUID `json:"recorded_by,omitempty"`
	MatchedAt         time.Time  `json:"matched_at"`
	RawNote           *string    `json:"raw_note,omitempty"`
	IsUnapplied       bool       `json:"is_unapplied"`
	CreatedAt         time.Time  `json:"created_at"`
	SkipOutboxEnqueue bool       `json:"-"`
}

type FinancialCorrection struct {
	ID                   uuid.UUID  `json:"id"`
	PropertyID          uuid.UUID  `json:"property_id"`
	OriginalPaymentID    uuid.UUID  `json:"original_payment_id"`
	ReversalPaymentID    *uuid.UUID `json:"reversal_payment_id,omitempty"`
	CorrectedPaymentID   *uuid.UUID `json:"corrected_payment_id,omitempty"`
	Reason               string     `json:"reason"`
	CorrectedBy          uuid.UUID  `json:"corrected_by"`
	OccurredAt           time.Time  `json:"occurred_at"`
	CreatedAt            time.Time  `json:"created_at"`
}

type PaymentAllocation struct {
	ID          uuid.UUID `json:"id"`
	PaymentID   uuid.UUID `json:"payment_id"`
	DueID       uuid.UUID `json:"due_id"`
	AmountPaise int64     `json:"amount_paise"`
	CreatedAt   time.Time `json:"created_at"`
}

type GatewayRefund struct {
	ID               uuid.UUID  `json:"id"`
	PaymentID        uuid.UUID  `json:"payment_id"`
	PropertyID       uuid.UUID  `json:"property_id"`
	Provider         string     `json:"provider"`
	CFRefundID       *string    `json:"cf_refund_id,omitempty"`
	ProviderRefundID *string    `json:"provider_refund_id,omitempty"`
	RefundReference  *string    `json:"refund_reference,omitempty"`
	IdempotencyKey   *string    `json:"idempotency_key,omitempty"`
	AmountPaise      int64      `json:"amount_paise"`
	Status           string     `json:"status"` // initiated, pending, succeeded, failed, cancelled, on_hold
	Source           string     `json:"source"` // owner, system, cashfree_auto, dashboard
	InitiatedBy      *uuid.UUID `json:"initiated_by,omitempty"`
	Reason           string     `json:"reason"`
	RawPayload       []byte     `json:"raw_payload,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type RefundAllocation struct {
	ID          uuid.UUID  `json:"id"`
	RefundID    uuid.UUID  `json:"refund_id"`
	DueID       *uuid.UUID `json:"due_id,omitempty"`
	AmountPaise int64      `json:"amount_paise"`
	CreatedAt   time.Time  `json:"created_at"`
}

type WebhookEvent struct {
	ID               uuid.UUID  `json:"id"`
	Provider         string     `json:"provider"`
	EventType        string     `json:"event_type"`
	EventID          *string    `json:"event_id,omitempty"`
	Signature        *string    `json:"signature,omitempty"`
	TimestampHeader  *string    `json:"timestamp_header,omitempty"`
	RawPayload       []byte     `json:"raw_payload"`
	ProcessingStatus string     `json:"processing_status"` // received, processed, dead_letter, duplicate
	ErrorMessage     *string    `json:"error_message,omitempty"`
	ReceivedAt       time.Time  `json:"received_at"`
	ProcessedAt      *time.Time `json:"processed_at,omitempty"`
}

var (
	ErrInvalidUTRFormat = errors.New("invalid UTR format: must be 6-50 alphanumeric characters")
	ErrEmptyUTR         = errors.New("UTR cannot be empty")
)

var utrRegex = regexp.MustCompile(`^[A-Z0-9]{6,50}$`)

// NormalizeUTR normalizes a UTR string:
// 1. Removes leading and trailing spaces
// 2. Converts to consistent upper-case
// 3. Validates that it matches required 6-50 alphanumeric characters format
func NormalizeUTR(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrEmptyUTR
	}
	normalized := strings.ToUpper(trimmed)
	if !utrRegex.MatchString(normalized) {
		return "", ErrInvalidUTRFormat
	}
	return normalized, nil
}
