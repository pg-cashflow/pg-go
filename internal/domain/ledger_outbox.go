package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
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
