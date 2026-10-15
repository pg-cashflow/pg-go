package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
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
	idemKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idemKey == "" {
		idemKey = strings.TrimSpace(c.GetHeader("X-Idempotency-Key"))
	}
	if idemKey != "" && h.JoinIdempotency != nil {
		if cached, err := h.JoinIdempotency.Get(c.Request.Context(), claims.UserID, idemKey); err == nil && len(cached) > 0 {
			c.Data(http.StatusOK, "application/json", cached)
			return
		}
	}

	ct := c.GetHeader("Content-Type")
	var name, permanent, current, parent, emergency string
	var emergencyName string
	var occupation domain.OccupationType
	var details domain.OccupationDetails
	var consent bool
	var photo []byte
	var referralCode string

	if strings.HasPrefix(ct, "multipart/") {
		if err := c.Request.ParseMultipartForm(maxJoinIDPhoto + (1 << 20)); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart body"})
			return
		}
		name = c.PostForm("name")
		permanent = c.PostForm("permanent_address")
		current = c.PostForm("current_address")
		parent = c.PostForm("parent_name")
		emergencyName = c.PostForm("emergency_contact_name")
		if emergencyName == "" {
			emergencyName = parent
		}
		emergency = c.PostForm("emergency_phone")
		if c.PostForm("reward_destination") != "" {
			apierr.RespondClientErr(c, http.StatusBadRequest, "reward_destination not allowed on join", apierr.CodeRequestInvalidBody)
			return
		}
		occupation = domain.OccupationType(c.PostForm("occupation_type"))
		details = domain.OccupationDetails{
			CollegeName:         c.PostForm("college_name"),
			Course:              c.PostForm("course"),
			Year:                c.PostForm("year"),
			EmployerName:        c.PostForm("employer_name"),
			ParentGuardianName:  c.PostForm("parent_guardian_name"),
			ParentGuardianPhone: c.PostForm("parent_guardian_phone"),
			OccupationNote:      c.PostForm("occupation_note"),
		}
		consent = c.PostForm("consent") == "true" || c.PostForm("consent") == "1"
		referralCode = c.PostForm("referral_code")
		if f, err := c.FormFile("image"); err == nil && f != nil {
			if f.Size > maxJoinIDPhoto {
				apierr.RespondClientErr(c, http.StatusBadRequest, "image too large (max 2MB)", apierr.CodeRequestImageTooLarge)
				return
			}
			src, err := f.Open()
			if err != nil {
				apierr.RespondClientErr(c, http.StatusBadRequest, "image read failed", apierr.CodeRequestImageReadFailed)
				return
			}
			defer src.Close()
			photo, err = io.ReadAll(io.LimitReader(src, maxJoinIDPhoto+1))
			if err != nil {
				apierr.RespondClientErr(c, http.StatusBadRequest, "image read failed", apierr.CodeRequestImageReadFailed)
				return
			}
			if len(photo) > maxJoinIDPhoto {
				apierr.RespondClientErr(c, http.StatusBadRequest, "image too large (max 2MB)", apierr.CodeRequestImageTooLarge)
				return
			}
		}
	} else {
		var body struct {
			Name                 string                   `json:"name"`
			PermanentAddress     string                   `json:"permanent_address"`
			CurrentAddress       string                   `json:"current_address"`
			ParentName           string                   `json:"parent_name"`
			EmergencyContactName string                   `json:"emergency_contact_name"`
			EmergencyPhone       string                   `json:"emergency_phone"`
			OccupationType       domain.OccupationType    `json:"occupation_type"`
			OccupationDetails    domain.OccupationDetails `json:"occupation_details"`
			Consent              bool                     `json:"consent"`
			ReferralCode         string                   `json:"referral_code"`
			RewardDestination    string                   `json:"reward_destination"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		if strings.TrimSpace(body.RewardDestination) != "" {
			apierr.RespondClientErr(c, http.StatusBadRequest, "reward_destination not allowed on join", apierr.CodeRequestInvalidBody)
			return
		}
		name = body.Name
		permanent = body.PermanentAddress
		current = body.CurrentAddress
		parent = body.ParentName
		emergencyName = body.EmergencyContactName
		if emergencyName == "" {
			emergencyName = parent
		}
		emergency = body.EmergencyPhone
		occupation = body.OccupationType
		details = body.OccupationDetails
		consent = body.Consent
		referralCode = body.ReferralCode
	}

	j, t, err := h.Joins.CompleteOnboarding(c.Request.Context(), claims.UserID, joinsvc.ProfileInput{
		Name:                 name,
		PermanentAddress:     permanent,
		CurrentAddress:       current,
		ParentName:           parent,
		EmergencyContactName: emergencyName,
		EmergencyPhone:       emergency,
		OccupationType:       occupation,
		Details:              details,
		Consent:              consent,
		IDPhotoBytes:         photo,
		ReferralCode:         referralCode,
	})
	if err != nil {
		respondErr(c, joinHTTPError(err))
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
	if h.AuthUserRepo == nil || h.Auth == nil {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "session refresh failed"})
		return
	}
	freshUser, err := h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID)
	if err != nil || freshUser == nil {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "session refresh failed"})
		return
	}
	accToken, refToken, err := h.Auth.IssueSession(c.Request.Context(), freshUser)
	if err != nil || refToken == "" {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "session creation failed"})
		return
	}
	h.setRefreshCookie(c, refToken, time.Now().Add(auth.RefreshTokenTTL))
	resp := gin.H{
		"join":    j,
		"tenant":  tenantResponse(t),
		"token":   accToken,
		"user":    freshUser,
		"message": "Profile saved.",
	}
	if h.OutboxEvents != nil && j != nil {
		notifyPayload, _ := json.Marshal(map[string]string{
			"join_request_id": j.ID.String(),
			"tenant_name":     j.Name,
			"channel":         "owner_join_notify_stub",
		})
		_ = h.OutboxEvents.InsertEvent(c.Request.Context(), &domain.OutboxEvent{
			EventType:  "owner.join_notify_stub",
			PropertyID: j.PropertyID,
			TenantID:   j.TenantID,
			ActorRole:  string(domain.RoleTenant),
			Payload:    notifyPayload,
		})
	}
	if idemKey != "" && h.JoinIdempotency != nil {
		if b, err := json.Marshal(resp); err == nil {
			_ = h.JoinIdempotency.Put(c.Request.Context(), claims.UserID, idemKey, b)
		}
	}
	c.JSON(http.StatusOK, resp)
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
	RentAmount       int64   `json:"rent_amount" binding:"required"`
	DueDay           int16   `json:"due_day" binding:"required"`
	DepositAmount    int64   `json:"deposit_amount"`
	NoticePeriodDays int16   `json:"notice_period_days"`
}

// ActivateJoin handles POST /owner/join-requests/:id/activate (assign room/rent).
func (h *Handlers) ActivateJoin(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	var body activateJoinBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "room/rent/due_day required from owner — tenant cannot set rent"})
		return
	}
	if body.DueDay < 1 || body.DueDay > 28 {
		apierr.RespondClientErr(c, http.StatusBadRequest, "due_day must be 1–28", apierr.CodeRequestDueDayInvalid)
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
		respondErr(c, joinHTTPError(err))
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
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if err := h.Joins.Reject(c.Request.Context(), pid, id); err != nil {
		respondErr(c, joinHTTPError(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func joinHTTPError(err error) error {
	switch {
	case errors.Is(err, joinsvc.ErrInvalidInvite):
		return clientErrWithCode(http.StatusNotFound, err.Error(), apierr.CodeJoinInvalidInvite)
	case errors.Is(err, joinsvc.ErrNotPending):
		return clientErrWithCode(http.StatusNotFound, err.Error(), apierr.CodeJoinNoPendingRequest)
	case errors.Is(err, joinsvc.ErrNotFound):
		return clientErrWithCode(http.StatusNotFound, err.Error(), apierr.CodeJoinNotFound)
	case errors.Is(err, joinsvc.ErrAlreadyOnboarded):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.CodeJoinAlreadyOnboarded)
	case errors.Is(err, joinsvc.ErrAlreadyActive):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.CodeJoinAlreadyActive)
	case errors.Is(err, joinsvc.ErrNameRequired):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeJoinNameRequired)
	case errors.Is(err, joinsvc.ErrConsentRequired):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeJoinConsentRequired)
	case errors.Is(err, joinsvc.ErrPhotoRequired):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeJoinPhotoRequired)
	case errors.Is(err, joinsvc.ErrProfileIncomplete):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeJoinProfileIncomplete)
	case errors.Is(err, joinsvc.ErrNotPendingOwner):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeJoinRequestNotPending)
	case errors.Is(err, joinsvc.ErrNotAwaitingAssign):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeJoinNotAwaitingAssignment)
	default:
		return err
	}
}

func (h *Handlers) joinerPhone(c *gin.Context, userID uuid.UUID) (string, bool) {
	if h.AuthUserRepo != nil {
		u, err := h.AuthUserRepo.GetByID(c.Request.Context(), userID)
		if err == nil && strings.TrimSpace(u.Phone) != "" {
			return u.Phone, true
		}
	}
	if h.Joins != nil {
		j, err := h.Joins.Me(c.Request.Context(), userID)
		if err == nil && strings.TrimSpace(j.Phone) != "" {
			return j.Phone, true
		}
	}
	return "", false
}
