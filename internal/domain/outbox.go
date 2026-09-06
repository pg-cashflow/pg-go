package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// OutboxEvent is written in the same DB transaction as the triggering domain
// write. The dispatcher polls undispatched events and fans them out to
// recipients as Notification rows. Retry state and dead-letter bookkeeping
// live here, not on the Notification, because an event can have multiple
// recipients in various states.
type OutboxEvent struct {
	ID           int64           `json:"id"`
	EventType    string          `json:"event_type"`
	PropertyID   uuid.UUID       `json:"property_id"`
	TenantID     *uuid.UUID      `json:"tenant_id,omitempty"`
	ActorRole    string          `json:"actor_role"`
	Payload      json.RawMessage `json:"payload"`
	AttemptCount int             `json:"attempt_count"`
	LastError    *string         `json:"last_error,omitempty"`
	NextRetryAt  *time.Time      `json:"next_retry_at,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	DispatchedAt *time.Time      `json:"dispatched_at,omitempty"`
	FailedAt     *time.Time      `json:"failed_at,omitempty"`
}
