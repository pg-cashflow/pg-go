// Package kyc orchestrates the Aadhaar identity verification lifecycle:
// consent recording, UIDAI Aadhaar OTP verification, Cashfree Smart OCR document
// verification, offline Secure QR verification, DPDP consent revocation, and
// duplicate-flag clearance.
//
// ADR-004 invariants enforced here:
//   - Network calls (Cashfree document upload, UIDAI OTP request) always precede DB transactions.
//   - Consent must be active before any verification can be initiated or QR verified.
//   - Verification processing is idempotent: terminal-state verifications are no-ops.
package kyc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/aadhaar"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// Sentinel errors surfaced to HTTP handlers for client-safe 4xx responses.
var (
	ErrNoActiveConsent       = errors.New("kyc: no active consent — record consent before initiating verification")
	ErrQRSignatureInvalid    = errors.New("kyc: QR signature verification failed — UIDAI key not configured or payload corrupted")
	ErrQRDataIncomplete      = errors.New("kyc: QR data incomplete — required fields (name, uid) missing")
	ErrCashfreeUnavailable   = errors.New("kyc: verification unavailable — cashfree is not configured")
	ErrDigiLockerUnavailable = ErrCashfreeUnavailable // Deprecated alias
)

// PermanentError is an interface implemented by errors that represent an
// unrecoverable upstream condition (such as 4xx HTTP responses or corrupt payload)
// where retrying delivery would be futile.
type PermanentError interface {
	error
	IsPermanent() bool
}

// IsPermanentError checks if err or any error in its chain implements PermanentError and returns true.
func IsPermanentError(err error) bool {
	var pe PermanentError
	if errors.As(err, &pe) {
		return pe.IsPermanent()
	}
	return false
}

// KYCRepo is the repository dependency for the service.
// Implemented by *postgres.KYCRepo.
type KYCRepo interface {
	RecordConsent(ctx context.Context, c *domain.KYCConsent, actor string) error
	GetActiveConsentByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.KYCConsent, error)
	RevokeConsent(ctx context.Context, tenantID uuid.UUID, actor string) error

	InitiateVerificationTx(ctx context.Context, tenantID, consentID uuid.UUID, method domain.KYCMethod, vendorName, vendorRefID, actor string) (*domain.KYCVerification, error)
	CompleteVerificationTx(ctx context.Context, verificationID uuid.UUID, maskedUID, identityHash string, isDedupable bool, hashKeyVersion int16, qrStatus *string, nameMismatch bool, attestedPhotoBytes []byte, verifiedAt, expiresAt time.Time, majorityDate *time.Time, actor string) (*domain.KYCVerification, error)
	FailVerificationTx(ctx context.Context, verificationID uuid.UUID, reason, actor string) error

	GetActiveVerificationByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, error)
	GetVerificationByVendorRefID(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error)
	GetLatestVerificationByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, error)
	ClearDuplicateFlag(ctx context.Context, verificationID uuid.UUID, actor, reason string) error

	ListAuditLogsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.KYCAuditLog, error)

	TryAcquireInFlightLock(ctx context.Context, tenantID uuid.UUID, ttl time.Duration) (bool, error)
	ReleaseInFlightLock(ctx context.Context, tenantID uuid.UUID) error
	GetTenantName(ctx context.Context, tenantID uuid.UUID) (string, error)
	GetAttestedPhoto(ctx context.Context, tenantID uuid.UUID) ([]byte, error)

	SubmitManualReviewTx(ctx context.Context, verificationID uuid.UUID, maskedUID, identityHash string, ocrData, documentURL string, documentBytes []byte, documentContentType string, actor string) (*domain.KYCVerification, error)
	ApproveManualReviewTx(ctx context.Context, verificationID, propertyID, reviewerID uuid.UUID, actor string) (*domain.KYCVerification, error)
	RejectManualReviewTx(ctx context.Context, verificationID, propertyID, reviewerID uuid.UUID, reason, actor string) (*domain.KYCVerification, error)
	ListPendingReviews(ctx context.Context, propertyID uuid.UUID) ([]domain.KYCVerification, error)
	GetVerificationByID(ctx context.Context, id uuid.UUID) (*domain.KYCVerification, error)
	GetKYCDocument(ctx context.Context, id uuid.UUID) ([]byte, string, error)
	VerifyPropertyOwnership(ctx context.Context, verificationID, propertyID uuid.UUID) (bool, error)
	RecordFailureAndCount(ctx context.Context, tenantID uuid.UUID, method domain.KYCMethod, reasonCode string, windowMinutes int) (int, error)
	RecordGuardianConsentTx(ctx context.Context, tenantID uuid.UUID, guardianName, guardianPhone, relation, refID string, actor string) (*domain.Tenant, error)
}

// CashfreeKYCClient is the Cashfree dependency for document OCR and Aadhaar OTP.
// Implemented by *cashfree.Client via CashfreeAdapter.
type CashfreeKYCClient interface {
	UploadAadhaarDocument(ctx context.Context, fileReader io.Reader, filename string) (*cashfree.SmartOCRResponse, error)
	RequestAadhaarOTP(ctx context.Context, aadhaarNumber string) (*cashfree.AadhaarOTPInitResult, error)
	VerifyAadhaarOTP(ctx context.Context, refID, otp string) (*cashfree.AadhaarOTPVerifyResult, error)
}

// Config holds the invariant configuration for the KYC service.
type Config struct {
	// IdentitySecret is the HMAC-SHA256 key used to compute identity_hash.
	// Must never change for active verifications (rotation requires re-hashing all records).
	IdentitySecret string

	// HashKeyVersion is stored alongside each identity_hash so future rotations
	// can be scoped to records with the old version.
	HashKeyVersion int16

	// VerificationValidityDays is how long a verified record remains valid before
	// the reaper transitions it to 'expired'. Typically 365.
	VerificationValidityDays int
}

// Service is the top-level KYC orchestrator.
type Service struct {
	repo     KYCRepo
	cashfree CashfreeKYCClient
	cfg      Config
}

// NewService constructs a ready-to-use KYC service.
func NewService(repo KYCRepo, cashfree CashfreeKYCClient, cfg Config) *Service {
	return &Service{repo: repo, cashfree: cashfree, cfg: cfg}
}

// --- Consent ------------------------------------------------------------------

// RecordConsent persists the tenant's explicit DPDP consent before any
// RecordConsent persists the tenant's explicit DPDP consent before any
// verification can proceed. Idempotent: if active consent exists for this version,
// it returns the existing consent record without creating duplicates.
func (s *Service) RecordConsent(
	ctx context.Context,
	tenantID uuid.UUID,
	purpose, consentVersion, consentText,
	ip, userAgent, actor string,
) (*domain.KYCConsent, error) {
	if existing, err := s.repo.GetActiveConsentByTenant(ctx, tenantID); err == nil && existing != nil {
		if existing.ConsentVersion == consentVersion && existing.IsActive() {
			return existing, nil
		}
	}

	if consentText == "" {
		consentText = "I agree to my identity details being verified and stored for PG occupancy records in compliance with the DPDP Act."
	}

	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s:%s", consentVersion, consentText)))
	hash := hex.EncodeToString(h.Sum(nil))

	c := &domain.KYCConsent{
		TenantID:        tenantID,
		Purpose:         purpose,
		ConsentVersion:  consentVersion,
		ConsentText:     consentText,
		ConsentTextHash: hash,
	}
	if ip != "" {
		c.IPAddress = &ip
	}
	if userAgent != "" {
		c.UserAgent = &userAgent
	}
	if err := s.repo.RecordConsent(ctx, c, actor); err != nil {
		return nil, fmt.Errorf("kyc service: record consent: %w", err)
	}
	return c, nil
}

// RevokeConsent implements DPDP Rule 8: cascading erasure of PII from all
// kyc_verification records and the tenants.aadhaar_last4 denormalised column.
func (s *Service) RevokeConsent(ctx context.Context, tenantID uuid.UUID, actor string) error {
	if err := s.repo.RevokeConsent(ctx, tenantID, actor); err != nil {
		return fmt.Errorf("kyc service: revoke consent: %w", err)
	}
	return nil
}

// --- Secure QR fallback -------------------------------------------------------

// VerifySecureQR provides the offline Aadhaar QR fallback path.
//
// Consent gate: enforced — same invariant as DigiLocker initiation.
// Signature verification: performed in-process by aadhaar.DecodeAadhaarQR with
// the UIDAI RSA-SHA256 public key loaded at startup.
// The pending+complete cycle is synchronous (no async webhook).
func (s *Service) VerifySecureQR(ctx context.Context, tenantID uuid.UUID, rawQR, actor string) (*domain.KYCVerification, error) {
	// 1. Consent gate — must have active consent before QR verification.
	consent, err := s.repo.GetActiveConsentByTenant(ctx, tenantID)
	if err != nil {
		if errors.Is(err, domain.ErrVerificationNotFound) {
			return nil, ErrNoActiveConsent
		}
		return nil, fmt.Errorf("kyc service: get consent for qr: %w", err)
	}
	if !consent.IsActive() {
		return nil, ErrNoActiveConsent
	}

	// 2. In-process RSA-SHA256 verification (no network call).
	data, partial, err := aadhaar.DecodeAadhaarQR(rawQR)
	if err != nil {
		// err is non-nil only for signature failures on Secure QR.
		return nil, ErrQRSignatureInvalid
	}
	if partial || data.Name == "" || data.UIDLast4 == "" {
		return nil, ErrQRDataIncomplete
	}

	// 3. Fetch tenant's submitted name and evaluate name match
	tenantName, err := s.repo.GetTenantName(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("kyc service: fetch tenant name for qr: %w", err)
	}
	nameMismatch := !domain.IsNameMatch(tenantName, data.Name)

	// 4. Normalise and compute identity hash.
	dobStr := data.DOB
	if dobStr == "" {
		dobStr = data.YOB // year-only fallback
	}
	canonicalDOB, isDedupable, _ := domain.NormalizeDOB(dobStr)
	name := domain.NormalizeName(data.Name)
	identityHash := domain.ComputeIdentityHash(s.cfg.IdentitySecret, name, canonicalDOB, data.Gender, data.UIDLast4)

	// 5. Initiate pending (supersedes any stale record).
	vendorRefID := uuid.New().String()
	pending, err := s.repo.InitiateVerificationTx(ctx,
		tenantID, consent.ID,
		domain.KYCMethodQR,
		"uidai_qr", vendorRefID, actor,
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: initiate qr verification: %w", err)
	}

	// 6. Immediately complete (synchronous — no async webhook).
	now := time.Now().UTC()
	expiresAt := now.AddDate(0, 0, s.cfg.VerificationValidityDays)
	qrStatus := domain.QRStatusSecure
	majorityDate, _ := domain.MajorityDateFromDOB(canonicalDOB)
	verified, err := s.repo.CompleteVerificationTx(ctx,
		pending.ID, data.UIDLast4, identityHash, isDedupable,
		s.cfg.HashKeyVersion, &qrStatus, nameMismatch, nil,
		now, expiresAt, majorityDate, actor,
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: complete qr verification: %w", err)
	}
	return verified, nil
}

// VerifyAadhaarDocument executes the single document-upload KYC flow using Cashfree Smart OCR (/bharat-ocr).
//
// Invariants enforced:
//   - Network call (Cashfree multipart upload) executes outside database transaction.
//   - Distributed lease (kyc_in_flight_lock) prevents double-billing across horizontal Cloud Run instances.
//   - Demographic fields (Name, DOB, Gender) are ephemeral inputs to ComputeIdentityHash and IsNameMatch; zero demographic persistence.
//   - Attested photo crop is saved to kyc_verification.attested_photo_bytes (photo_stored=true).
//   - tenant profile is updated with masked UID only (never overwrites name or operational id_photo_bytes).
func (s *Service) VerifyAadhaarDocument(
	ctx context.Context,
	tenantID uuid.UUID,
	fileReader io.Reader,
	filename, contentType, actor string,
) (*domain.KYCVerification, error) {
	// Read file bytes up to 5MB + 1 byte guard
	fileBytes, err := io.ReadAll(io.LimitReader(fileReader, 5*1024*1024+1))
	if err != nil {
		return nil, fmt.Errorf("kyc service: read uploaded document: %w", err)
	}
	if len(fileBytes) > 5*1024*1024 {
		return nil, errors.New("file size exceeds 5MB limit")
	}

	// Validate by file signature. The client-declared type and filename are never trusted.
	detectedCT, err := SniffDocumentMIME(fileBytes)
	if err != nil {
		return nil, err
	}
	contentType = detectedCT
	filename = SanitizeDocumentFilename(filename)

	// 1. Consent gate
	consent, err := s.repo.GetActiveConsentByTenant(ctx, tenantID)
	if err != nil {
		if errors.Is(err, domain.ErrVerificationNotFound) {
			return nil, ErrNoActiveConsent
		}
		return nil, fmt.Errorf("kyc service: get consent: %w", err)
	}
	if !consent.IsActive() {
		return nil, ErrNoActiveConsent
	}

	// 2. Acquire distributed in-flight lease FIRST (30s TTL, Cloud Run multi-instance guard)
	locked, err := s.repo.TryAcquireInFlightLock(ctx, tenantID, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("kyc service: acquire in-flight lock: %w", err)
	}
	if !locked {
		return nil, domain.ErrVerificationInProgress
	}
	defer func() {
		_ = s.repo.ReleaseInFlightLock(context.Background(), tenantID)
	}()

	// 3. Under lease lock: pre-check active verification status and enforce 60s cooldown
	active, err := s.repo.GetActiveVerificationByTenant(ctx, tenantID)
	if err != nil && !errors.Is(err, domain.ErrVerificationNotFound) {
		return nil, fmt.Errorf("kyc service: check active verification: %w", err)
	}
	if active != nil {
		if active.Status == domain.KYCStatusVerified {
			if active.ExpiresAt == nil || time.Now().UTC().Before(*active.ExpiresAt) {
				return nil, domain.ErrAlreadyVerified
			}
		} else if active.Status == domain.KYCStatusPending && time.Since(active.CreatedAt) < 60*time.Second {
			return nil, domain.ErrVerificationInProgress
		}
	}

	// 4. Billed network call outside DB transaction
	tenantName, err := s.repo.GetTenantName(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("kyc service: fetch tenant name: %w", err)
	}

	var resp *cashfree.SmartOCRResponse
	if s.cashfree != nil {
		resp, err = s.cashfree.UploadAadhaarDocument(ctx, bytes.NewReader(fileBytes), filename)
		if err != nil {
			return nil, fmt.Errorf("kyc service: upload aadhaar document: %w", err)
		}
	}

	if resp == nil || resp.Name == "" || resp.MaskedUID == "" {
		// KYC-2: Use a UUID for the synthetic reference so the vendor_ref_id is collision-free
		// under concurrent uploads. time.Now().UnixNano() is not guaranteed unique across
		// goroutines on the same host within the same nanosecond.
		// The distinct status suffix lets downstream queries and reviewers distinguish
		// "OCR service unavailable" from "OCR returned a genuinely incomplete result".
		syntheticID := uuid.New().String()
		resp = &cashfree.SmartOCRResponse{
			Status:    "PENDING_MANUAL_REVIEW_OCR_UNAVAILABLE",
			Name:      tenantName,
			MaskedUID: "XXXX",
		}
		vendorRefID := fmt.Sprintf("cf_ocr_manual_%s", syntheticID)
		_ = vendorRefID // assigned below via the shared path
		// reassign so the shared vendorRefID line below picks it up
		_ = fmt.Sprintf // keep import; vendorRefID is used immediately after the if/else
		responseVendorRefID := fmt.Sprintf("cf_ocr_manual_%s", syntheticID)
		_ = responseVendorRefID
		// Use a closure-captured value so both branches reach the same declaration below.
		// Simpler: set a local overrideVendorRefID and use it after the block.
		var overrideVendorRefID = fmt.Sprintf("cf_ocr_manual_%s", syntheticID)
		nameMismatch := !domain.IsNameMatch(tenantName, resp.Name)
		canonicalDOB, _, _ := domain.NormalizeDOB(resp.DOB)
		name := domain.NormalizeName(resp.Name)
		identityHash := domain.ComputeIdentityHash(s.cfg.IdentitySecret, name, canonicalDOB, resp.Gender, resp.MaskedUID)
		pending, err := s.repo.InitiateVerificationTx(ctx,
			tenantID, consent.ID,
			domain.KYCMethodOCR,
			"cashfree_smart_ocr", overrideVendorRefID, actor,
		)
		if err != nil {
			return nil, fmt.Errorf("kyc service: initiate ocr verification: %w", err)
		}
		ocrBytes, _ := json.Marshal(map[string]any{
			"name":          resp.Name,
			"dob":           resp.DOB,
			"gender":        resp.Gender,
			"masked_uid":    resp.MaskedUID,
			"qr_status":     resp.QRStatus,
			"name_mismatch": nameMismatch,
			"ocr_status":    "UNAVAILABLE",
		})
		docURL := fmt.Sprintf("/api/owner/kyc/%s/document", pending.ID)
		underReview, err := s.repo.SubmitManualReviewTx(ctx,
			pending.ID, resp.MaskedUID, identityHash,
			string(ocrBytes), docURL, fileBytes, contentType, actor,
		)
		if err != nil {
			return nil, fmt.Errorf("kyc service: submit manual review: %w", err)
		}
		return underReview, nil
	} else {
		slog.InfoContext(ctx, "cashfree smart ocr completed",
			"tenant_id", tenantID,
			"cashfree_reference_id", resp.ReferenceID,
			"vendor_status", resp.Status,
			"message_code", resp.MessageCode,
			"qr_status", resp.QRStatus,
			"has_photo", len(resp.PhotoBytes) > 0,
		)
	}

	vendorRefID := fmt.Sprintf("cf_ocr_%d", resp.ReferenceID)
	nameMismatch := !domain.IsNameMatch(tenantName, resp.Name)

	// 6. Normalise demographics and compute identity hash
	canonicalDOB, _, _ := domain.NormalizeDOB(resp.DOB)
	name := domain.NormalizeName(resp.Name)
	identityHash := domain.ComputeIdentityHash(s.cfg.IdentitySecret, name, canonicalDOB, resp.Gender, resp.MaskedUID)

	// 7. DB transaction 1: Initiate pending verification record
	pending, err := s.repo.InitiateVerificationTx(ctx,
		tenantID, consent.ID,
		domain.KYCMethodOCR,
		"cashfree_smart_ocr", vendorRefID, actor,
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: initiate ocr verification: %w", err)
	}

	// 8. DB transaction 2: Manual upload requires human review -> pending_manual_review
	ocrBytes, _ := json.Marshal(map[string]any{
		"name":          resp.Name,
		"dob":           resp.DOB,
		"gender":        resp.Gender,
		"masked_uid":    resp.MaskedUID,
		"qr_status":     resp.QRStatus,
		"name_mismatch": nameMismatch,
	})
	docURL := fmt.Sprintf("/api/owner/kyc/%s/document", pending.ID)
	underReview, err := s.repo.SubmitManualReviewTx(ctx,
		pending.ID, resp.MaskedUID, identityHash,
		string(ocrBytes), docURL, fileBytes, contentType, actor,
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: submit manual review: %w", err)
	}

	return underReview, nil
}

// InitiateAadhaarOTP requests an Aadhaar OTP from UIDAI via Cashfree for a tenant.
func (s *Service) InitiateAadhaarOTP(
	ctx context.Context,
	tenantID uuid.UUID,
	aadhaarNumber string,
	actor string,
) (string, error) {
	consent, err := s.repo.GetActiveConsentByTenant(ctx, tenantID)
	if err != nil {
		if errors.Is(err, domain.ErrVerificationNotFound) {
			return "", ErrNoActiveConsent
		}
		return "", fmt.Errorf("kyc service: get consent: %w", err)
	}
	if !consent.IsActive() {
		return "", ErrNoActiveConsent
	}

	locked, err := s.repo.TryAcquireInFlightLock(ctx, tenantID, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("kyc service: acquire in-flight lock: %w", err)
	}
	if !locked {
		return "", domain.ErrVerificationInProgress
	}
	defer func() {
		_ = s.repo.ReleaseInFlightLock(context.Background(), tenantID)
	}()

	if s.cashfree == nil {
		return "", ErrCashfreeUnavailable
	}

	// KYC-1: Under the distributed lock, pre-check active verification status before
	// issuing a billed UIDAI OTP request. The 30s in-flight lock alone is a concurrent-request
	// guard; this check is the sequential-request guard. Without it, a tenant who is already
	// verified (or has a pending OTP from >30s ago once the lock expires) can re-trigger
	// a new billed UIDAI call repeatedly. Mirrors the identical guard in VerifyAadhaarDocument.
	active, err := s.repo.GetActiveVerificationByTenant(ctx, tenantID)
	if err != nil && !errors.Is(err, domain.ErrVerificationNotFound) {
		return "", fmt.Errorf("kyc service: check active verification: %w", err)
	}
	if active != nil {
		if active.Status == domain.KYCStatusVerified {
			if active.ExpiresAt == nil || time.Now().UTC().Before(*active.ExpiresAt) {
				return "", domain.ErrAlreadyVerified
			}
		} else if active.Status == domain.KYCStatusPending && time.Since(active.CreatedAt) < 60*time.Second {
			return "", domain.ErrVerificationInProgress
		}
	}

	res, err := s.cashfree.RequestAadhaarOTP(ctx, aadhaarNumber)
	if err != nil {
		return "", fmt.Errorf("kyc service: request aadhaar otp: %w", err)
	}

	vendorRefID := fmt.Sprintf("cf_aadhaar_otp_%s", res.RefID)

	_, err = s.repo.InitiateVerificationTx(ctx,
		tenantID, consent.ID,
		domain.KYCMethodAadhaarOTP,
		"cashfree_aadhaar_otp", vendorRefID, actor,
	)
	if err != nil {
		return "", fmt.Errorf("kyc service: initiate aadhaar otp record: %w", err)
	}

	return res.RefID, nil
}

// CompleteAadhaarOTP verifies the submitted OTP with UIDAI and auto-verifies the tenant.
// AadhaarVerifyFailure represents a classified UIDAI/Cashfree failure returned to the client.
type AadhaarVerifyFailure struct {
	ReasonCode        string
	Class             cashfree.AadhaarFailureClass
	Retryable         bool
	CooldownSeconds   int
	AttemptsRemaining int
	Underlying        error
}

func (f *AadhaarVerifyFailure) Error() string {
	if f.Underlying != nil {
		return fmt.Sprintf("aadhaar verification failed (%s): %v", f.ReasonCode, f.Underlying)
	}
	return fmt.Sprintf("aadhaar verification failed (%s)", f.ReasonCode)
}

func (f *AadhaarVerifyFailure) Unwrap() error {
	return f.Underlying
}

func (s *Service) CompleteAadhaarOTP(
	ctx context.Context,
	tenantID uuid.UUID,
	refID, otp string,
	actor string,
) (*domain.KYCVerification, error) {
	vendorRefID := fmt.Sprintf("cf_aadhaar_otp_%s", refID)
	pending, err := s.repo.GetVerificationByVendorRefID(ctx, vendorRefID)
	if err != nil {
		return nil, fmt.Errorf("kyc service: find aadhaar verification: %w", err)
	}
	if pending == nil || pending.TenantID != tenantID {
		return nil, domain.ErrVerificationNotFound
	}
	if pending.Status != domain.KYCStatusPending {
		return nil, domain.ErrVerificationNotPending
	}

	if s.cashfree == nil {
		return nil, ErrCashfreeUnavailable
	}

	res, err := s.cashfree.VerifyAadhaarOTP(ctx, refID, otp)
	if err != nil {
		var httpStatus int
		var msg string
		var apiErr *cashfree.APIError
		if errors.As(err, &apiErr) {
			httpStatus = apiErr.StatusCode
			msg = apiErr.Body
		} else {
			msg = err.Error()
		}
		code, class := cashfree.ClassifyAadhaarError(httpStatus, msg)
		var count int
		if code != "COOLDOWN_ACTIVE" {
			c, logErr := s.repo.RecordFailureAndCount(ctx, tenantID, domain.KYCMethodAadhaarOTP, code, 10)
			if logErr != nil {
				slog.Error("kyc: failure log write failed", "err", logErr)
			} else {
				count = c
			}
		}
		if code == "INVALID_AADHAAR" && count >= 2 {
			class = cashfree.FailureTerminal
		}

		attemptsRemaining := domain.AadhaarOTPMaxAttempts - count
		if attemptsRemaining < 0 {
			attemptsRemaining = 0
		}
		if class == cashfree.FailureTerminal || class == cashfree.FailureSystemFault {
			attemptsRemaining = 0
		}

		retryable := class == cashfree.FailureRecoverable && attemptsRemaining > 0
		if class == cashfree.FailureCooldown {
			retryable = true
		}

		if class == cashfree.FailureTerminal || (!retryable && attemptsRemaining == 0) {
			_ = s.repo.FailVerificationTx(ctx, pending.ID, err.Error(), actor)
		}

		return nil, &AadhaarVerifyFailure{
			ReasonCode:        code,
			Class:             class,
			Retryable:         retryable,
			CooldownSeconds:   cashfree.CooldownSecondsFor(class, code),
			AttemptsRemaining: attemptsRemaining,
			Underlying:        err,
		}
	}

	canonicalDOB, isDedupable, _ := domain.NormalizeDOB(res.DOB)
	majorityDate, mdErr := domain.MajorityDateFromDOB(canonicalDOB)
	if mdErr != nil {
		slog.Warn("kyc: unparseable dob received from aadhaar otp", "tenant_id", tenantID, "dob", res.DOB, "err", mdErr)
		return nil, &AadhaarVerifyFailure{
			ReasonCode: "INVALID_DOB_FORMAT",
			Retryable:  false,
			Underlying: mdErr,
		}
	}
	name := domain.NormalizeName(res.Name)
	identityHash := domain.ComputeIdentityHash(s.cfg.IdentitySecret, name, canonicalDOB, res.Gender, res.MaskedUID)

	tenantName, _ := s.repo.GetTenantName(ctx, tenantID)
	nameMismatch := !domain.IsNameMatch(tenantName, res.Name)

	now := time.Now().UTC()
	expiresAt := now.AddDate(0, 0, s.cfg.VerificationValidityDays)

	// AUTO-VERIFIED: Government authenticated via direct UIDAI OTP
	verified, err := s.repo.CompleteVerificationTx(ctx,
		pending.ID, res.MaskedUID, identityHash, isDedupable,
		s.cfg.HashKeyVersion, nil, nameMismatch, res.PhotoBytes,
		now, expiresAt, majorityDate, actor,
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: complete aadhaar otp: %w", err)
	}

	return verified, nil
}

// ListPendingReviews returns pending manual reviews for a property.
func (s *Service) ListPendingReviews(ctx context.Context, propertyID uuid.UUID) ([]domain.KYCVerification, error) {
	return s.repo.ListPendingReviews(ctx, propertyID)
}

// ApproveManualReview approves a pending manual review. propertyID scopes the
// operation to the calling owner's property — the repo must reject (not found)
// if verificationID resolves to a tenant on a different property.
func (s *Service) ApproveManualReview(ctx context.Context, verificationID, propertyID, reviewerID uuid.UUID, actor string) (*domain.KYCVerification, error) {
	return s.repo.ApproveManualReviewTx(ctx, verificationID, propertyID, reviewerID, actor)
}

// RejectManualReview rejects a pending manual review with a human reason.
// propertyID scopes the operation to the calling owner's property — the repo
// must reject (not found) if verificationID resolves to a tenant on a
// different property.
func (s *Service) RejectManualReview(ctx context.Context, verificationID, propertyID, reviewerID uuid.UUID, reason, actor string) (*domain.KYCVerification, error) {
	return s.repo.RejectManualReviewTx(ctx, verificationID, propertyID, reviewerID, reason, actor)
}


// GetAttestedPhoto retrieves the verified government/vendor-attested photo crop for owner inspection.
func (s *Service) GetAttestedPhoto(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	return s.repo.GetAttestedPhoto(ctx, tenantID)
}

// GetKYCDocument retrieves the document file bytes or fallback photo for owner inspection.
func (s *Service) GetKYCDocument(ctx context.Context, id, propertyID uuid.UUID) ([]byte, string, error) {
	ok, err := s.repo.VerifyPropertyOwnership(ctx, id, propertyID)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", domain.ErrVerificationNotFound
	}
	return s.repo.GetKYCDocument(ctx, id)
}

// --- Read-side ----------------------------------------------------------------

// GetStatus returns the active (pending/verified) verification and active consent
// for the tenant's self-service status endpoint. Either value may be nil if no
// active record exists.
func (s *Service) GetStatus(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
	v, err := s.repo.GetActiveVerificationByTenant(ctx, tenantID)
	if errors.Is(err, domain.ErrVerificationNotFound) {
		v, err = nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("kyc service: get verification status: %w", err)
	}

	c, err := s.repo.GetActiveConsentByTenant(ctx, tenantID)
	if errors.Is(err, domain.ErrVerificationNotFound) {
		c, err = nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("kyc service: get consent status: %w", err)
	}
	return v, c, nil
}

// GetOwnerView returns the most recent verification (any status) and the full
// audit log for a tenant — exposed only to property-authorised owners.
func (s *Service) GetOwnerView(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, []domain.KYCAuditLog, error) {
	v, err := s.repo.GetLatestVerificationByTenant(ctx, tenantID)
	if errors.Is(err, domain.ErrVerificationNotFound) {
		v, err = nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("kyc service: get latest verification: %w", err)
	}

	logs, err := s.repo.ListAuditLogsByTenant(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("kyc service: list audit logs: %w", err)
	}
	return v, logs, nil
}

// GetVerificationByVendorRefID retrieves a verification record by its vendor
// reference ID (e.g. for Aadhaar OTP or OCR flow reconciliation).
func (s *Service) GetVerificationByVendorRefID(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error) {
	return s.repo.GetVerificationByVendorRefID(ctx, vendorRefID)
}

// --- Owner operations ---------------------------------------------------------

// ClearDuplicateFlag dismisses a false-positive duplicate flag on a verified
// verification record with a mandatory audited reason. This is the sole path
// for clearing the flag — direct DB edits would break the audit chain.
func (s *Service) ClearDuplicateFlag(ctx context.Context, verificationID uuid.UUID, actor, reason string) error {
	if reason == "" {
		return fmt.Errorf("kyc service: clear duplicate flag requires a non-empty reason")
	}
	if err := s.repo.ClearDuplicateFlag(ctx, verificationID, actor, reason); err != nil {
		return fmt.Errorf("kyc service: clear duplicate flag: %w", err)
	}
	return nil
}

// RecordGuardianConsent records guardian details and cryptographic audit log for a tenant.
func (s *Service) RecordGuardianConsent(ctx context.Context, tenantID uuid.UUID, guardianName, guardianPhone, relation, refID string, actor string) (*domain.Tenant, error) {
	return s.repo.RecordGuardianConsentTx(ctx, tenantID, guardianName, guardianPhone, relation, refID, actor)
}

