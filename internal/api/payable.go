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
	age := t.AgeStatus(time.Now().UTC())
	switch age {
	case domain.AgeStateMinor:
		// Minor requires verified guardian consent
		if t.GuardianConsentVerifiedAt == nil || t.GuardianConsentMethod == nil || *t.GuardianConsentMethod != "otp_verified" {
			return false
		}
	case domain.AgeStateUnknown:
		// KYC-05: Unknown age must not be treated as adult.
		// If DOB / MajorityDate is missing, payment is blocked pending age verification.
		return false
	case domain.AgeStateAdult:
		// Adult is eligible to proceed to property KYC check
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
