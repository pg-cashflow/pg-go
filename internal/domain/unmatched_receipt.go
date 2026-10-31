package domain

import (
	"time"

	"github.com/google/uuid"
)

// Canonical unmatched receipt statuses.
const (
	ReceiptStatusHeld          = "held"
	ReceiptStatusResolving     = "resolving"
	ReceiptStatusResolved      = "resolved"
	ReceiptStatusHeldUnapplied = "held" // canonical domain alias for HELD_UNAPPLIED
)

// Canonical unmatched receipt resolution actions.
const (
	ReceiptActionCredit   = "applied_as_credit"
	ReceiptActionRefund   = "refunded"
	ReceiptActionWriteOff = "written_off"
)

// Tenant-facing status message.
const (
	TenantUIStatusHeld = "Payment received. Owner is resolving."
)

// Canonical failure reasons.
const (
	ReceiptReasonAmountMismatch    = "amount_mismatch"
	ReceiptReasonUnknownOrder      = "unknown_order"
	ReceiptReasonDueAlreadySettled = "due_already_settled"
	ReceiptReasonSessionSuperseded = "session_superseded"
	ReceiptReasonDuplicatePayment  = "duplicate_payment"
	ReceiptReasonPaymentNotFound   = "payment_not_found"
)

// UnmatchedGatewayReceipt represents an incoming payment that could not be automatically
// applied to dues (e.g. amount mismatch, closed dues) and requires owner resolution.
type UnmatchedGatewayReceipt struct {
	ID                       uuid.UUID  `json:"id"`
	PropertyID               *uuid.UUID `json:"property_id,omitempty"`
	PaymentIntentID          *uuid.UUID `json:"payment_intent_id,omitempty"`
	DueID                    *uuid.UUID `json:"due_id,omitempty"`
	TenantID                 *uuid.UUID `json:"tenant_id,omitempty"`
	PaymentID                *uuid.UUID `json:"payment_id,omitempty"`
	Provider                 string     `json:"provider"`
	ProviderOrderID          string     `json:"provider_order_id"`
	ProviderPaymentID        string     `json:"provider_payment_id"`
	CFPaymentID              *string    `json:"cf_payment_id,omitempty"`
	AmountPaise              int64      `json:"amount_paise"`
	Currency                 string     `json:"currency"`
	PaymentMethod            *string    `json:"payment_method,omitempty"`
	FailureReason            string     `json:"failure_reason"`
	RawPayload               []byte     `json:"raw_payload,omitempty"`
	Status                   string     `json:"status"` // held, resolving, resolved
	ResolvedAt               *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy               *uuid.UUID `json:"resolved_by,omitempty"`
	ResolutionAction         *string    `json:"resolution_action,omitempty"`
	ResolutionRefundID       *uuid.UUID `json:"resolution_refund_id,omitempty"`
	ResolutionIdempotencyKey *string    `json:"resolution_idempotency_key,omitempty"`
	ResolutionNotes          *string    `json:"resolution_notes,omitempty"`
	HeldNotifiedAt           *time.Time `json:"held_notified_at,omitempty"`
	ResolutionNotifiedAt     *time.Time `json:"resolution_notified_at,omitempty"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

// IsResolvable returns true if the receipt is currently held and awaiting resolution.
func (r *UnmatchedGatewayReceipt) IsResolvable() bool {
	return r.Status == ReceiptStatusHeld
}
