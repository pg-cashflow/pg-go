package notification

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// NotificationRepo is the interface the Service depends on.
type NotificationRepo interface {
	ListNotifications(ctx context.Context, recipientID uuid.UUID, cursor *domain.NotificationCursor, limit int) ([]*domain.Notification, *domain.NotificationCursor, int, error)
	MarkAsRead(ctx context.Context, notificationID, recipientID uuid.UUID) error
	MarkAllAsRead(ctx context.Context, recipientID uuid.UUID) error
}

// Service provides the read-side of in-app notifications:
// listing, badge count, and marking notifications as read.
type Service struct {
	repo NotificationRepo
}

func NewService(repo NotificationRepo) *Service {
	return &Service{repo: repo}
}

// List returns a page of notifications for recipientID using a compound cursor.
// unreadCount is the total number of unread notifications (not just this page).
func (s *Service) List(
	ctx context.Context,
	recipientID uuid.UUID,
	cursorStr string,
	limit int,
) ([]*domain.Notification, string, int, error) {
	var cursor *domain.NotificationCursor
	if cursorStr != "" {
		c, err := decodeCursor(cursorStr)
		if err != nil {
			return nil, "", 0, fmt.Errorf("notification: invalid cursor: %w", err)
		}
		cursor = c
	}

	items, next, unread, err := s.repo.ListNotifications(ctx, recipientID, cursor, limit)
	if err != nil {
		return nil, "", 0, err
	}

	var nextStr string
	if next != nil {
		nextStr = encodeCursor(next)
	}
	return items, nextStr, unread, nil
}

// MarkAsRead marks a single notification as read for recipientID.
// The repo scopes the update to recipient_id so cross-recipient tampering is impossible.
func (s *Service) MarkAsRead(ctx context.Context, notificationID, recipientID uuid.UUID) error {
	return s.repo.MarkAsRead(ctx, notificationID, recipientID)
}

// MarkAllAsRead marks every unread notification as read for recipientID.
func (s *Service) MarkAllAsRead(ctx context.Context, recipientID uuid.UUID) error {
	return s.repo.MarkAllAsRead(ctx, recipientID)
}

// encodeCursor serialises a NotificationCursor to a URL-safe base64 string.
func encodeCursor(c *domain.NotificationCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor deserialises a URL-safe base64 cursor string.
func decodeCursor(s string) (*domain.NotificationCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c domain.NotificationCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.CreatedAt.IsZero() || c.ID == uuid.Nil {
		return nil, fmt.Errorf("malformed cursor fields")
	}
	return &c, nil
}

// BackoffDuration returns the exponential backoff delay for the given attempt
// number: 2^attempts seconds (2s, 4s, 8s, 16s, 32s).
// Computed here — not in SQL — to avoid Postgres multi-column SET pre-update
// read ordering issues.
func BackoffDuration(attempts int) time.Duration {
	return time.Duration(1<<attempts) * time.Second
}
