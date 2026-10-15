package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
)

func (h *Handlers) JoinWaitlist(c *gin.Context) {
	if h.Waitlist == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "waitlist unavailable"})
		return
	}
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.PropertyID == nil {
		return
	}
	var body struct {
		RoomID string `json:"room_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.RoomID == "" {
		apierr.RespondBindErr(c, "room_id required", apierr.CodeRequestInvalidBody)
		return
	}
	roomID, err := uuid.Parse(body.RoomID)
	if err != nil {
		apierr.RespondClientErr(c, http.StatusBadRequest, "invalid room_id", apierr.CodeRequestInvalidId)
		return
	}
	phone, ok := h.joinerPhone(c, claims.UserID)
	if !ok {
		apierr.RespondClientErr(c, http.StatusBadRequest, "phone required", apierr.CodeRequestInvalidBody)
		return
	}
	entry, err := h.Waitlist.Join(c.Request.Context(), *claims.PropertyID, roomID, claims.UserID, phone)
	if err != nil {
		switch {
		case errors.Is(err, postgres.ErrWaitlistDisabled):
			apierr.RespondClientErr(c, http.StatusUnprocessableEntity, "waitlist disabled", apierr.CodeWaitlistDisabled)
		case errors.Is(err, postgres.ErrWaitlistRoomOpen):
			apierr.RespondClientErr(c, http.StatusConflict, "room has capacity", apierr.CodeWaitlistRoomOpen)
		case errors.Is(err, postgres.ErrWaitlistDuplicate):
			apierr.RespondClientErr(c, http.StatusConflict, "already on waitlist", apierr.CodeWaitlistDuplicate)
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "waitlist join failed"})
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{"waitlist": entry})
}

func (h *Handlers) JoinWaitlistAccept(c *gin.Context) {
	if h.Waitlist == nil || h.Joins == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "waitlist unavailable"})
		return
	}
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	phone, ok := h.joinerPhone(c, claims.UserID)
	if !ok {
		apierr.RespondClientErr(c, http.StatusBadRequest, "phone required", apierr.CodeRequestInvalidBody)
		return
	}
	var body struct {
		InviteCode string `json:"invite_code"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.InviteCode == "" {
		apierr.RespondBindErr(c, "invite_code required", apierr.CodeRequestInvalidBody)
		return
	}
	invite, err := h.Waitlist.AcceptOffer(c.Request.Context(), claims.UserID, phone, body.InviteCode, func(ctx context.Context, slotID uuid.UUID) error {
		return h.Joins.BindSlotInvite(ctx, claims.UserID, slotID)
	})
	if err != nil {
		switch {
		case errors.Is(err, joinsvc.ErrInvalidInvite):
			apierr.RespondClientErr(c, http.StatusNotFound, "invalid invite", apierr.CodeJoinInvalidInvite)
		case errors.Is(err, postgres.ErrSlotBoundPhone):
			apierr.RespondClientErr(c, http.StatusForbidden, "invite not for this phone", apierr.CodeSlotBoundPhone)
		case errors.Is(err, joinsvc.ErrNotPending):
			apierr.RespondClientErr(c, http.StatusConflict, "no pending join", apierr.CodeJoinNoPendingRequest)
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "accept failed"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"invite": invite})
}
