package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Ledger outbox event type constants.
const (
	LedgerOutboxDepartureSettlement = "departure_settlement_mirror"
	LedgerOutboxPayoutBatch         = "payout_batch_transfer"
	LedgerOutboxPayment             = "payment_mirror"
	LedgerOutboxDepositSettlement   = "deposit_settlement_mirror"
	LedgerOutboxRefund              = "refund_mirror"
	LedgerOutboxRewardRedeem        = "reward_redeem_mirror"
	LedgerOutboxCorrection          = "correction_mirror"
)

// LedgerOutboxEvent represents an atomic post-commit event for double-entry financial journals.
// Enqueued inside the triggering domain transaction (e.g. SettleDepartureUnderLock) before commit.
type LedgerOutboxEvent struct {
	ID             int64           `json:"id"`
	EventType      string          `json:"event_type"`
	PropertyID     uuid.UUID       `json:"property_id"`
	SourceID       uuid.UUID       `json:"source_id"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key"`
	AttemptCount   int             `json:"attempt_count"`
	MaxAttempts    int             `json:"max_attempts"`
	LastError      *string         `json:"last_error,omitempty"`
	NextRetryAt    *time.Time      `json:"next_retry_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	ProcessedAt    *time.Time      `json:"processed_at,omitempty"`
	FailedAt       *time.Time      `json:"failed_at,omitempty"`
}

// DepartureSettlementMirrorPayload defines the serialized payload for departure settlement ledger mirrors.
type DepartureSettlementMirrorPayload struct {
	PropertyID                 uuid.UUID `json:"property_id"`
	DepartureID                uuid.UUID `json:"departure_id"`
	DepositAmountPaise         int64     `json:"deposit_amount_paise"`
	UnusedRentRefundPaise      int64     `json:"unused_rent_refund_paise"`
	TotalDeductions            int64     `json:"total_deductions"`
	NetRefundPaise             int64     `json:"net_refund_paise"`
	OutstandingDuesNettedPaise int64     `json:"outstanding_dues_netted_paise"`
	ReceivableBalancePaise     int64     `json:"receivable_balance_paise"`
	OccurredAt                 time.Time `json:"occurred_at"`
}

// PaymentAllocationItemPayload defines an individual allocation within a payment mirror payload.
type PaymentAllocationItemPayload struct {
	AmountPaise int64  `json:"amount_paise"`
	DueKind     string `json:"due_kind"`
}

// PaymentMirrorPayload defines the serialized payload for payment collection and unapplied payment ledger mirrors.
type PaymentMirrorPayload struct {
	PropertyID     uuid.UUID                      `json:"property_id"`
	PaymentID      uuid.UUID                      `json:"payment_id"`
	Allocations    []PaymentAllocationItemPayload `json:"allocations,omitempty"`
	UnappliedPaise int64                          `json:"unapplied_paise,omitempty"`
	IsUnapplied    bool                           `json:"is_unapplied"`
	MatchedAt      time.Time                      `json:"matched_at"`
	MatchedBy      MatchedBy                      `json:"matched_by"`
	AmountPaise    int64                          `json:"amount_paise"`
}

// RefundAllocationItemPayload defines an individual allocation within a refund mirror payload.
type RefundAllocationItemPayload struct {
	AmountPaise int64  `json:"amount_paise"`
	DueKind     string `json:"due_kind"`
}

// RefundMirrorPayload defines the serialized payload for gateway and manual refunds.
type RefundMirrorPayload struct {
	PropertyID  uuid.UUID                     `json:"property_id"`
	RefundID    uuid.UUID                     `json:"refund_id"`
	PaymentID   uuid.UUID                     `json:"payment_id"`
	AmountPaise int64                         `json:"amount_paise"`
	IsUnapplied bool                          `json:"is_unapplied"`
	Allocations []RefundAllocationItemPayload `json:"allocations,omitempty"`
	DueKind     string                        `json:"due_kind,omitempty"`
	OccurredAt  time.Time                     `json:"occurred_at"`
}

// RewardRedeemMirrorPayload defines the serialized payload for reward point redemptions.
type RewardRedeemMirrorPayload struct {
	PropertyID   uuid.UUID `json:"property_id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	RedemptionID uuid.UUID `json:"redemption_id"`
	PointsSpent  int       `json:"points_spent"`
	AmountPaise  int64     `json:"amount_paise"`
	OccurredAt   time.Time `json:"occurred_at"`
}

// CorrectionMirrorPayload defines the serialized payload for payment corrections.
type CorrectionMirrorPayload struct {
	PropertyID         uuid.UUID  `json:"property_id"`
	CorrectionID       uuid.UUID  `json:"correction_id"`
	OriginalPaymentID  uuid.UUID  `json:"original_payment_id"`
	ReversalPaymentID  *uuid.UUID `json:"reversal_payment_id,omitempty"`
	CorrectedPaymentID *uuid.UUID `json:"corrected_payment_id,omitempty"`
	AmountPaise        int64      `json:"amount_paise"`
	OccurredAt         time.Time  `json:"occurred_at"`
}

