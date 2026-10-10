package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// StepUpAuthInput provides common fields for cryptographic re-authentication.
type StepUpAuthInput struct {
	OTP                string     `json:"otp,omitempty"`
	ReauthConfirmation string     `json:"reauth_confirmation,omitempty"`
	FirebaseIDToken    string     `json:"firebase_id_token,omitempty"`
	Purpose            string     `json:"purpose,omitempty"`
	BatchID            *uuid.UUID `json:"batch_id,omitempty"`
}

// verifyDualControlOrStepUp enforces dual-control maker-checker when a property
// has multiple active owners, or cryptographic step-up re-authentication (OTP or fresh Firebase token)
// when in solo-owner mode.
// If createdBy is provided and len(activeOwners) > 1, maker cannot approve their own action.
// If both h.UserStore and h.Auth are unconfigured (e.g., in lightweight repo unit tests),
// it skips verification. In all other cases (including production), full verification is enforced.
// Returns the approval mode ("dual_control", "solo_owner_reauth") and true if verification succeeded.
func (h *Handlers) verifyDualControlOrStepUp(
	c *gin.Context,
	pid uuid.UUID,
	uid uuid.UUID,
	createdBy *uuid.UUID,
	authInput StepUpAuthInput,
) (string, bool) {
	// Fail-closed: if auth services are unconfigured, reject request unconditionally.
	if h.UserStore == nil && h.Auth == nil {
		slog.Error("step up verification failed: auth services unconfigured", "property_id", pid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "step up verification unconfigured"})
		return "", false
	}

	var activeOwners []domain.User
	if h.UserStore != nil {
		owners, err := h.UserStore.GetByPropertyAndRole(c.Request.Context(), pid, domain.RoleOwner)
		if err == nil {
			activeOwners = owners
		}
	}

	if len(activeOwners) > 1 {
		// Strict maker-checker enforcement
		if createdBy != nil && uid == *createdBy {
			slog.Warn("dual control approval rejected: maker cannot be checker", "user_id", uid)
			respondErr(c, clientErr(http.StatusForbidden, "dual control required: action must be approved by a different owner"))
			return "", false
		}
		// Checker approving under dual control
		return "dual_control", true
	}

	// Solo owner path (or co-owner was removed): require cryptographic step-up re-authentication
	candidateOTP := strings.TrimSpace(authInput.OTP)
	if candidateOTP == "" && len(strings.TrimSpace(authInput.ReauthConfirmation)) == 6 {
		isDigits := true
		for _, r := range strings.TrimSpace(authInput.ReauthConfirmation) {
			if r < '0' || r > '9' {
				isDigits = false
				break
			}
		}
		if isDigits {
			candidateOTP = strings.TrimSpace(authInput.ReauthConfirmation)
		}
	}

	candidateFirebase := strings.TrimSpace(authInput.FirebaseIDToken)

	if h.UserStore == nil || h.Auth == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth service not configured"})
		return "", false
	}

	user, err := h.UserStore.GetByID(c.Request.Context(), uid)
	if err != nil {
		respondErr(c, clientErr(http.StatusUnauthorized, "owner account not found for step-up verification"))
		return "", false
	}

	if candidateOTP != "" {
		phone := strings.TrimSpace(user.Phone)
		if phone == "" {
			respondErr(c, clientErr(http.StatusUnauthorized, "step-up authentication failed: owner has no registered phone"))
			return "", false
		}
		purpose := authInput.Purpose
		if purpose == "" {
			purpose = "payout_approval"
		}
		if err := h.Auth.VerifyStepUpOTPSpecific(c.Request.Context(), phone, candidateOTP, purpose, authInput.BatchID); err != nil {
			if errors.Is(err, auth.ErrInvalidOTP) {
				respondErr(c, clientErr(http.StatusUnauthorized, "invalid payout approval code"))
				return "", false
			}
			if errors.Is(err, auth.ErrOTPExpired) {
				respondErr(c, clientErr(http.StatusUnauthorized, "payout approval code has expired; please request a new code"))
				return "", false
			}
			if errors.Is(err, auth.ErrOTPLocked) {
				respondErr(c, clientErr(http.StatusUnauthorized, "payout approval code locked due to too many failed attempts; please request a new code"))
				return "", false
			}
			respondErr(c, clientErr(http.StatusUnauthorized, fmt.Sprintf("step-up authentication failed: %v", err)))
			return "", false
		}
	} else if candidateFirebase != "" {
		ident, err := h.Auth.VerifyFirebaseStepUp(c.Request.Context(), candidateFirebase, 5*time.Minute)
		if err != nil {
			if errors.Is(err, auth.ErrStaleAuthToken) {
				respondErr(c, clientErr(http.StatusUnauthorized, "firebase re-authentication is stale: fresh authentication (<5 minutes) required for approval"))
				return "", false
			}
			respondErr(c, clientErr(http.StatusUnauthorized, "invalid firebase token for step-up authentication"))
			return "", false
		}
		// Verify Firebase identity matches the authenticated owner
		matches := false
		if user.FirebaseUID != nil && *user.FirebaseUID == ident.UID {
			matches = true
		} else if user.Phone != "" && strings.TrimSpace(user.Phone) == ident.Phone && ident.Phone != "" {
			matches = true
		} else if user.Email != "" && strings.EqualFold(user.Email, ident.Email) && ident.Email != "" {
			matches = true
		}
		if !matches {
			respondErr(c, clientErr(http.StatusUnauthorized, "step-up authentication failed: token identity does not match authenticated owner"))
			return "", false
		}
	} else {
		respondErr(c, clientErr(http.StatusUnauthorized, "step-up authentication required: valid otp or fresh firebase_id_token must be provided for solo-owner approval"))
		return "", false
	}

	return "solo_owner_reauth", true
}
