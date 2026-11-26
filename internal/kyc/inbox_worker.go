package kyc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// KYCAlerter sends operational alerts for KYC webhook processing failures.
type KYCAlerter interface {
	NotifyKYCDeadLetter(ctx context.Context, item *KYCInboxItem, failureErr string) error
}

var (
	// ErrLeaseLost indicates that the worker's processing lease expired or was superseded.
	ErrLeaseLost = errors.New("kyc inbox: lease lost or superseded")
	// ErrDigiLockerDisabled indicates DigiLocker integration is disabled and event must be deferred.
	ErrDigiLockerDisabled = errors.New("kyc inbox: digilocker disabled; deferred")
)

// KYCInboxItem represents a record in kyc_webhook_inbox.
type KYCInboxItem struct {
	ID               int64
	Provider         string
	EventHash        string
	EventTimestamp   string
	VerificationID   *string
	ReferenceID      *string
	EventType        *string
	Status           *string
	Payload          json.RawMessage
	ProcessingStatus string
	Attempts         int
	MaxAttempts      int
	ClaimToken       *string
	LeaseExpiresAt   *time.Time
	NextRetryAt      *time.Time
	ErrorMessage     *string
	ReceivedAt       time.Time
	ProcessedAt      *time.Time
}

// KYCInboxStore defines database operations for the KYC webhook inbox worker.
type KYCInboxStore interface {
	ListPendingInboxIDs(ctx context.Context, limit int) ([]int64, error)
	LockAndClaimInboxTx(ctx context.Context, tx pgx.Tx, id int64, lease time.Duration) (*KYCInboxItem, error)
	MarkInboxProcessedTx(ctx context.Context, tx pgx.Tx, id int64, claimToken string) error
	MarkInboxFailedTx(ctx context.Context, tx pgx.Tx, id int64, claimToken string, errStr string, nextRetry time.Time) error
	MarkInboxDeadLetterTx(ctx context.Context, tx pgx.Tx, id int64, claimToken string, errStr string) error
	GetVerificationForUpdate(ctx context.Context, tx pgx.Tx, verificationID, refID string) (*domain.KYCVerification, error)
	ApplyVerificationWebhookTx(ctx context.Context, tx pgx.Tx, v *domain.KYCVerification, webhookStatus string) error
}

// KYCCallbackProcessor defines domain processing for KYC verification callbacks.
type KYCCallbackProcessor interface {
	ProcessDigiLockerCallback(ctx context.Context, verificationIDStr, cfRefID, eventType, status, actor string) (*domain.KYCVerification, error)
}

// KYCInboxWorker processes pending KYC webhook callbacks asynchronously.
type KYCInboxWorker struct {
	pool                 *pgxpool.Pool
	store                KYCInboxStore
	processor            KYCCallbackProcessor
	alerter              KYCAlerter
	featureKYCDigiLocker bool
}

// NewKYCInboxWorker creates a new KYCInboxWorker.
func NewKYCInboxWorker(pool *pgxpool.Pool, store KYCInboxStore, processor KYCCallbackProcessor, alerter ...KYCAlerter) *KYCInboxWorker {
	var a KYCAlerter
	if len(alerter) > 0 {
		a = alerter[0]
	}
	return &KYCInboxWorker{
		pool:                 pool,
		store:                store,
		processor:            processor,
		alerter:              a,
		featureKYCDigiLocker: true,
	}
}

// SetFeatureKYCDigiLocker configures whether the DigiLocker provider integration is active.
func (w *KYCInboxWorker) SetFeatureKYCDigiLocker(enabled bool) {
	w.featureKYCDigiLocker = enabled
}

// ProcessBatch processes up to limit pending inbox items.
func (w *KYCInboxWorker) ProcessBatch(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	ids, err := w.store.ListPendingInboxIDs(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("list pending kyc inbox ids: %w", err)
	}

	processedCount := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return processedCount, ctx.Err()
		}
		if err := w.ProcessSingleItem(ctx, id); err != nil {
			slog.Error("failed processing kyc inbox item", "inbox_id", id, "err", err)
		} else {
			processedCount++
		}
	}
	return processedCount, nil
}

// Start runs the periodic inbox processing loop until ctx is canceled.
func (w *KYCInboxWorker) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.ProcessBatch(ctx, 25); err != nil {
				slog.ErrorContext(ctx, "kyc inbox worker error", "err", err)
			}
		}
	}
}

// ProcessSingleItem claims, executes, and finalizes a single KYC webhook event.
func (w *KYCInboxWorker) ProcessSingleItem(ctx context.Context, id int64) error {
	leaseDuration := 2 * time.Minute

	var item *KYCInboxItem
	var execErr error

	if w.pool != nil {
		err := pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			claimed, err := w.store.LockAndClaimInboxTx(ctx, tx, id, leaseDuration)
			if err != nil {
				return err
			}
			item = claimed
			return nil
		})
		if err != nil {
			return fmt.Errorf("claim kyc inbox item %d: %w", id, err)
		}
	} else {
		// Mock/test path without pool
		claimed, err := w.store.LockAndClaimInboxTx(ctx, nil, id, leaseDuration)
		if err != nil {
			return fmt.Errorf("claim kyc inbox item %d: %w", id, err)
		}
		item = claimed
	}

	if item == nil {
		return nil // Already claimed or processed
	}
	claimToken := ""
	if item.ClaimToken != nil {
		claimToken = *item.ClaimToken
	}
	if claimToken == "" {
		return fmt.Errorf("kyc inbox item %d claimed without valid lease claim_token", id)
	}

	// Execute processing: for DigiLocker callbacks, avoid an outer transaction
	// holding kyc_verification to prevent lock order inversion with tenants.
	vID := ""
	if item.VerificationID != nil {
		vID = *item.VerificationID
	}
	refID := ""
	if item.ReferenceID != nil {
		refID = *item.ReferenceID
	}
	eventType := ""
	if item.EventType != nil {
		eventType = *item.EventType
	}
	isDigiLocker := strings.HasPrefix(vID, "cf_dgl_") ||
		strings.HasPrefix(refID, "cf_dgl_") ||
		strings.HasPrefix(eventType, "DIGILOCKER_")

	if isDigiLocker {
		execErr = w.executeItem(ctx, nil, item)
	} else if w.pool != nil {
		execErr = pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return w.executeItem(ctx, tx, item)
		})
	} else {
		execErr = w.executeItem(ctx, nil, item)
	}

	// Finalize status: processed, deferred, failed, or dead_letter
	now := time.Now().UTC()
	if execErr == nil {
		if w.pool != nil {
			return pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
				return w.store.MarkInboxProcessedTx(ctx, tx, id, claimToken)
			})
		}
		return w.store.MarkInboxProcessedTx(ctx, nil, id, claimToken)
	}

	if errors.Is(execErr, ErrDigiLockerDisabled) {
		// KYC-FLAG-04: Preserve in-flight callbacks when DigiLocker integration is disabled.
		// Reschedule retry without advancing failure toward dead_letter exhaustion.
		nextRetry := now.Add(5 * time.Minute)
		var markErr error
		if w.pool != nil {
			markErr = pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
				return w.store.MarkInboxFailedTx(ctx, tx, id, claimToken, "digilocker disabled; deferred", nextRetry)
			})
		} else {
			markErr = w.store.MarkInboxFailedTx(ctx, nil, id, claimToken, "digilocker disabled; deferred", nextRetry)
		}
		if markErr != nil {
			slog.ErrorContext(ctx, "kyc inbox: mark deferred failed", "inbox_id", id, "err", markErr)
			return fmt.Errorf("deferral finalization failed (%w): %v", markErr, execErr)
		}
		slog.InfoContext(ctx, "kyc inbox event deferred: digilocker disabled", "inbox_id", id, "next_retry", nextRetry)
		return nil
	}

	// Failure branch: evaluate max attempts
	errStr := sanitizeKYCError(execErr.Error())
	isExhausted := item.Attempts >= item.MaxAttempts

	if isExhausted {
		var markErr error
		if w.pool != nil {
			markErr = pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
				return w.store.MarkInboxDeadLetterTx(ctx, tx, id, claimToken, errStr)
			})
		} else {
			markErr = w.store.MarkInboxDeadLetterTx(ctx, nil, id, claimToken, errStr)
		}
		if markErr != nil {
			slog.ErrorContext(ctx, "kyc inbox: mark dead letter failed", "inbox_id", id, "err", markErr)
		}

		if w.alerter != nil {
			_ = w.alerter.NotifyKYCDeadLetter(ctx, item, errStr)
		}
		slog.ErrorContext(ctx, "kyc inbox event dead-lettered after exhausting attempts",
			"inbox_id", id, "attempts", item.Attempts, "err", errStr)
		if markErr != nil {
			return fmt.Errorf("dead letter finalization failed (%w): %v", markErr, execErr)
		}
		return execErr
	}

	// Calculate exponential backoff with jitter
	backoff := calculateBackoff(item.Attempts)
	nextRetry := now.Add(backoff)

	var markErr error
	if w.pool != nil {
		markErr = pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return w.store.MarkInboxFailedTx(ctx, tx, id, claimToken, errStr, nextRetry)
		})
	} else {
		markErr = w.store.MarkInboxFailedTx(ctx, nil, id, claimToken, errStr, nextRetry)
	}
	if markErr != nil {
		slog.ErrorContext(ctx, "kyc inbox: mark failed retry scheduling failed", "inbox_id", id, "err", markErr)
		return fmt.Errorf("failed retry finalization failed (%w): %v", markErr, execErr)
	}

	slog.WarnContext(ctx, "kyc inbox event retry scheduled",
		"inbox_id", id, "attempts", item.Attempts, "next_retry", nextRetry, "err", errStr)
	return execErr
}

// executeItem performs the state machine check and mutation under row lock.
func (w *KYCInboxWorker) executeItem(ctx context.Context, tx pgx.Tx, item *KYCInboxItem) error {
	vID := ""
	if item.VerificationID != nil {
		vID = *item.VerificationID
	}
	refID := ""
	if item.ReferenceID != nil {
		refID = *item.ReferenceID
	}

	if vID == "" && refID == "" {
		// Missing lookup key cannot be matched to a verification
		return fmt.Errorf("inbox item missing verification_id and reference_id")
	}

	verification, err := w.store.GetVerificationForUpdate(ctx, tx, vID, refID)
	if err != nil {
		return fmt.Errorf("get kyc verification: %w", err)
	}
	if verification == nil {
		// Verification record not found in system
		return fmt.Errorf("kyc verification not found for id=%s ref=%s", vID, refID)
	}

	// Invariant 8: Prevent late callbacks from overwriting terminal states
	if verification.IsTerminal() {
		slog.InfoContext(ctx, "skipping kyc webhook mutation: verification already terminal",
			"verification_id", verification.ID, "status", verification.Status)
		return nil
	}

	webhookStatus := ""
	if item.Status != nil {
		webhookStatus = *item.Status
	}
	eventType := ""
	if item.EventType != nil {
		eventType = *item.EventType
	}

	isDigiLocker := strings.HasPrefix(vID, "cf_dgl_") ||
		strings.HasPrefix(refID, "cf_dgl_") ||
		strings.HasPrefix(eventType, "DIGILOCKER_") ||
		verification.Method == domain.KYCMethodDigiLocker

	if isDigiLocker {
		if !w.featureKYCDigiLocker {
			slog.InfoContext(ctx, "deferring kyc webhook mutation: digilocker disabled", "inbox_id", item.ID)
			return ErrDigiLockerDisabled
		}
		if w.processor != nil {
			// Demultiplex: Delegate to domain DigiLocker callback processor.
			// This fetches the attested Aadhaar document from Cashfree, checks name matching,
			// computes identity hash, routes to manual review on mismatch, or completes
			// verification with full cryptographic credentials (satisfying chk_kyc_verified_fields).
			_, err := w.processor.ProcessDigiLockerCallback(ctx, vID, refID, eventType, webhookStatus, "cashfree:inbox_worker")
			if err != nil && !errors.Is(err, domain.ErrVerificationNotFound) {
				return fmt.Errorf("process digilocker callback: %w", err)
			}
			return nil
		}
		return nil
	}

	// Smart OCR Webhook: OCR alone must NEVER auto-verify!
	// Document upload verifications must remain in pending_manual_review for owner approval.
	if verification.Method == domain.KYCMethodOCR || strings.HasPrefix(refID, "cf_ocr_") {
		var fraudPayload struct {
			FraudChecks struct {
				IsForged        bool `json:"is_forged"`
				IsScreenshot    bool `json:"is_screenshot"`
				IsPhotoOfScreen bool `json:"is_photo_of_screen"`
			} `json:"fraud_checks"`
		}
		if len(item.Payload) > 0 {
			_ = json.Unmarshal(item.Payload, &fraudPayload)
			if fraudPayload.FraudChecks.IsForged || fraudPayload.FraudChecks.IsScreenshot || fraudPayload.FraudChecks.IsPhotoOfScreen {
				slog.WarnContext(ctx, "kyc inbox: document fraud detected in OCR callback",
					"verification_id", verification.ID,
					"is_forged", fraudPayload.FraudChecks.IsForged,
				)
			}
		}
		// Acknowledged and finalized in inbox; stays in pending_manual_review
		return nil
	}

	if verification.Status == domain.KYCStatusPendingManualReview {
		// Pending owner review must not be overwritten by later provider callbacks.
		return nil
	}

	return w.store.ApplyVerificationWebhookTx(ctx, tx, verification, webhookStatus)
}

func calculateBackoff(attempt int) time.Duration {
	base := 5 * time.Second
	max := 15 * time.Minute
	factor := math.Pow(2, float64(attempt-1))
	delay := time.Duration(float64(base) * factor)
	if delay > max {
		delay = max
	}
	// Add 10-25% jitter
	jitter := time.Duration(rand.Float64() * float64(delay) * 0.25)
	return delay + jitter
}

var (
	aadhaarRe = regexp.MustCompile(`\b\d{4}[ -]?\d{4}[ -]?\d{4}\b`)
	panRe     = regexp.MustCompile(`\b[A-Za-z]{5}\d{4}[A-Za-z]\b`)
	phoneRe   = regexp.MustCompile(`\b(?:\+91|0)?[6-9]\d{9}\b`)
	accountRe = regexp.MustCompile(`\b\d{9,18}\b`)
)

func sanitizeKYCError(errStr string) string {
	errStr = aadhaarRe.ReplaceAllString(errStr, "[REDACTED_AADHAAR]")
	errStr = panRe.ReplaceAllString(errStr, "[REDACTED_PAN]")
	errStr = phoneRe.ReplaceAllString(errStr, "[REDACTED_PHONE]")
	errStr = accountRe.ReplaceAllString(errStr, "[REDACTED_ACCOUNT]")
	if len(errStr) > 500 {
		errStr = errStr[:500]
	}
	return errStr
}
