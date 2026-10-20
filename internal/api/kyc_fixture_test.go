package api

import (
	"context"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// verifiedKYCSvc reports every tenant as KYC-verified. Route and isolation fixtures use it
// so that tenant behavior is tested past the KYC onboarding gate.
func verifiedKYCSvc() KYCService {
	return &fakeKYCSvc{statusFn: func(_ context.Context, _ uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
		return &domain.KYCVerification{Status: domain.KYCStatusVerified}, nil, nil
	}}
}
