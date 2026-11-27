package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// KYCExpiryRepo abstracts the repository operations required for KYC expiration sweeps.
type KYCExpiryRepo interface {
	ExpireLapsedVerifications(ctx context.Context, now time.Time) (int64, error)
	ExpireStalePendingVerifications(ctx context.Context, cutoff time.Time) (int64, error)
	ExpireStaleManualReviews(ctx context.Context, cutoff time.Time) (int64, error)
	ListPendingDigiLockerForReconciliation(ctx context.Context, minAgeCutoff, maxAgeCutoff time.Time) ([]domain.KYCVerification, error)
	ExpireConsentWindowPendingDigiLocker(ctx context.Context, cutoff time.Time) (int64, error)
}

// DigiLockerPoller abstracts the live polling operation for in-flight DigiLocker sessions.
type DigiLockerPoller interface {
	PollDigiLockerStatus(ctx context.Context, tenantID uuid.UUID, actor string) (*domain.KYCVerification, error)
}

// KYCInboxPurger purges processed kyc_webhook_inbox rows older than the retention window.
type KYCInboxPurger interface {
	PurgeProcessedInbox(ctx context.Context, olderThan time.Duration) (int64, error)
}

// KYCInboxRetention is the retention window for processed kyc_webhook_inbox rows (30 days).
const KYCInboxRetention = 30 * 24 * time.Hour

// KYCExpiryJob sweeps and expires stale pending verifications and lapsed verified records,
// clearing active profile PII (tenants.aadhaar_last4) to enforce the zero-stale-PII policy,
// and reconciles in-flight DigiLocker sessions to prevent dropped webhooks from stalling tenants.
type KYCExpiryJob struct {
	Repo        KYCExpiryRepo
	Poller      DigiLockerPoller
	InboxPurger KYCInboxPurger
}

func NewKYCExpiryJob(repo KYCExpiryRepo) *KYCExpiryJob {
	return &KYCExpiryJob{Repo: repo}
}

func (j *KYCExpiryJob) WithPoller(poller DigiLockerPoller) *KYCExpiryJob {
	j.Poller = poller
	return j
}

func (j *KYCExpiryJob) WithInboxPurger(purger KYCInboxPurger) *KYCExpiryJob {
	j.InboxPurger = purger
	return j
}

func (j *KYCExpiryJob) Run(ctx context.Context, now time.Time, pendingTTL time.Duration) error {
	return j.RunWithManualReviewTTL(ctx, now, pendingTTL, 48*time.Hour)
}

func (j *KYCExpiryJob) RunWithManualReviewTTL(ctx context.Context, now time.Time, pendingTTL, manualReviewTTL time.Duration) error {
	// 1. Expire stale pending verification sessions past TTL (24 hour default)
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

	// 3. Expire stale pending manual review verifications past retention cutoff (48-hour SLA)
	manualCutoff := now.Add(-manualReviewTTL)
	staleManual, err := j.Repo.ExpireStaleManualReviews(ctx, manualCutoff)
	if err != nil {
		return fmt.Errorf("kyc job: expire stale manual reviews: %w", err)
	}

	// 4. Force-fail pending DigiLocker sessions older than 15-minute Cashfree consent window
	consentCutoff := now.Add(-15 * time.Minute)
	consentExpired, err := j.Repo.ExpireConsentWindowPendingDigiLocker(ctx, consentCutoff)
	if err != nil {
		return fmt.Errorf("kyc job: expire consent window pending digilocker: %w", err)
	}

	// 5. Active reconciliation sweep: poll in-flight DigiLocker sessions aged 5 to 15 minutes
	var reconciledCount int
	if j.Poller != nil {
		minAgeCutoff := now.Add(-5 * time.Minute)
		maxAgeCutoff := now.Add(-15 * time.Minute)
		pendingDGL, err := j.Repo.ListPendingDigiLockerForReconciliation(ctx, minAgeCutoff, maxAgeCutoff)
		if err != nil {
			slog.Warn("kyc job: list pending digilocker for reconciliation failed", "err", err)
		} else {
			for _, v := range pendingDGL {
				_, pollErr := j.Poller.PollDigiLockerStatus(ctx, v.TenantID, "system:reconcile")
				if pollErr != nil {
					slog.Warn("kyc job: digilocker reconcile poll failed", "verification_id", v.ID, "err", pollErr)
				} else {
					reconciledCount++
				}
			}
		}
	}

	var purgedInbox int64
	if j.InboxPurger != nil {
		purgedInbox, err = j.InboxPurger.PurgeProcessedInbox(ctx, KYCInboxRetention)
		if err != nil {
			slog.Warn("kyc job: purge processed kyc_webhook_inbox failed", "err", err)
		}
	}

	slog.Info("kyc expiry sweep completed",
		"stale_pending_expired", stalePending,
		"lapsed_verified_expired", lapsed,
		"stale_manual_expired", staleManual,
		"digilocker_consent_expired", consentExpired,
		"digilocker_reconciled", reconciledCount,
		"inbox_purged", purgedInbox,
	)
	return nil
}
