package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func (h *Handlers) OwnerListWaitlist(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.Waitlist == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "waitlist unavailable"})
		return
	}
	roomID, err := uuid.Parse(c.Query("room_id"))
	if err != nil {
		apierr.RespondClientErr(c, http.StatusBadRequest, "room_id required", apierr.CodeRequestInvalidId)
		return
	}
	list, err := h.Waitlist.List(c.Request.Context(), pid, roomID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"waitlist": list})
}

func (h *Handlers) OwnerOfferWaitlistSlot(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	if h.Waitlist == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "waitlist unavailable"})
		return
	}
	roomID, err := uuid.Parse(c.Param("room_id"))
	if err != nil {
		apierr.RespondClientErr(c, http.StatusBadRequest, "invalid room_id", apierr.CodeRequestInvalidId)
		return
	}
	result, err := h.Waitlist.OfferNext(c.Request.Context(), pid, roomID, uid)
	if err != nil {
		switch {
		case errors.Is(err, postgres.ErrWaitlistDisabled):
			apierr.RespondClientErr(c, http.StatusUnprocessableEntity, "waitlist disabled", apierr.CodeWaitlistDisabled)
		case errors.Is(err, postgres.ErrWaitlistNotFound):
			apierr.RespondClientErr(c, http.StatusNotFound, "no waiters", apierr.CodeJoinNotFound)
		case errors.Is(err, postgres.ErrSlotRoomFull):
			apierr.RespondClientErr(c, http.StatusConflict, "room is full", apierr.CodeSlotRoomFull)
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "offer failed"})
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{"waitlist": result.Entry, "slot_invite": result.Invite})
}
