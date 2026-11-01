package api

import (
	"context"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

func (h *Handlers) tenantPayable(ctx context.Context, t *domain.Tenant) bool {
	if t == nil {
		return false
	}
	if t.IsMinor(time.Now().UTC()) && t.GuardianConsentVerifiedAt == nil {
		return false
	}
	requireKYC := true
	if h.OnboardingSettings != nil {
		s, err := h.OnboardingSettings.Get(ctx, t.PropertyID)
		if err == nil && s != nil {
			requireKYC = s.RequireKYCBeforePay
		}
	} else if h.KYCSvc == nil {
		requireKYC = false
	}
	kycVerified := true
	if requireKYC {
		kycVerified = h.IsKYCVerified(ctx, t.ID)
	}
	return t.Payable(requireKYC, kycVerified)
}
