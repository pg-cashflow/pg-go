package kyc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// KYCReconcilerStore defines persistence operations required for KYC reconciliation.
type KYCReconcilerStore interface {
	ListRequestUnknownVerifications(ctx context.Context, limit int) ([]*domain.KYCVerification, error)
	ExpireVerificationTx(ctx context.Context, verificationID uuid.UUID, reason, actor string, leaseToken ...uuid.UUID) error
	FailVerificationTx(ctx context.Context, verificationID uuid.UUID, reason, actor string, leaseToken ...uuid.UUID) error
	UpdateVerificationPendingManualReviewTx(
		ctx context.Context,
		verificationID uuid.UUID,
		maskedUID, identityHash, ocrToStore, documentURL string,
		encDocBytes []byte,
		documentContentType string,
		isDedupable bool,
		docType domain.DocumentType,
		actor string,
		leaseToken ...uuid.UUID,
	) error
	TryAcquireInFlightLockWithToken(ctx context.Context, tenantID, leaseToken uuid.UUID, ttl time.Duration) (bool, error)
	ReleaseInFlightLockWithToken(ctx context.Context, tenantID, leaseToken uuid.UUID) error
}

// KYCReconciler handles durable background reconciliation of request_unknown verifications.
type KYCReconciler struct {
	repo      KYCReconcilerStore
	cashfree  CashfreeKYCClient
	processor KYCCallbackProcessor
	nowFn     func() time.Time
}

// NewKYCReconciler constructs a new KYCReconciler instance.
func NewKYCReconciler(repo KYCReconcilerStore, cashfree CashfreeKYCClient, processor KYCCallbackProcessor) *KYCReconciler {
	return &KYCReconciler{
		repo:      repo,
		cashfree:  cashfree,
		processor: processor,
		nowFn:     time.Now,
	}
}

// ErrInFlightLockContention indicates a verification is currently leased by another worker or active tenant session.
var ErrInFlightLockContention = errors.New("kyc reconciler: verification locked by active lease")

// Start runs the reconciler periodically in the background until the context is canceled.
func (r *KYCReconciler) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.ReconcileBatch(ctx, 50); err != nil {
				slog.ErrorContext(ctx, "kyc reconciler: batch execution failed", "err", err)
			}
		}
	}
}

// ReconcileBatch processes up to limit verifications currently in request_unknown status.
func (r *KYCReconciler) ReconcileBatch(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	items, err := r.repo.ListRequestUnknownVerifications(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("kyc reconciler: list request_unknown: %w", err)
	}

	reconciled := 0
	for _, v := range items {
		if v == nil || v.Status != domain.KYCStatusRequestUnknown {
			continue
		}
		if err := r.reconcileSingle(ctx, v); err != nil {
			if errors.Is(err, ErrInFlightLockContention) {
				continue
			}
			slog.WarnContext(ctx, "kyc reconciler: failed to reconcile verification",
				"verification_id", v.ID,
				"method", v.Method,
				"err", err,
			)
			continue
		}
		reconciled++
	}

	return reconciled, nil
}

func (r *KYCReconciler) reconcileSingle(ctx context.Context, v *domain.KYCVerification) error {
	now := r.nowFn().UTC()
	const actor = "kyc:reconciler"
	leaseToken := uuid.New()

	if r.repo != nil {
		locked, err := r.repo.TryAcquireInFlightLockWithToken(ctx, v.TenantID, leaseToken, 5*time.Minute)
		if err != nil {
			return fmt.Errorf("kyc reconciler: lock tenant: %w", err)
		}
		if !locked {
			slog.InfoContext(ctx, "kyc reconciler: skipping verification with active in-flight lease", "tenant_id", v.TenantID, "verification_id", v.ID)
			return ErrInFlightLockContention
		}
		defer func() {
			_ = r.repo.ReleaseInFlightLockWithToken(ctx, v.TenantID, leaseToken)
		}()
	}

	switch v.Method {
	case domain.KYCMethodDigiLocker:
		return r.reconcileDigiLocker(ctx, v, now, actor, leaseToken)

	case domain.KYCMethodAadhaarOTP:
		return r.reconcileAadhaarOTP(ctx, v, now, actor, leaseToken)

	case domain.KYCMethodOCR:
		return r.reconcileDocumentUpload(ctx, v, now, actor, leaseToken)

	default:
		// For unhandled or legacy methods, expire if older than 24 hours
		if now.Sub(v.CreatedAt) > 24*time.Hour {
			return r.repo.ExpireVerificationTx(ctx, v.ID, "unhandled method ambiguity window expired", actor, leaseToken)
		}
		return nil
	}
}

func (r *KYCReconciler) reconcileDigiLocker(ctx context.Context, v *domain.KYCVerification, now time.Time, actor string, leaseToken uuid.UUID) error {
	if r.cashfree == nil || v.VendorReferenceID == "" {
		if now.Sub(v.CreatedAt) > 24*time.Hour {
			return r.repo.ExpireVerificationTx(ctx, v.ID, "digilocker session expired without vendor reference", actor, leaseToken)
		}
		return nil
	}

	statusRes, err := r.cashfree.GetDigiLockerStatus(ctx, v.VendorReferenceID)
	if err != nil {
		// If gateway returns 404 or definitively not found, or if session expired (>24h)
		var apiErr *cashfree.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			return r.repo.FailVerificationTx(ctx, v.ID, "DIGILOCKER_SESSION_NOT_FOUND", actor, leaseToken)
		}
		if now.Sub(v.CreatedAt) > 24*time.Hour {
			return r.repo.ExpireVerificationTx(ctx, v.ID, "digilocker session query timed out after 24 hours", actor, leaseToken)
		}
		return fmt.Errorf("query digilocker status: %w", err)
	}

	if statusRes == nil {
		return nil
	}

	st := strings.ToUpper(strings.TrimSpace(string(statusRes.Status)))
	switch st {
	case "AUTHENTICATED", "SUCCESS":
		if r.processor != nil {
			cfRefID := ""
			if v.CFReferenceID != nil {
				cfRefID = *v.CFReferenceID
			}
			_, procErr := r.processor.ProcessDigiLockerCallback(ctx, v.VendorReferenceID, cfRefID, "DIGILOCKER_VERIFICATION_SUCCESS", st, actor)
			return procErr
		}
		return nil

	case "EXPIRED":
		return r.repo.ExpireVerificationTx(ctx, v.ID, "DIGILOCKER_SESSION_EXPIRED", actor, leaseToken)

	case "FAILED", "REJECTED":
		return r.repo.FailVerificationTx(ctx, v.ID, "DIGILOCKER_VERIFICATION_FAILED", actor, leaseToken)

	default:
		// Still in progress at provider. If past 24h, expire.
		if now.Sub(v.CreatedAt) > 24*time.Hour {
			return r.repo.ExpireVerificationTx(ctx, v.ID, "digilocker pending timeout exceeded 24 hours", actor, leaseToken)
		}
		return nil
	}
}

func (r *KYCReconciler) reconcileAadhaarOTP(ctx context.Context, v *domain.KYCVerification, now time.Time, actor string, leaseToken uuid.UUID) error {
	// UIDAI Aadhaar OTP validity is 10 minutes.
	// If 15 minutes have elapsed since creation, the OTP session is definitively expired at UIDAI/Cashfree.
	// Safely transition request_unknown to expired, allowing the tenant to initiate a fresh attempt.
	if now.Sub(v.CreatedAt) >= 15*time.Minute {
		return r.repo.ExpireVerificationTx(ctx, v.ID, "aadhaar otp session expired after ambiguity window", actor, leaseToken)
	}
	return nil
}

func (r *KYCReconciler) reconcileDocumentUpload(ctx context.Context, v *domain.KYCVerification, now time.Time, actor string, leaseToken uuid.UUID) error {
	// If an OCR request is in request_unknown and no webhook callback has arrived within 2 hours,
	// escalate to pending_manual_review so the property owner/operator can review the document directly
	// without stranding the tenant indefinitely.
	if now.Sub(v.CreatedAt) >= 2*time.Hour {
		maskedUID := "XXXX"
		if v.MaskedUID != nil && *v.MaskedUID != "" {
			maskedUID = *v.MaskedUID
		}
		identityHash := ""
		if v.IdentityHash != nil {
			identityHash = *v.IdentityHash
		}
		ocrToStore := `{"ocr_status":"RECONCILER_ESCALATED_MANUAL_REVIEW","reason":"provider outcome unresolved after ambiguity window"}`
		docURL := fmt.Sprintf("/api/owner/kyc/%s/document", v.ID)
		docType := domain.DocTypeAadhaar

		return r.repo.UpdateVerificationPendingManualReviewTx(
			ctx,
			v.ID,
			maskedUID,
			identityHash,
			ocrToStore,
			docURL,
			nil, // document bytes already in store or storage
			"application/octet-stream",
			false,
			docType,
			actor,
			leaseToken,
		)
	}
	return nil
}
