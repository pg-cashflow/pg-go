package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/kyc"
)

// KYCInboxRepo implements kyc.KYCInboxStore for PostgreSQL.
type KYCInboxRepo struct {
	pool    *pgxpool.Pool
	kycRepo *KYCRepo
}

// NewKYCInboxRepo creates a new KYCInboxRepo.
func NewKYCInboxRepo(pool *pgxpool.Pool, kycRepo *KYCRepo) *KYCInboxRepo {
	return &KYCInboxRepo{
		pool:    pool,
		kycRepo: kycRepo,
	}
}

// ListPendingInboxIDs fetches up to limit IDs of unexpired pending or failed items ready for processing.
func (r *KYCInboxRepo) ListPendingInboxIDs(ctx context.Context, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id FROM kyc_webhook_inbox
 		WHERE (
		    (processing_status = 'received' AND received_at <= NOW() - INTERVAL '10 seconds')
		    OR (processing_status = 'failed' AND (next_retry_at IS NULL OR next_retry_at <= NOW()))
		)
		  AND (lease_expires_at IS NULL OR lease_expires_at <= NOW())
		ORDER BY id ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("kyc inbox: list pending: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("kyc inbox: scan pending id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LockAndClaimInboxTx claims an inbox item by advancing attempts and setting lease expiration.
func (r *KYCInboxRepo) LockAndClaimInboxTx(ctx context.Context, tx pgx.Tx, id int64, lease time.Duration) (*kyc.KYCInboxItem, error) {
	runner := DBTX(r.pool)
	if tx != nil {
		runner = tx
	}

	var item kyc.KYCInboxItem
	err := runner.QueryRow(ctx, `
		UPDATE kyc_webhook_inbox
		SET processing_status = 'processing',
		    attempts = attempts + 1,
		    lease_expires_at = NOW() + $2
		WHERE id = $1
		  AND (
		    processing_status IN ('received', 'failed')
		    OR (processing_status = 'processing' AND lease_expires_at <= NOW())
		  )
		RETURNING id, provider, event_hash, event_timestamp, verification_id, reference_id,
		          event_type, status, payload, processing_status, attempts, max_attempts,
		          lease_expires_at, next_retry_at, error_message, received_at, processed_at
	`, id, lease).Scan(
		&item.ID, &item.Provider, &item.EventHash, &item.EventTimestamp,
		&item.VerificationID, &item.ReferenceID, &item.EventType, &item.Status,
		&item.Payload, &item.ProcessingStatus, &item.Attempts, &item.MaxAttempts,
		&item.LeaseExpiresAt, &item.NextRetryAt, &item.ErrorMessage,
		&item.ReceivedAt, &item.ProcessedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // Already claimed or locked
		}
		return nil, fmt.Errorf("kyc inbox: claim item %d: %w", id, err)
	}
	return &item, nil
}

// MarkInboxProcessedTx marks the inbox item as processed.
func (r *KYCInboxRepo) MarkInboxProcessedTx(ctx context.Context, tx pgx.Tx, id int64) error {
	runner := DBTX(r.pool)
	if tx != nil {
		runner = tx
	}
	_, err := runner.Exec(ctx, `
		UPDATE kyc_webhook_inbox
		SET processing_status = 'processed',
		    processed_at = NOW()
		WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("kyc inbox: mark processed %d: %w", id, err)
	}
	return nil
}

// MarkInboxFailedTx updates an inbox item status to failed and schedules the next retry.
func (r *KYCInboxRepo) MarkInboxFailedTx(ctx context.Context, tx pgx.Tx, id int64, errStr string, nextRetry time.Time) error {
	runner := DBTX(r.pool)
	if tx != nil {
		runner = tx
	}
	_, err := runner.Exec(ctx, `
		UPDATE kyc_webhook_inbox
		SET processing_status = 'failed',
		    error_message = $2,
		    next_retry_at = $3
		WHERE id = $1
	`, id, errStr, nextRetry)
	if err != nil {
		return fmt.Errorf("kyc inbox: mark failed %d: %w", id, err)
	}
	return nil
}

// MarkInboxDeadLetterTx moves an exhausted item to dead_letter status.
func (r *KYCInboxRepo) MarkInboxDeadLetterTx(ctx context.Context, tx pgx.Tx, id int64, errStr string) error {
	runner := DBTX(r.pool)
	if tx != nil {
		runner = tx
	}
	_, err := runner.Exec(ctx, `
		UPDATE kyc_webhook_inbox
		SET processing_status = 'dead_letter',
		    error_message = $2,
		    processed_at = NOW()
		WHERE id = $1
	`, id, errStr)
	if err != nil {
		return fmt.Errorf("kyc inbox: mark dead letter %d: %w", id, err)
	}
	return nil
}

// GetVerificationForUpdate locks and retrieves the kyc_verification record for mutation.
func (r *KYCInboxRepo) GetVerificationForUpdate(ctx context.Context, tx pgx.Tx, verificationID, refID string) (*domain.KYCVerification, error) {
	runner := DBTX(r.pool)
	if tx != nil {
		runner = tx
	}

	verificationID = strings.TrimSpace(verificationID)
	refID = strings.TrimSpace(refID)
	var ocrRef string
	if refID != "" && !strings.HasPrefix(refID, "cf_ocr_") {
		ocrRef = fmt.Sprintf("cf_ocr_%s", refID)
	}

	query := `
		SELECT ` + kycVerificationCols + `
		FROM kyc_verification
		WHERE id::text = $1
		   OR vendor_reference_id = $1
		   OR cf_reference_id = $1
		   OR ($2 <> '' AND (vendor_reference_id = $2 OR cf_reference_id = $2))
		   OR ($3 <> '' AND vendor_reference_id = $3)
		ORDER BY created_at DESC
		LIMIT 1
		FOR UPDATE
	`
	row := runner.QueryRow(ctx, query, verificationID, refID, ocrRef)
	return r.kycRepo.scanKYCVerification(row)
}

// ApplyVerificationWebhookTx executes the verification state mutation under transaction lock.
func (r *KYCInboxRepo) ApplyVerificationWebhookTx(ctx context.Context, tx pgx.Tx, v *domain.KYCVerification, webhookStatus string) error {
	now := time.Now().UTC()
	status := strings.ToUpper(strings.TrimSpace(webhookStatus))

	apply := func(targetTx pgx.Tx) error {
		// Invariant: OCR alone must NEVER auto-verify!
		if v.Method == domain.KYCMethodOCR {
			return nil
		}

		if status == "SUCCESS" || status == "AUTHENTICATED" || status == "VERIFIED" {
			// To satisfy chk_kyc_verified_fields, status='verified' requires:
			// verified_at, expires_at, identity_hash, and masked_uid to NOT be NULL.
			if v.IdentityHash == nil || v.MaskedUID == nil || *v.IdentityHash == "" || *v.MaskedUID == "" {
				tag, err := targetTx.Exec(ctx, `
					UPDATE kyc_verification
					SET status = 'pending_manual_review',
					    updated_at = $2
					WHERE id = $1 AND status NOT IN ('verified', 'rejected', 'revoked', 'expired')
				`, v.ID, now)
				if err != nil {
					return fmt.Errorf("kyc inbox: update verification pending_manual_review: %w", err)
				}
				if tag.RowsAffected() > 0 {
					_ = r.kycRepo.appendAuditLogTx(ctx, targetTx, v.TenantID, "cashfree:webhook_worker", "verification_manual_review", "missing credentials routed to manual review", now)
				}
				return nil
			}

			tag, err := targetTx.Exec(ctx, `
				UPDATE kyc_verification
				SET status = 'verified',
				    verified_at = COALESCE(verified_at, $2),
				    expires_at = COALESCE(expires_at, $2 + INTERVAL '365 days'),
				    updated_at = $2
				WHERE id = $1 AND status NOT IN ('verified', 'rejected', 'revoked', 'expired')
			`, v.ID, now)
			if err != nil {
				return fmt.Errorf("kyc inbox: update verification verified: %w", err)
			}
			if tag.RowsAffected() > 0 {
				_ = r.kycRepo.appendAuditLogTx(ctx, targetTx, v.TenantID, "cashfree:webhook_worker", "verification_verified", "verified by inbox worker", now)
			}
			return nil
		}

		if status == "FAILED" || status == "REJECTED" {
			tag, err := targetTx.Exec(ctx, `
				UPDATE kyc_verification
				SET status = 'failed',
				    updated_at = $2
				WHERE id = $1 AND status NOT IN ('verified', 'rejected', 'revoked', 'expired')
			`, v.ID, now)
			if err != nil {
				return fmt.Errorf("kyc inbox: update verification failed: %w", err)
			}
			if tag.RowsAffected() > 0 {
				_ = r.kycRepo.appendAuditLogTx(ctx, targetTx, v.TenantID, "cashfree:webhook_worker", "verification_failed", "failed by inbox worker", now)
			}
			return nil
		}

		return nil
	}

	if tx != nil {
		return apply(tx)
	}
	return WithinTx(ctx, r.pool, apply)
}
