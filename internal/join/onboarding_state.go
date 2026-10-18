package join

import (
	"context"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// OnboardingState is returned by GET /me/onboarding.
//
// Stage "kyc_required" is a hard gate: the frontend must not render the tenant
// dashboard while in this stage and instead shows the full-screen KYC onboarding
// flow. It is distinct from the older, softer "kyc_optional" stage, which a
// property can still opt into via RequireKYCBeforePay=false if it wants KYC to
// remain advisory only for payments (the dashboard gate is independent of that
// per-property payment policy — it is a platform-wide onboarding requirement).
type OnboardingState struct {
	Stage                string `json:"stage"`
	ProfileSchemaVersion int    `json:"profile_schema_version"`
	KYCStatus            string `json:"kyc_status,omitempty"`
	TenantStatus         string `json:"tenant_status,omitempty"`
	Payable              bool   `json:"payable"`
	RequireKYCBeforePay  bool   `json:"require_kyc_before_pay"`
	// KYCGatePassed mirrors the gate used to compute Stage, exposed so the
	// frontend doesn't need to reverse-engineer gating logic from Stage alone.
	KYCGatePassed bool `json:"kyc_gate_passed"`
}

// BuildOnboardingState computes where the user is in the signup/admission flow.
//
// kycVerified is true only once UIDAI/DigiLocker verification itself completed
// (status == verified). kycGatePassed is broader: it is also true once the tenant
// has submitted a document for manual review (status == pending_manual_review),
// since that is the full extent of what the tenant can do — the remaining step
// (owner approval) is outside their control, so the dashboard gate does not wait
// on it.
func (s *Service) BuildOnboardingState(ctx context.Context, user *domain.User, kycVerified bool, kycGatePassed bool) (*OnboardingState, error) {
	out := &OnboardingState{
		Stage:                "authenticated",
		ProfileSchemaVersion: 1,
		KYCStatus:            "unverified",
	}
	if user == nil {
		return out, nil
	}
	if user.TenantID == nil {
		out.Stage = "profile"
		j, err := s.joins.GetPendingByUser(ctx, user.ID)
		if err == nil && j != nil && j.ProfileSchemaVersion > 0 {
			out.ProfileSchemaVersion = j.ProfileSchemaVersion
		}
		return out, nil
	}
	out.TenantStatus = string(domain.TenantStatusPendingAllocation)
	if s.tenantSvc != nil {
		t, err := s.tenantSvc.GetByID(ctx, *user.TenantID)
		if err == nil && t != nil {
			out.TenantStatus = string(t.Status)
			requireKYC := false
			if s.onboardingDB != nil {
				settings, _ := s.onboardingDB.Get(ctx, t.PropertyID)
				if settings != nil {
					out.ProfileSchemaVersion = settings.ProfileSchemaVersion
					requireKYC = settings.RequireKYCBeforePay
				}
			}
			out.RequireKYCBeforePay = requireKYC
			out.KYCGatePassed = kycGatePassed
			switch {
			case kycVerified:
				out.KYCStatus = "verified"
			case kycGatePassed:
				out.KYCStatus = "pending_manual_review"
			default:
				out.KYCStatus = "unverified"
			}
			out.Payable = t.Payable(requireKYC, kycVerified)

			// Hard onboarding gate: dashboard access requires the tenant to have
			// done their part of identity verification, independent of the
			// per-property RequireKYCBeforePay payment policy above.
			if !kycGatePassed {
				out.Stage = "kyc_required"
				return out, nil
			}

			switch {
			case t.Status == domain.TenantStatusActive:
				out.Stage = "active"
				if out.Payable {
					out.Stage = "payable"
				}
			case t.Status == domain.TenantStatusPendingAllocation:
				out.Stage = "kyc_optional"
			default:
				out.Stage = "authenticated"
			}
		}
	}
	return out, nil
}
