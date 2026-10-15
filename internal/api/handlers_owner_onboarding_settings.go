package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func (h *Handlers) OwnerGetOnboardingSettings(c *gin.Context) {
	h.getOnboardingSettings(c)
}

func (h *Handlers) ManagerGetOnboardingSettings(c *gin.Context) {
	h.getOnboardingSettings(c)
}

func (h *Handlers) getOnboardingSettings(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.OnboardingSettings == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "onboarding settings unavailable"})
		return
	}
	s, err := h.OnboardingSettings.Get(c.Request.Context(), pid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings load failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"onboarding_settings": s})
}

type patchOnboardingSettingsBody struct {
	DefaultRentPaise          *int64 `json:"default_rent_paise"`
	DefaultDepositPaise       *int64 `json:"default_deposit_paise"`
	DefaultDueDay             *int16 `json:"default_due_day"`
	AutoActivate              *bool  `json:"auto_activate"`
	ReserveBedsForOpenInvites *bool  `json:"reserve_beds_for_open_invites"`
	MaxExtraBedsPerRoom       *int   `json:"max_extra_beds_per_room"`
	SlotInviteDefaultTTLDays  *int   `json:"slot_invite_default_ttl_days"`
	WaitlistEnabled           *bool  `json:"waitlist_enabled"`
	WaitlistOfferTTLHours     *int   `json:"waitlist_offer_ttl_hours"`
	RequireKYCBeforePay       *bool  `json:"require_kyc_before_pay"`
	JoinPendingTTLDays        *int   `json:"join_pending_ttl_days"`
	ReferralEnabled           *bool  `json:"referral_enabled"`
	JoinerDiscountBps         *int   `json:"joiner_discount_bps"`
	ReferrerRewardBps         *int   `json:"referrer_reward_bps"`
}

func (h *Handlers) OwnerPatchOnboardingSettings(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.OnboardingSettings == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "onboarding settings unavailable"})
		return
	}
	var body patchOnboardingSettingsBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid body", apierr.CodeRequestInvalidBody)
		return
	}
	patch := postgres.OnboardingSettingsPatch{
		DefaultRentPaise:          body.DefaultRentPaise,
		DefaultDepositPaise:       body.DefaultDepositPaise,
		DefaultDueDay:             body.DefaultDueDay,
		AutoActivate:              body.AutoActivate,
		ReserveBedsForOpenInvites: body.ReserveBedsForOpenInvites,
		MaxExtraBedsPerRoom:       body.MaxExtraBedsPerRoom,
		SlotInviteDefaultTTLDays:  body.SlotInviteDefaultTTLDays,
		WaitlistEnabled:           body.WaitlistEnabled,
		WaitlistOfferTTLHours:     body.WaitlistOfferTTLHours,
		RequireKYCBeforePay:       body.RequireKYCBeforePay,
		JoinPendingTTLDays:        body.JoinPendingTTLDays,
		ReferralEnabled:           body.ReferralEnabled,
		JoinerDiscountBps:         body.JoinerDiscountBps,
		ReferrerRewardBps:         body.ReferrerRewardBps,
	}
	s, err := h.OnboardingSettings.Update(c.Request.Context(), pid, patch)
	if err != nil {
		if errors.Is(err, postgres.ErrOnboardingSettingsInvalid) {
			apierr.RespondClientErr(c, http.StatusUnprocessableEntity, "invalid onboarding settings", apierr.CodeOnboardingSettingsInvalid)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings update failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"onboarding_settings": s})
}

// stubOnboardingSettingsStore supports tests with Get-only wiring.
type stubOnboardingSettingsStore struct {
	get    func(context.Context, uuid.UUID) (*domain.PropertyOnboardingSettings, error)
	update func(context.Context, uuid.UUID, postgres.OnboardingSettingsPatch) (*domain.PropertyOnboardingSettings, error)
}

func (s *stubOnboardingSettingsStore) Get(ctx context.Context, propertyID uuid.UUID) (*domain.PropertyOnboardingSettings, error) {
	if s.get != nil {
		return s.get(ctx, propertyID)
	}
	return &domain.PropertyOnboardingSettings{PropertyID: propertyID}, nil
}

func (s *stubOnboardingSettingsStore) Update(ctx context.Context, propertyID uuid.UUID, patch postgres.OnboardingSettingsPatch) (*domain.PropertyOnboardingSettings, error) {
	if s.update != nil {
		return s.update(ctx, propertyID, patch)
	}
	return s.Get(ctx, propertyID)
}
