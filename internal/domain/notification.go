package domain

import (
	"time"

	"github.com/google/uuid"
)

// Notification is the durable, per-recipient in-app notification record.
// The bell badge count and notification center panel read from this table.
//
// Financial invariants: Title is server-formatted plain text — never a raw
// rupee/paise string computed client-side, never a VPA.
type Notification struct {
	ID               uuid.UUID  `json:"id"`
	SourceEventID    *int64     `json:"source_event_id,omitempty"`
	RecipientID      uuid.UUID  `json:"recipient_id"`
	PropertyID       uuid.UUID  `json:"property_id"`
	Type             string     `json:"type"`
	Title            string     `json:"title"`
	DeepLink         string     `json:"deep_link"`
	IsActionRequired bool       `json:"is_action_required"`
	ReadAt           *time.Time `json:"read_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

// NotificationCursor is a compound pagination cursor (created_at, id) that
// prevents duplicate or skipped items when multiple notifications share
// identical timestamps (which happens during the same dispatcher pass).
type NotificationCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

// RecipientFailure records a single per-recipient dispatch failure.
// RecipientID may be uuid.Nil when the error occurred before recipient
// resolution completed (e.g. malformed payload, DB lookup error).
type RecipientFailure struct {
	RecipientID uuid.UUID `json:"recipient_id"`
	Error       string    `json:"error"`
}

// DispatchErrorSummary is serialised into outbox_events.last_error so an ops
// person looking at a dead-lettered event can see exactly which recipients
// succeeded and which are still waiting — not just an opaque error string.
type DispatchErrorSummary struct {
	Succeeded []uuid.UUID        `json:"succeeded"`
	Failed    []RecipientFailure `json:"failed"`
}

// NotificationDelivery is the per-channel delivery attempt record.
type NotificationDelivery struct {
	ID             uuid.UUID `json:"id"`
	NotificationID uuid.UUID `json:"notification_id"`
	Channel        string    `json:"channel"` // 'inapp' | 'push' | 'whatsapp' | 'sms' | 'email'
	Status         string    `json:"status"`  // 'pending' | 'sent' | 'failed' | 'skipped_pref'
	AttemptCount   int       `json:"attempt_count"`
	LastError      *string   `json:"last_error,omitempty"`
}
