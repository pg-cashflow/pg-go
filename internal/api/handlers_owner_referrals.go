package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func (h *Handlers) OwnerListReferrals(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if !h.requirePropertyActiveModule(c, pid, "referrals") {
		return
	}
	if h.Referrals == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "referrals unavailable"})
		return
	}
	list, err := h.Referrals.ListByProperty(c.Request.Context(), pid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"referrals": list})
}

func (h *Handlers) OwnerCreateReferral(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if !h.requirePropertyActiveModule(c, pid, "referrals") {
		return
	}
	if h.Referrals == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "referrals unavailable"})
		return
	}
	var body struct {
		JoinerPhone           string `json:"joiner_phone"`
		JoinerName            string `json:"joiner_name"`
		ReferrerExternalPhone string `json:"referrer_external_phone"`
		ReferrerExternalName  string `json:"referrer_external_name"`
		RewardDestination     string `json:"reward_destination"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid body", apierr.CodeRequestInvalidBody)
		return
	}
	if strings.TrimSpace(body.JoinerPhone) == "" {
		apierr.RespondClientErr(c, http.StatusBadRequest, "joiner_phone required", apierr.CodeRequestInvalidBody)
		return
	}
	dest := domain.ReferralDestReferrerCredit
	if strings.TrimSpace(body.RewardDestination) == "pass_to_joiner" {
		dest = domain.ReferralDestPassToJoiner
	}
	ref, err := h.Referrals.CreateOffline(c.Request.Context(), pid, body.JoinerPhone, body.JoinerName, body.ReferrerExternalPhone, body.ReferrerExternalName, dest)
	if err != nil {
		if err == postgres.ErrReferralDisabled {
			apierr.RespondClientErr(c, http.StatusUnprocessableEntity, "referrals disabled", apierr.CodeRequestInvalidBody)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create failed"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"referral": ref})
}
