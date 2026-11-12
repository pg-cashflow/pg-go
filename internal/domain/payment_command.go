package domain

import (
	"time"

	"github.com/google/uuid"
)

// Payment command types for external gateway side-effects.
const (
	PaymentCommandRefundDispatch = "cashfree_refund_dispatch"
)

const (
	DefaultPaymentCommandMaxAttempts = 12
	MaxPaymentCommandRetryAge        = 48 * time.Hour
)

// PaymentCommandStatus defines the processing lifecycle for payment commands.
type PaymentCommandStatus string

const (
	PaymentCommandStatusPending    PaymentCommandStatus = "pending"
	PaymentCommandStatusProcessing PaymentCommandStatus = "processing"
	PaymentCommandStatusCompleted  PaymentCommandStatus = "completed"
	PaymentCommandStatusFailed     PaymentCommandStatus = "failed"
	PaymentCommandStatusDeadLetter PaymentCommandStatus = "dead_letter"
)

// PaymentCommand represents a durable external payment gateway command.
type PaymentCommand struct {
	ID             int64                `json:"id"`
	CommandType    string               `json:"command_type"`
	PropertyID     uuid.UUID            `json:"property_id"`
	SourceID       uuid.UUID            `json:"source_id"`
	Payload        []byte               `json:"payload"`
	IdempotencyKey string               `json:"idempotency_key"`
	Status         PaymentCommandStatus `json:"status"`
	AttemptCount   int                  `json:"attempt_count"`
	MaxAttempts    int                  `json:"max_attempts"`
	LastError      *string              `json:"last_error,omitempty"`
	LockedUntil    *time.Time           `json:"locked_until,omitempty"`
	NextRetryAt    time.Time            `json:"next_retry_at"`
	CreatedAt      time.Time            `json:"created_at"`
	ProcessedAt    *time.Time           `json:"processed_at,omitempty"`
	FailedAt       *time.Time           `json:"failed_at,omitempty"`
}
