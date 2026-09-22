package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// KYCExpiryRepo abstracts the repository operations required for KYC expiration sweeps.
type KYCExpiryRepo interface {
	ExpireLapsedVerifications(ctx context.Context, now time.Time) (int64, error)
	ExpireStalePendingVerifications(ctx context.Context, cutoff time.Time) (int64, error)
}

// KYCExpiryJob sweeps and expires stale pending verifications and lapsed verified records,
// clearing active profile PII (tenants.aadhaar_last4) to enforce the zero-stale-PII policy.
type KYCExpiryJob struct {
	Repo KYCExpiryRepo
}

func NewKYCExpiryJob(repo KYCExpiryRepo) *KYCExpiryJob {
	return &KYCExpiryJob{Repo: repo}
}

func (j *KYCExpiryJob) Run(ctx context.Context, now time.Time, pendingTTL time.Duration) error {
	// 1. Expire stale pending verification sessions past TTL (1 hour default)
	pendingCutoff := now.Add(-pendingTTL)
	stalePending, err := j.Repo.ExpireStalePendingVerifications(ctx, pendingCutoff)
	if err != nil {
		return fmt.Errorf("kyc job: expire stale pending: %w", err)
	}

	// 2. Expire lapsed verified records and scrub profile aadhaar_last4
	lapsed, err := j.Repo.ExpireLapsedVerifications(ctx, now)
	if err != nil {
		return fmt.Errorf("kyc job: expire lapsed verifications: %w", err)
	}

	slog.Info("kyc expiry sweep completed",
		"stale_pending_expired", stalePending,
		"lapsed_verified_expired", lapsed,
	)
	return nil
}
