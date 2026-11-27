package api

import (
	"context"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

func (h *Handlers) tenantPayable(ctx context.Context, t *domain.Tenant) bool {
	ok, _ := h.tenantPayability(ctx, t)
	return ok
}

// tenantPayability evaluates whether a tenant can pay and returns a typed,
// user-actionable availability_status reason when blocked (never leaking PII).
func (h *Handlers) tenantPayability(ctx context.Context, t *domain.Tenant) (bool, string) {
	if t == nil {
		return false, "TEMPORARILY_UNAVAILABLE"
	}
	age := t.AgeStatus(time.Now().UTC())
	switch age {
	case domain.AgeStateMinor:
		// Minor requires explicit reviewer-approved guardian consent (guardian_consent_verified_at != nil).
		// Submission method/reference is audit metadata; approval timestamp is the single source of truth.
		if t.GuardianConsentVerifiedAt == nil {
			return false, "GUARDIAN_CONSENT_REQUIRED"
		}
	case domain.AgeStateUnknown:
		// KYC-05: Unknown age must not be treated as adult.
		// If DOB / MajorityDate is missing, payment is blocked pending age verification.
		return false, "AGE_VERIFICATION_REQUIRED"
	case domain.AgeStateAdult:
		// Adult is eligible to proceed to property KYC check
	}
	// KYC is mandatory platform-wide before payment.
	// Fail-closed: if the KYC service is unconfigured or unavailable, payment is blocked.
	if h.KYCSvc == nil {
		return false, "TEMPORARILY_UNAVAILABLE"
	}
	kycVerified := h.CanTenantPay(ctx, t.ID)
	if !t.Payable(true, kycVerified) {
		return false, "KYC_REQUIRED"
	}
	return true, ""
}
