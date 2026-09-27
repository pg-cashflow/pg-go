package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// NotificationService is the read-side of the in-app notification system.
// The concrete implementation is *notification.Service.
type NotificationService interface {
	List(ctx context.Context, recipientID uuid.UUID, cursor string, limit int) ([]*domain.Notification, string, int, error)
	MarkAsRead(ctx context.Context, notificationID, recipientID uuid.UUID) error
	MarkAllAsRead(ctx context.Context, recipientID uuid.UUID) error
}

// ListNotifications handles GET /notifications — paginated notification list
// for the authenticated user, including total unread badge count.
//
// Query params:
//   - cursor (optional): opaque compound cursor from a prior response
//   - limit  (optional): 1–50, defaults to 20
func (h *Handlers) ListNotifications(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}

	cursor := c.Query("cursor")
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = 20
	}

	items, nextCursor, unread, err := h.NotificationSvc.List(c.Request.Context(), claims.UserID, cursor, limit)
	if err != nil {
		respondErr(c, err)
		return
	}

	resp := gin.H{
		"notifications": items,
		"unread_count":  unread,
	}
	if nextCursor != "" {
		resp["next_cursor"] = nextCursor
	}
	c.JSON(http.StatusOK, resp)
}

// MarkNotificationRead handles PATCH /notifications/:id/read.
// The repo scopes the UPDATE to recipient_id = claims.UserID, making
// cross-user tampering impossible at the DB layer.
func (h *Handlers) MarkNotificationRead(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	notifID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if err := h.NotificationSvc.MarkAsRead(c.Request.Context(), notifID, claims.UserID); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// MarkAllNotificationsRead handles PATCH /notifications/read-all.
// Marks every unread notification for the authenticated user as read.
func (h *Handlers) MarkAllNotificationsRead(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	if err := h.NotificationSvc.MarkAllAsRead(c.Request.Context(), claims.UserID); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
