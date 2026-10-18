package api

import (
	"context"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

func (h *Handlers) tenantPayable(ctx context.Context, t *domain.Tenant) bool {
	if t == nil {
		return false
	}
	requireKYC := false
	if h.OnboardingSettings != nil {
		s, err := h.OnboardingSettings.Get(ctx, t.PropertyID)
		if err == nil && s != nil {
			requireKYC = s.RequireKYCBeforePay
		}
	}
	kycVerified := true
	if requireKYC {
		kycVerified = h.IsKYCVerified(ctx, t.ID)
	}
	return t.Payable(requireKYC, kycVerified)
}
