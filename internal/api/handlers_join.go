package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
)

// LookupInvite handles GET /join/invite/:code (public).
func (h *Handlers) LookupInvite(c *gin.Context) {
	if h.Joins == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	p, err := h.Joins.LookupInvite(c.Request.Context(), c.Param("code"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "invalid invite code"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"property_id":   p.ID,
		"property_name": p.Name,
		"owner_name":    p.OwnerName,
	})
}

// JoinMe handles GET /join/me (pending tenant — profile not yet submitted).
func (h *Handlers) JoinMe(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	j, err := h.Joins.Me(c.Request.Context(), claims.UserID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "complete your profile to continue"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"join": j,
		"user": gin.H{
			"id":          claims.UserID,
			"role":        claims.Role,
			"phone":       j.Phone,
			"property_id": claims.PropertyID,
		},
		"message": "Fill your details to enter the tenant portal.",
	})
}

const maxJoinIDPhoto = 2 << 20

// JoinProfile handles POST /join (multipart: profile fields + id photo).
// Completes onboarding: creates pending_allocation tenant and links the user.
func (h *Handlers) JoinProfile(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}

	ct := c.GetHeader("Content-Type")
	var name, permanent, current, parent, emergency string
	var consent bool
	var photo []byte

	if strings.HasPrefix(ct, "multipart/") {
		if err := c.Request.ParseMultipartForm(maxJoinIDPhoto + (1 << 20)); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart body"})
			return
		}
		name = c.PostForm("name")
		permanent = c.PostForm("permanent_address")
		current = c.PostForm("current_address")
		parent = c.PostForm("parent_name")
		emergency = c.PostForm("emergency_phone")
		consent = c.PostForm("consent") == "true" || c.PostForm("consent") == "1"
		if f, err := c.FormFile("image"); err == nil && f != nil {
			if f.Size > maxJoinIDPhoto {
				c.JSON(http.StatusBadRequest, gin.H{"error": "image too large (max 2MB)"})
				return
			}
			src, err := f.Open()
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "image read failed"})
				return
			}
			defer src.Close()
			photo, err = io.ReadAll(io.LimitReader(src, maxJoinIDPhoto+1))
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "image read failed"})
				return
			}
			if len(photo) > maxJoinIDPhoto {
				c.JSON(http.StatusBadRequest, gin.H{"error": "image too large (max 2MB)"})
				return
			}
		}
	} else {
		var body struct {
			Name             string `json:"name"`
			PermanentAddress string `json:"permanent_address"`
			CurrentAddress   string `json:"current_address"`
			ParentName       string `json:"parent_name"`
			EmergencyPhone   string `json:"emergency_phone"`
			Consent          bool   `json:"consent"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		name = body.Name
		permanent = body.PermanentAddress
		current = body.CurrentAddress
		parent = body.ParentName
		emergency = body.EmergencyPhone
		consent = body.Consent
	}

	j, t, err := h.Joins.CompleteOnboarding(c.Request.Context(), claims.UserID, joinsvc.ProfileInput{
		Name:             name,
		PermanentAddress: permanent,
		CurrentAddress:   current,
		ParentName:       parent,
		EmergencyPhone:   emergency,
		Consent:          consent,
		IDPhotoBytes:     photo,
	})
	if err != nil {
		// ADR-2 H2 gap case: CompleteOnboarding can wrap DB constraint errors
		// through a 4xx path. Use typed error matching via clientErr rather
		// than err.Error() pass-through.
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, joinsvc.ErrNotPending):
			status = http.StatusNotFound
		case errors.Is(err, joinsvc.ErrAlreadyOnboarded):
			status = http.StatusConflict
		}
		respondErr(c, clientErr(status, err.Error()))
		return
	}
	if h.OutboxEvents != nil && j != nil {
		payload, _ := json.Marshal(map[string]string{
			"join_request_id": j.ID.String(),
			"name":            j.Name,
		})
		var tid *uuid.UUID
		if t != nil {
			tid = &t.ID
		}
		_ = h.OutboxEvents.InsertEvent(c.Request.Context(), &domain.OutboxEvent{
			EventType:  string(domain.EvtJoinRequested),
			PropertyID: j.PropertyID,
			TenantID:   tid,
			ActorRole:  string(domain.RoleTenant),
			Payload:    payload,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"join":   j,
		"tenant": tenantResponse(t),
		"message": "Profile saved. Re-exchange your Firebase token to enter the tenant portal.",
	})
}

// OwnerInvite handles GET /owner/invite.
func (h *Handlers) OwnerInvite(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	p, err := h.PropertyStore.GetByID(c.Request.Context(), pid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "property"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"invite_code": p.InviteCode, "payment_mode": p.PaymentMode})
}

// OwnerRotateInvite handles POST /owner/invite/rotate.
func (h *Handlers) OwnerRotateInvite(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	code, err := h.Joins.RotateInvite(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invite_code": code})
}

// ListJoinRequests handles GET /owner/join-requests.
func (h *Handlers) ListJoinRequests(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var st *domain.JoinStatus
	if q := c.Query("status"); q != "" {
		s := domain.JoinStatus(q)
		st = &s
	}
	list, err := h.Joins.List(c.Request.Context(), pid, st)
	if err != nil {
		slog.Error("list join requests failed", "property_id", pid, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"join_requests": list})
}

type activateJoinBody struct {
	RoomNumber       *string `json:"room_number"`
	RentAmount       int     `json:"rent_amount" binding:"required"`
	DueDay           int16   `json:"due_day" binding:"required"`
	DepositAmount    int     `json:"deposit_amount"`
	NoticePeriodDays int16   `json:"notice_period_days"`
}

// ActivateJoin handles POST /owner/join-requests/:id/activate (assign room/rent).
func (h *Handlers) ActivateJoin(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var body activateJoinBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "room/rent/due_day required from owner — tenant cannot set rent"})
		return
	}
	if body.DueDay < 1 || body.DueDay > 28 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "due_day must be 1–28"})
		return
	}
	t, err := h.Joins.Activate(c.Request.Context(), pid, id, joinsvc.ActivateInput{
		RoomNumber:       body.RoomNumber,
		RentAmount:       body.RentAmount,
		DueDay:           body.DueDay,
		DepositAmount:    body.DepositAmount,
		NoticePeriodDays: body.NoticePeriodDays,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, joinsvc.ErrNotFound) {
			status = http.StatusNotFound
		}
		respondErr(c, clientErr(status, err.Error()))
		return
	}
	// Notify tenant that their join was approved.
	if h.OutboxEvents != nil {
		payload, _ := json.Marshal(map[string]string{"tenant_id": t.ID.String()})
		_ = h.OutboxEvents.InsertEvent(c.Request.Context(), &domain.OutboxEvent{
			EventType:  string(domain.EvtJoinApproved),
			PropertyID: pid,
			TenantID:   &t.ID,
			ActorRole:  string(domain.RoleOwner),
			Payload:    payload,
		})
	}
	c.JSON(http.StatusOK, tenantResponse(t))
}

// RejectJoin handles POST /owner/join-requests/:id/reject (incomplete profile only).
func (h *Handlers) RejectJoin(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if err := h.Joins.Reject(c.Request.Context(), pid, id); err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
