package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/slotinvite"
)

func (h *Handlers) OwnerCreateSlotInvite(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	if h.SlotInvites == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "slot invites unavailable"})
		return
	}
	var body struct {
		FloorID           uuid.UUID `json:"floor_id"`
		RoomID            uuid.UUID `json:"room_id"`
		RentPaise         int64     `json:"rent_paise"`
		DepositPaise      int64     `json:"deposit_paise"`
		DueDay            int16     `json:"due_day"`
		AllowOverCapacity bool      `json:"allow_over_capacity"`
		Reason            string    `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid body", apierr.CodeRequestInvalidBody)
		return
	}
	if body.AllowOverCapacity {
		if !h.canAllowOverCapacity(c, pid, uid) {
			apierr.Abort(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
			return
		}
		if strings.TrimSpace(body.Reason) == "" {
			apierr.RespondClientErr(c, http.StatusBadRequest, "reason required for over-capacity invite", apierr.CodeRequestInvalidBody)
			return
		}
	}
	inv, err := h.SlotInvites.Create(c.Request.Context(), slotinvite.CreateInput{
		PropertyID:         pid,
		FloorID:            body.FloorID,
		RoomID:             body.RoomID,
		RentPaise:          body.RentPaise,
		DepositPaise:       body.DepositPaise,
		DueDay:             body.DueDay,
		AllowOverCapacity:  body.AllowOverCapacity,
		OverCapacityReason: body.Reason,
		CreatedBy:          uid,
	})
	if err != nil {
		respondSlotInviteErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"invite": inv})
}

func (h *Handlers) OwnerListSlotInvites(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.SlotInvites == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "slot invites unavailable"})
		return
	}
	var status *domain.SlotInviteStatus
	if s := strings.TrimSpace(c.Query("status")); s != "" {
		st := domain.SlotInviteStatus(s)
		status = &st
	}
	list, err := h.SlotInvites.List(c.Request.Context(), pid, status)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"invites": list})
}

func (h *Handlers) OwnerRevokeSlotInvite(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	if h.SlotInvites == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "slot invites unavailable"})
		return
	}
	inviteID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		apierr.RespondClientErr(c, http.StatusBadRequest, "invalid id", apierr.CodeRequestInvalidBody)
		return
	}
	inv, err := h.SlotInvites.Revoke(c.Request.Context(), pid, inviteID, uid)
	if err != nil {
		respondSlotInviteErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invite": inv})
}

func (h *Handlers) canAllowOverCapacity(c *gin.Context, pid, uid uuid.UUID) bool {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return false
	}
	if claims.Role == domain.RoleOwner {
		return true
	}
	if h.CapabilityStore == nil {
		return false
	}
	allowed, err := h.CapabilityStore.HasActive(c.Request.Context(), pid, uid, "slot_invites.allow_over_capacity")
	return err == nil && allowed
}

func respondSlotInviteErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, postgres.ErrSlotRoomFull):
		apierr.RespondClientErr(c, http.StatusConflict, "room is full", apierr.CodeSlotRoomFull)
	case errors.Is(err, postgres.ErrSlotRoomMasterMissing):
		apierr.RespondClientErr(c, http.StatusUnprocessableEntity, "create floors and rooms before slot invites", apierr.CodeOnboardingRoomMasterMissing)
	case errors.Is(err, postgres.ErrSlotInviteNotOpen):
		apierr.RespondClientErr(c, http.StatusConflict, "invite is not open", apierr.CodeSlotInviteNotOpen)
	case errors.Is(err, postgres.ErrSlotCrossProperty):
		apierr.RespondClientErr(c, http.StatusNotFound, "not found", apierr.CodeJoinNotFound)
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "slot invite failed"})
	}
}
