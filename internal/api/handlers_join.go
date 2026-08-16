package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/aadhaar"
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

// JoinMe handles GET /join/me (pending tenant).
func (h *Handlers) JoinMe(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	j, err := h.Joins.Me(c.Request.Context(), claims.UserID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "waiting for owner to assign room and rent"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"join":    j,
		"user":    gin.H{"id": claims.UserID, "role": claims.Role, "phone": "", "property_id": claims.PropertyID},
		"message": "Owner will assign your room and rent.",
	})
}

type joinProfileBody struct {
	Name        string `json:"name"`
	QRPayload   string `json:"qr_payload"`
	UIDLast4    string `json:"uid_last4"`
	Consent     bool   `json:"consent"`
	Confirm     bool   `json:"confirm"`
	AadhaarName string `json:"aadhaar_name"`
}

// JoinProfile handles POST /join (set name / optional KYC on pending request).
func (h *Handlers) JoinProfile(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	var body joinProfileBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	var decoded aadhaar.AadhaarData
	var last4 *string
	if body.QRPayload != "" {
		if !body.Consent {
			c.JSON(http.StatusBadRequest, gin.H{"error": "consent required"})
			return
		}
		d, _, err := aadhaar.DecodeAadhaarQR(body.QRPayload)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "qr decode failed — use manual last-4 if the card has no Secure QR"})
			return
		}
		decoded = d
		if !body.Confirm {
			c.JSON(http.StatusOK, gin.H{"aadhaar": decoded, "needs_confirm": true})
			return
		}
		if decoded.UIDLast4 != "" && decoded.Verified {
			last4 = &decoded.UIDLast4
		}
		if body.Name == "" && decoded.Name != "" {
			body.Name = decoded.Name
		}
		if h.Events != nil && claims.PropertyID != nil {
			_ = aadhaar.RecordConsent(c.Request.Context(), h.Events, uuid.Nil, *claims.PropertyID, "join_app", "aadhaar_kyc")
		}
	} else if strings.TrimSpace(body.UIDLast4) != "" {
		if !body.Consent {
			c.JSON(http.StatusBadRequest, gin.H{"error": "consent required"})
			return
		}
		v := strings.TrimSpace(body.UIDLast4)
		last4 = &v
	}
	if strings.TrimSpace(body.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	j, err := h.Joins.SetProfile(c.Request.Context(), claims.UserID, body.Name, last4)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"join": j, "aadhaar": decoded})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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

// ActivateJoin handles POST /owner/join-requests/:id/activate.
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
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tenantResponse(t))
}

// RejectJoin handles POST /owner/join-requests/:id/reject.
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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
