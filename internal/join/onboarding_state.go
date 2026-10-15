package join

import (
	"context"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// OnboardingState is returned by GET /me/onboarding.
type OnboardingState struct {
	Stage                string `json:"stage"`
	ProfileSchemaVersion int    `json:"profile_schema_version"`
	KYCStatus            string `json:"kyc_status,omitempty"`
	TenantStatus         string `json:"tenant_status,omitempty"`
	Payable              bool   `json:"payable"`
	RequireKYCBeforePay  bool   `json:"require_kyc_before_pay"`
}

func (s *Service) BuildOnboardingState(ctx context.Context, user *domain.User, kycVerified bool) (*OnboardingState, error) {
	out := &OnboardingState{
		Stage:               "authenticated",
		ProfileSchemaVersion: 1,
		KYCStatus:           "unverified",
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
			if kycVerified {
				out.KYCStatus = "verified"
			} else {
				out.KYCStatus = "optional"
			}
			out.Payable = t.Payable(requireKYC, kycVerified)
			switch {
			case t.Status == domain.TenantStatusActive:
				out.Stage = "active"
				if out.Payable {
					out.Stage = "payable"
				} else if requireKYC && !kycVerified {
					out.Stage = "kyc_optional"
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
