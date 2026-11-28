// Package kyc orchestrates the Aadhaar identity verification lifecycle:
// consent recording, UIDAI Aadhaar OTP verification, Cashfree Smart OCR document
// verification, offline Secure QR verification, DPDP consent revocation, and
// duplicate-flag clearance.
//
// ADR-004 invariants enforced here:
//   - A durable pending intent is committed before billable external calls; DB transactions and locks are not held across provider I/O.
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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	ErrInvalidAadhaarNumber  = errors.New("kyc: invalid aadhaar number — must be 12 digits and satisfy Verhoeff checksum")
	ErrInvalidConsentPurpose = errors.New("kyc: invalid or unsupported consent purpose")
	ErrAadhaarOTPDisabled    = errors.New("kyc: aadhaar otp is disabled under ADR-005")
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
	CompleteVerificationTx(ctx context.Context, verificationID uuid.UUID, maskedUID, identityHash string, isDedupable bool, hashKeyVersion int16, qrStatus *string, nameMismatch bool, attestedPhotoBytes []byte, verifiedAt, expiresAt time.Time, majorityDate *time.Time, actor string, leaseToken ...uuid.UUID) (*domain.KYCVerification, error)
	FailVerificationTx(ctx context.Context, verificationID uuid.UUID, reason, actor string, leaseToken ...uuid.UUID) error
	MarkRequestUnknownTx(ctx context.Context, verificationID uuid.UUID, reason, actor string, leaseToken ...uuid.UUID) error

	GetActiveVerificationByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, error)
	GetVerificationByVendorRefID(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error)
	GetVerificationByCFReferenceID(ctx context.Context, cfRefID string) (*domain.KYCVerification, error)
	UpdateVendorRefIDTx(ctx context.Context, verificationID uuid.UUID, vendorRefID string) error
	UpdateCFReferenceIDTx(ctx context.Context, verificationID uuid.UUID, cfRefID string) error
	GetLatestVerificationByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, error)
	ClearDuplicateFlag(ctx context.Context, verificationID uuid.UUID, actor, reason string) error

	ListAuditLogsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.KYCAuditLog, error)

	TryAcquireInFlightLock(ctx context.Context, tenantID uuid.UUID, ttl time.Duration) (bool, error)
	ReleaseInFlightLock(ctx context.Context, tenantID uuid.UUID) error
	GetTenantName(ctx context.Context, tenantID uuid.UUID) (string, error)
	GetAttestedPhoto(ctx context.Context, tenantID uuid.UUID) ([]byte, error)

	SubmitManualReviewTx(ctx context.Context, verificationID uuid.UUID, maskedUID, identityHash string, ocrData, documentURL string, documentBytes []byte, documentContentType string, actor string, documentType string, leaseToken ...uuid.UUID) (*domain.KYCVerification, error)
	ApproveManualReviewTx(ctx context.Context, verificationID, propertyID, reviewerID uuid.UUID, actor string) (*domain.KYCVerification, error)
	RejectManualReviewTx(ctx context.Context, verificationID, propertyID, reviewerID uuid.UUID, reason, actor string) (*domain.KYCVerification, error)
	ListPendingReviews(ctx context.Context, propertyID uuid.UUID) ([]domain.KYCVerification, error)
	GetVerificationByID(ctx context.Context, id uuid.UUID) (*domain.KYCVerification, error)
	GetKYCDocument(ctx context.Context, id uuid.UUID) ([]byte, string, error)
	VerifyPropertyOwnership(ctx context.Context, verificationID, propertyID uuid.UUID) (bool, error)
	RecordFailureAndCount(ctx context.Context, tenantID uuid.UUID, method domain.KYCMethod, reasonCode string, windowMinutes int) (int, error)
	CountAndRecordOTPAttemptTx(ctx context.Context, tenantID uuid.UUID, maxAllowed int, window time.Duration) (int, error)
	RefundOTPAttemptTx(ctx context.Context, tenantID uuid.UUID) error
	RecordGuardianConsentTx(ctx context.Context, tenantID uuid.UUID, guardianName, guardianPhone, relation, refID string, actor string) (*domain.Tenant, error)
}

// CashfreeKYCClient is the Cashfree dependency for document OCR, DigiLocker, and Aadhaar OTP.
// Implemented by *cashfree.Client via CashfreeAdapter.
type CashfreeKYCClient interface {
	UploadDocument(ctx context.Context, fileReader io.Reader, filename string, documentType string, verificationID ...string) (*cashfree.SmartOCRResponse, error)
	UploadAadhaarDocument(ctx context.Context, fileReader io.Reader, filename string) (*cashfree.SmartOCRResponse, error)
	RequestAadhaarOTP(ctx context.Context, aadhaarNumber string) (*cashfree.AadhaarOTPInitResult, error)
	VerifyAadhaarOTP(ctx context.Context, refID, otp string) (*cashfree.AadhaarOTPVerifyResult, error)
	CreateDigiLockerURL(ctx context.Context, in cashfree.CreateDigiLockerURLInput) (*cashfree.DigiLockerSession, error)
	GetDigiLockerStatus(ctx context.Context, verificationID string) (*cashfree.DigiLockerStatusResult, error)
	GetDigiLockerAadhaar(ctx context.Context, verificationID string) (*cashfree.DigiLockerAadhaar, error)
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

	// DigiLockerVerifyAccountPreflight enables the optional Cashfree POST /digilocker/verify-account
	// preflight check when user_flow == "signup" (default false until sandbox confirmed).
	DigiLockerVerifyAccountPreflight bool
}

type digiLockerAccountVerifier interface {
	VerifyDigiLockerAccount(ctx context.Context, in cashfree.VerifyDigiLockerAccountInput) (*cashfree.DigiLockerAccountResult, error)
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

// CashfreeClient returns the underlying CashfreeKYCClient.
func (s *Service) CashfreeClient() CashfreeKYCClient {
	return s.cashfree
}

// --- Consent ------------------------------------------------------------------

func (s *Service) acquireLease(ctx context.Context, tenantID uuid.UUID, ttl time.Duration) (uuid.UUID, bool, error) {
	leaseToken := uuid.New()
	if fenced, ok := s.repo.(interface {
		TryAcquireInFlightLockWithToken(ctx context.Context, tenantID, leaseToken uuid.UUID, ttl time.Duration) (bool, error)
	}); ok {
		locked, err := fenced.TryAcquireInFlightLockWithToken(ctx, tenantID, leaseToken, ttl)
		return leaseToken, locked, err
	}
	locked, err := s.repo.TryAcquireInFlightLock(ctx, tenantID, ttl)
	return leaseToken, locked, err
}

func (s *Service) releaseLease(ctx context.Context, tenantID, leaseToken uuid.UUID) {
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if fenced, ok := s.repo.(interface {
		ReleaseInFlightLockWithToken(ctx context.Context, tenantID, leaseToken uuid.UUID) error
	}); ok {
		_ = fenced.ReleaseInFlightLockWithToken(relCtx, tenantID, leaseToken)
		return
	}
	_ = s.repo.ReleaseInFlightLock(relCtx, tenantID)
}

// RecordConsent persists the tenant's explicit DPDP consent before any
// verification can proceed. Idempotent: if active consent exists for this version,
// it returns the existing consent record without creating duplicates.
func (s *Service) RecordConsent(
	ctx context.Context,
	tenantID uuid.UUID,
	purpose, consentVersion, consentText,
	ip, userAgent, actor string,
) (*domain.KYCConsent, error) {
	// Single source of truth: AllowedConsentPurposes (consent.go), mirrored by the
	// kyc_consent_purpose_check constraint in migration 110.
	if err := ValidateConsentPurpose(purpose); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConsentPurpose, err)
	}

	if consentText == "" {
		consentText = "I agree to my identity details being verified and stored for PG occupancy records in compliance with the DPDP Act."
	}

	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s:%s", consentVersion, consentText)))
	hash := hex.EncodeToString(h.Sum(nil))

	if existing, err := s.repo.GetActiveConsentByTenant(ctx, tenantID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, domain.ErrVerificationNotFound) {
			return nil, fmt.Errorf("kyc service: check active consent: %w", err)
		}
	} else if existing != nil {
		if existing.ConsentVersion == consentVersion && existing.Purpose == purpose && existing.ConsentTextHash == hash && existing.IsActive() {
			return existing, nil
		}
	}

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
// Consent gate: enforced — same invariant as Aadhaar OTP initiation.
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

	// 2. Reject non-numeric payloads immediately — they cannot be signed Secure QR.
	if !aadhaar.LooksLikeSecureQR(rawQR) {
		return nil, ErrQRSignatureInvalid
	}

	// 3. In-process RSA-SHA256 verification (no network call).
	data, partial, err := aadhaar.DecodeAadhaarQR(rawQR)
	if err != nil {
		// err is non-nil only for signature failures on Secure QR.
		return nil, ErrQRSignatureInvalid
	}
	// AUTH: reject any payload that did not pass UIDAI RSA-SHA256 verification.
	// decodeXMLLike and decodeOpaque both return Verified=false; only decodeSecureQR sets Verified=true.
	if !data.Verified {
		return nil, ErrQRSignatureInvalid
	}
	if partial || data.Name == "" || data.UIDLast4 == "" {
		return nil, ErrQRDataIncomplete
	}
	if data.DOB == "" && data.YOB == "" {
		return nil, ErrQRDataIncomplete
	}
	if data.Gender == "" {
		return nil, ErrQRDataIncomplete
	}

	// 4. Fetch tenant's submitted name and evaluate name match
	tenantName, err := s.repo.GetTenantName(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("kyc service: fetch tenant name for qr: %w", err)
	}
	nameMismatch := !domain.IsNameMatch(tenantName, data.Name)

	// 5. Normalise and compute identity hash.
	dobStr := data.DOB
	if dobStr == "" {
		dobStr = data.YOB // year-only fallback
	}
	canonicalDOB, isDedupable, _ := domain.NormalizeDOB(dobStr)
	name := domain.NormalizeName(data.Name)
	var identityHash string
	if isDedupable {
		identityHash = domain.ComputeIdentityHash(s.cfg.IdentitySecret, name, canonicalDOB, data.Gender, data.UIDLast4)
	}

	// 6. Pre-check active verification status under tenant scope
	active, err := s.repo.GetActiveVerificationByTenant(ctx, tenantID)
	if err != nil && !errors.Is(err, domain.ErrVerificationNotFound) {
		return nil, fmt.Errorf("kyc service: check active verification for qr: %w", err)
	}
	if active != nil {
		if active.Status == domain.KYCStatusVerified {
			if active.ExpiresAt == nil || time.Now().UTC().Before(*active.ExpiresAt) {
				return nil, domain.ErrAlreadyVerified
			}
		} else if active.Status == domain.KYCStatusPending ||
			active.Status == domain.KYCStatusPendingManualReview ||
			active.Status == domain.KYCStatusRequestUnknown {
			return nil, domain.ErrVerificationInProgress
		}
	}

	// 7. Initiate pending verification
	vendorRefID := uuid.New().String()
	pending, err := s.repo.InitiateVerificationTx(ctx,
		tenantID, consent.ID,
		domain.KYCMethodQR,
		"uidai_qr", vendorRefID, actor,
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: initiate qr verification: %w", err)
	}

	// 8. Route to manual review:
	// A standalone Secure QR verification must never auto-verify based on cryptographic signature
	// and name matching alone, as anyone with a photo of another person's QR could pass.
	// Both clean matches and name mismatches are routed to pending_manual_review via SubmitManualReviewTx.
	// Only verified tenants can pay dues (KYC verification is mandatory platform-wide).
	// Final verification occurs when the owner/manager approves the review (ApproveManualReviewTx),
	// which auto-purges biometric document/photo bytes per DPDP Act 2023.
	qrStatus := domain.QRStatusSecure
	contentType := "image/jp2"
	if len(data.PhotoBytes) >= 3 && data.PhotoBytes[0] == 0xFF && data.PhotoBytes[1] == 0xD8 && data.PhotoBytes[2] == 0xFF {
		contentType = "image/jpeg"
	}

	qrDataJSON, _ := json.Marshal(map[string]any{
		"name":          data.Name,
		"dob":           dobStr,
		"gender":        data.Gender,
		"masked_uid":    data.UIDLast4,
		"qr_status":     qrStatus,
		"name_mismatch": nameMismatch,
		"has_photo":     len(data.PhotoBytes) > 0,
	})

	underReview, err := s.repo.SubmitManualReviewTx(ctx,
		pending.ID, data.UIDLast4, identityHash,
		string(qrDataJSON), "", data.PhotoBytes, contentType, actor, "aadhaar",
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: submit qr for manual review: %w", err)
	}
	return underReview, nil
}

// VerifyDocument executes the document-upload KYC flow using Cashfree Smart OCR (/bharat-ocr).
// Supports voluntary document types (Aadhaar, PAN, Passport, Voter ID, Driving Licence) per Aadhaar Act.
//
// Invariants enforced:
//   - Network call (Cashfree multipart upload) executes outside database transaction.
//   - Distributed lease (kyc_in_flight_lock) prevents double-billing across horizontal Cloud Run instances.
//   - Zero raw Aadhaar UID persistence: extracted demographic fields are ephemeral inputs to ComputeIdentityHash; only masked UID is stored.
//   - In manual review, demographic OCR data is stored encrypted and wiped upon review decision (approval, rejection, expiry, or revocation).
//   - Tampered documents enter a flagged pending_manual_review state and never auto-verify.
func (s *Service) VerifyDocument(
	ctx context.Context,
	tenantID uuid.UUID,
	fileReader io.Reader,
	filename, contentType, documentType, actor string,
) (*domain.KYCVerification, error) {
	normDocType := string(domain.NormalizeDocumentType(documentType))

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

	// 2. Acquire distributed in-flight lease FIRST (60s TTL, fenced multi-instance guard)
	leaseToken, locked, err := s.acquireLease(ctx, tenantID, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("kyc service: acquire in-flight lock: %w", err)
	}
	if !locked {
		return nil, domain.ErrVerificationInProgress
	}
	defer func() {
		s.releaseLease(ctx, tenantID, leaseToken)
	}()

	// 3. Under lease lock: pre-check active verification status
	active, err := s.repo.GetActiveVerificationByTenant(ctx, tenantID)
	if err != nil && !errors.Is(err, domain.ErrVerificationNotFound) {
		return nil, fmt.Errorf("kyc service: check active verification: %w", err)
	}
	if active != nil {
		if active.Status == domain.KYCStatusVerified {
			if active.ExpiresAt == nil || time.Now().UTC().Before(*active.ExpiresAt) {
				return nil, domain.ErrAlreadyVerified
			}
		} else if active.Status == domain.KYCStatusPending ||
			active.Status == domain.KYCStatusPendingManualReview ||
			active.Status == domain.KYCStatusRequestUnknown {
			return nil, domain.ErrVerificationInProgress
		}
	}

	// 4. Create durable pending verification record BEFORE dispatching billable Smart OCR call.
	// This prevents orphaning a billed operation if a local DB failure occurs after provider success.
	tenantName, err := s.repo.GetTenantName(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("kyc service: fetch tenant name: %w", err)
	}

	correlationID := fmt.Sprintf("cf_ocr_%s", strings.ReplaceAll(uuid.New().String(), "-", "")[:24])
	pending, err := s.repo.InitiateVerificationTx(ctx,
		tenantID, consent.ID,
		domain.KYCMethodOCR,
		"cashfree_smart_ocr", correlationID, actor,
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: initiate ocr verification intent: %w", err)
	}

	var resp *cashfree.SmartOCRResponse
	if s.cashfree != nil {
		resp, err = s.cashfree.UploadDocument(ctx, bytes.NewReader(fileBytes), filename, normDocType, correlationID)
		if err != nil {
			if isAmbiguousKYCError(err) {
				_ = s.repo.MarkRequestUnknownTx(ctx, pending.ID, err.Error(), actor)
				pending.Status = domain.KYCStatusRequestUnknown
				return pending, fmt.Errorf("kyc service: upload document ambiguous state: %w", err)
			}
			_ = s.repo.FailVerificationTx(ctx, pending.ID, "OCR_UPLOAD_FAILED", actor)
			return nil, fmt.Errorf("kyc service: upload document: %w", err)
		}
	}

	var vendorRefID string
	var ocrBytes []byte
	var maskedUID string
	var identityHash string

	if resp == nil || resp.Name == "" || resp.MaskedUID == "" {
		syntheticID := uuid.New().String()
		vendorRefID = fmt.Sprintf("cf_ocr_manual_%s", syntheticID)
		maskedUID = "XXXX"
		if resp != nil && resp.MaskedUID != "" {
			maskedUID = resp.MaskedUID
		}
		nameMismatch := true
		name := domain.NormalizeName(tenantName)
		identityHash = domain.ComputeIdentityHash(s.cfg.IdentitySecret, name, "", "", maskedUID)
		ocrBytes, _ = json.Marshal(map[string]any{
			"document_type": normDocType,
			"name":          tenantName,
			"masked_uid":    maskedUID,
			"name_mismatch": nameMismatch,
			"ocr_status":    "UNAVAILABLE",
		})
	} else {
		// Log allowlisted metadata ONLY; zero full UID or raw body logged
		slog.InfoContext(ctx, "cashfree smart ocr completed",
			"tenant_id", tenantID,
			"cashfree_reference_id", resp.ReferenceID,
			"document_type", normDocType,
			"vendor_status", resp.Status,
			"masked_uid", resp.MaskedUID,
			"is_tampered", resp.IsTampered,
			"has_photo", len(resp.PhotoBytes) > 0,
		)
		vendorRefID = fmt.Sprintf("cf_ocr_%v", resp.ReferenceID)
		nameMismatch := !domain.IsNameMatch(tenantName, resp.Name)
		if resp.IsTampered {
			nameMismatch = true
		}
		canonicalDOB, isDedupable, _ := domain.NormalizeDOB(resp.DOB)
		name := domain.NormalizeName(resp.Name)
		if isDedupable {
			identityHash = domain.ComputeIdentityHash(s.cfg.IdentitySecret, name, canonicalDOB, resp.Gender, resp.MaskedUID)
		}
		maskedUID = resp.MaskedUID
		// Clear temporary OCR identity strings (name, dob, gender) immediately after comparison
		// and hash derivation so raw PII is not persisted in v.OCRData.
		resp.Name = ""
		resp.DOB = ""
		resp.Gender = ""
		ocrBytes, _ = json.Marshal(map[string]any{
			"document_type":        normDocType,
			"dob_exact":            isDedupable,
			"masked_uid":           resp.MaskedUID,
			"qr_status":            resp.QRStatus,
			"name_mismatch":        nameMismatch,
			"is_tampered":          resp.IsTampered,
			"tamper_reason":        resp.TamperReason,
			"image_quality_passed": resp.ImageQualityPassed,
			"quality_issues":       resp.QualityIssues,
		})
	}

	var updateRefErr error
	for attempt := 0; attempt < 3; attempt++ {
		updateRefErr = s.repo.UpdateVendorRefIDTx(ctx, pending.ID, vendorRefID)
		if updateRefErr == nil {
			break
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
		}
	}
	if updateRefErr != nil {
		slog.ErrorContext(ctx, "kyc service: failed to persist vendor_ref_id after retries; preserving request_unknown",
			"verification_id", pending.ID,
			"vendor_ref_id", vendorRefID,
			"err", updateRefErr,
		)
		_ = s.repo.MarkRequestUnknownTx(ctx, pending.ID, "VENDOR_REF_PERSISTENCE_FAILED: "+updateRefErr.Error(), actor, leaseToken)
		pending.Status = domain.KYCStatusRequestUnknown
		return pending, fmt.Errorf("kyc service: persist vendor ref id: %w", updateRefErr)
	}

	docURL := fmt.Sprintf("/api/owner/kyc/%s/document", pending.ID)
	underReview, err := s.repo.SubmitManualReviewTx(ctx,
		pending.ID, maskedUID, identityHash,
		string(ocrBytes), docURL, fileBytes, contentType, actor,
		normDocType, leaseToken,
	)
	if err != nil {
		slog.ErrorContext(ctx, "kyc service: failed to submit manual review after provider success; preserving request_unknown",
			"verification_id", pending.ID,
			"vendor_ref_id", vendorRefID,
			"err", err,
		)
		_ = s.repo.MarkRequestUnknownTx(ctx, pending.ID, "MANUAL_REVIEW_PERSISTENCE_FAILED: "+err.Error(), actor, leaseToken)
		pending.Status = domain.KYCStatusRequestUnknown
		return pending, fmt.Errorf("kyc service: submit manual review: %w", err)
	}

	return underReview, nil
}

// VerifyAadhaarDocument executes the single document-upload KYC flow using Cashfree Smart OCR (/bharat-ocr).
// Deprecated: use VerifyDocument to support non-Aadhaar identification per the Aadhaar Act.
func (s *Service) VerifyAadhaarDocument(
	ctx context.Context,
	tenantID uuid.UUID,
	fileReader io.Reader,
	filename, contentType, actor string,
) (*domain.KYCVerification, error) {
	return s.VerifyDocument(ctx, tenantID, fileReader, filename, contentType, "aadhaar", actor)
}

// InitiateAadhaarOTP requests an Aadhaar OTP from UIDAI via Cashfree for a tenant.
func (s *Service) InitiateAadhaarOTP(
	ctx context.Context,
	tenantID uuid.UUID,
	aadhaarNumber string,
	actor string,
) (string, error) {
	cleanedNum := strings.ReplaceAll(aadhaarNumber, " ", "")
	cleanedNum = strings.ReplaceAll(cleanedNum, "-", "")
	if err := aadhaar.ValidateAadhaarNumber(cleanedNum); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidAadhaarNumber, err)
	}

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

	leaseToken, locked, err := s.acquireLease(ctx, tenantID, 60*time.Second)
	if err != nil {
		return "", fmt.Errorf("kyc service: acquire in-flight lock: %w", err)
	}
	if !locked {
		return "", domain.ErrVerificationInProgress
	}
	defer func() {
		s.releaseLease(ctx, tenantID, leaseToken)
	}()

	if s.cashfree == nil {
		return "", ErrCashfreeUnavailable
	}

	// KYC-1: Under the distributed lock, pre-check active verification status before
	// issuing a billed UIDAI OTP request. The 60s in-flight lock alone is a concurrent-request
	// guard; this check is the sequential-request guard. Without it, a tenant who is already
	// verified (or has a pending OTP from >60s ago once the lock expires) can re-trigger
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
		} else if active.Status == domain.KYCStatusPending ||
			active.Status == domain.KYCStatusPendingManualReview ||
			active.Status == domain.KYCStatusRequestUnknown {
			return "", domain.ErrVerificationInProgress
		}
	}

	// Gate on daily OTP attempt limit before making the billed UIDAI call.
	_, err = s.repo.CountAndRecordOTPAttemptTx(ctx, tenantID, domain.MaxDailyAadhaarOTPRequests, 24*time.Hour)
	if err != nil {
		if errors.Is(err, domain.ErrDailyOTPLimitExceeded) {
			return "", err
		}
		return "", fmt.Errorf("kyc service: check daily otp quota: %w", err)
	}

	// Create durable pending verification record BEFORE dispatching billable UIDAI call to Cashfree.
	// This prevents orphaning a billed OTP if a local DB failure occurs after provider success.
	intentRefID := fmt.Sprintf("intent_aadhaar_otp_%s", uuid.New().String())
	var pending *domain.KYCVerification
	for attempt := 0; attempt < 3; attempt++ {
		pending, err = s.repo.InitiateVerificationTx(ctx,
			tenantID, consent.ID,
			domain.KYCMethodAadhaarOTP,
			"cashfree_aadhaar_otp", intentRefID, actor,
		)
		if err == nil {
			break
		}
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		_ = s.repo.RefundOTPAttemptTx(ctx, tenantID)
		return "", fmt.Errorf("kyc service: create durable pending verification: %w", err)
	}

	res, err := s.cashfree.RequestAadhaarOTP(ctx, cleanedNum)
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

		if isAmbiguousKYCError(err) || httpStatus >= 500 {
			slog.ErrorContext(ctx, "kyc service: ambiguous provider outcome during aadhaar otp initiation; preserving request_unknown",
				"tenant_id", tenantID,
				"verification_id", pending.ID,
				"http_status", httpStatus,
				"err", err,
			)
			_ = s.repo.RefundOTPAttemptTx(ctx, tenantID)
			_ = s.repo.MarkRequestUnknownTx(ctx, pending.ID, "AMBIGUOUS_PROVIDER_OUTCOME: "+msg, actor)
			pending.Status = domain.KYCStatusRequestUnknown
			return "", &AadhaarVerifyFailure{
				ReasonCode:        "REQUEST_UNKNOWN",
				Class:             cashfree.FailureSystemFault,
				Retryable:         false,
				AttemptsRemaining: 0,
				Underlying:        err,
			}
		}

		fail := s.classifyAndRecordAadhaarFailure(ctx, tenantID, &pending.ID, httpStatus, msg, actor, err)
		if fail.Class == cashfree.FailureSystemFault {
			_ = s.repo.RefundOTPAttemptTx(ctx, tenantID)
		}
		_ = s.repo.FailVerificationTx(ctx, pending.ID, fail.ReasonCode, actor, leaseToken)
		return "", fail
	}

	// Validate Cashfree response: require status SUCCESS and a non-empty ref_id.
	// Cashfree documents a 200 response whose message says the Aadhaar is not linked to a mobile
	// number. No OTP is sent in that case, so never open an OTP-entry session for it.
	isSuccess := res != nil &&
		strings.EqualFold(res.Status, "SUCCESS") &&
		strings.TrimSpace(res.RefID) != "" &&
		!strings.Contains(strings.ToLower(res.Message), "not linked")

	if !isSuccess {
		statusStr := "UNKNOWN"
		msg := ""
		if res != nil {
			statusStr = res.Status
			msg = res.Message
			if msg == "" {
				msg = res.Status
			}
		}
		underlying := fmt.Errorf("aadhaar otp request failed: status=%s, message=%s", statusStr, msg)
		fail := s.classifyAndRecordAadhaarFailure(ctx, tenantID, &pending.ID, 200, msg, actor, underlying)
		if fail.Class == cashfree.FailureSystemFault {
			_ = s.repo.RefundOTPAttemptTx(ctx, tenantID)
		}
		_ = s.repo.FailVerificationTx(ctx, pending.ID, fail.ReasonCode, actor, leaseToken)
		return "", fail
	}

	vendorRefID := fmt.Sprintf("cf_aadhaar_otp_%s", res.RefID)

	var updateErr error
	for attempt := 0; attempt < 3; attempt++ {
		updateErr = s.repo.UpdateVendorRefIDTx(ctx, pending.ID, vendorRefID)
		if updateErr == nil {
			break
		}
		slog.Warn("kyc: UpdateVendorRefIDTx failed, retrying",
			"tenant_id", tenantID,
			"vendor_ref_id", vendorRefID,
			"attempt", attempt+1,
			"err", updateErr,
		)
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	if updateErr != nil {
		slog.Error("CRITICAL: failed to update vendor reference id after Cashfree success; preserving request_unknown",
			"tenant_id", tenantID,
			"ref_id", res.RefID,
			"vendor_ref_id", vendorRefID,
			"err", updateErr,
		)
		_ = s.repo.MarkRequestUnknownTx(ctx, pending.ID, "VENDOR_REF_PERSISTENCE_FAILED: "+updateErr.Error(), actor)
		pending.Status = domain.KYCStatusRequestUnknown
		return "", fmt.Errorf("kyc service: update vendor ref id: %w", updateErr)
	}

	return res.RefID, nil
}

func (s *Service) classifyAndRecordAadhaarFailure(
	ctx context.Context,
	tenantID uuid.UUID,
	verificationID *uuid.UUID,
	httpStatus int,
	msg string,
	actor string,
	underlying error,
) *AadhaarVerifyFailure {
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

	if verificationID != nil && (class == cashfree.FailureTerminal || (!retryable && attemptsRemaining == 0)) {
		_ = s.repo.FailVerificationTx(ctx, *verificationID, underlying.Error(), actor)
	}

	return &AadhaarVerifyFailure{
		ReasonCode:        code,
		Class:             class,
		Retryable:         retryable,
		CooldownSeconds:   cashfree.CooldownSecondsFor(class, code),
		AttemptsRemaining: attemptsRemaining,
		Underlying:        underlying,
	}
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
	// ADR-005: Aadhaar OTP is disabled platform-wide; fail closed even on direct service calls.
	return nil, ErrAadhaarOTPDisabled
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
	// Authorization must not depend on the expiry job running on schedule.
	if v != nil && v.Status == domain.KYCStatusVerified && !v.IsValid(time.Now().UTC()) {
		expired := *v
		expired.Status = domain.KYCStatusExpired
		v = &expired
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
// KYC-07: Both OTP and manual evidence submissions enter pending manual review.
func (s *Service) RecordGuardianConsent(ctx context.Context, tenantID uuid.UUID, guardianName, guardianPhone, relation, refID string, actor string) (*domain.Tenant, error) {
	return s.repo.RecordGuardianConsentTx(ctx, tenantID, guardianName, guardianPhone, relation, refID, actor)
}

type guardianConsentReviewRepo interface {
	ApproveGuardianConsentTx(ctx context.Context, tenantID, propertyID, reviewerID uuid.UUID, actor string) (*domain.Tenant, error)
	RejectGuardianConsentTx(ctx context.Context, tenantID, propertyID, reviewerID uuid.UUID, reason, actor string) (*domain.Tenant, error)
}

// ApproveGuardianConsent marks a minor's pending guardian consent submission as verified after manual review.
func (s *Service) ApproveGuardianConsent(ctx context.Context, tenantID, propertyID, reviewerID uuid.UUID, actor string) (*domain.Tenant, error) {
	r, ok := s.repo.(guardianConsentReviewRepo)
	if !ok {
		return nil, errors.New("kyc service: repository does not support guardian consent approval")
	}
	return r.ApproveGuardianConsentTx(ctx, tenantID, propertyID, reviewerID, actor)
}

// RejectGuardianConsent rejects a minor's pending guardian consent submission.
func (s *Service) RejectGuardianConsent(ctx context.Context, tenantID, propertyID, reviewerID uuid.UUID, reason, actor string) (*domain.Tenant, error) {
	r, ok := s.repo.(guardianConsentReviewRepo)
	if !ok {
		return nil, errors.New("kyc service: repository does not support guardian consent rejection")
	}
	return r.RejectGuardianConsentTx(ctx, tenantID, propertyID, reviewerID, reason, actor)
}

// InitiateDigiLockerVerification begins a Cashfree Secure ID DigiLocker consent flow.
func (s *Service) InitiateDigiLockerVerification(ctx context.Context, tenantID uuid.UUID, redirectURL, userFlow, actor string) (*domain.KYCVerification, string, error) {
	if s.cashfree == nil {
		return nil, "", errors.New("kyc service: cashfree client unconfigured")
	}
	consent, err := s.repo.GetActiveConsentByTenant(ctx, tenantID)
	if err != nil || consent == nil || !consent.IsActive() {
		return nil, "", ErrNoActiveConsent
	}

	leaseToken, acquired, err := s.acquireLease(ctx, tenantID, 30*time.Second)
	if err != nil {
		return nil, "", fmt.Errorf("kyc service: acquire lock: %w", err)
	}
	if !acquired {
		return nil, "", domain.ErrVerificationInProgress
	}
	defer func() {
		s.releaseLease(ctx, tenantID, leaseToken)
	}()

	active, err := s.repo.GetActiveVerificationByTenant(ctx, tenantID)
	if err != nil && !errors.Is(err, domain.ErrVerificationNotFound) {
		return nil, "", fmt.Errorf("kyc service: check active verification for digilocker: %w", err)
	}
	if active != nil {
		if active.Status == domain.KYCStatusVerified {
			if active.ExpiresAt == nil || time.Now().UTC().Before(*active.ExpiresAt) {
				return nil, "", domain.ErrAlreadyVerified
			}
		} else if active.Status == domain.KYCStatusPending ||
			active.Status == domain.KYCStatusPendingManualReview ||
			active.Status == domain.KYCStatusRequestUnknown {
			return nil, "", domain.ErrVerificationInProgress
		}
	}

	vendorRefID := fmt.Sprintf("cf_dgl_%s", uuid.New().String())
	if s.cfg.DigiLockerVerifyAccountPreflight && strings.EqualFold(strings.TrimSpace(userFlow), cashfree.DigiLockerFlowSignUp) {
		verifier, ok := s.cashfree.(digiLockerAccountVerifier)
		if !ok {
			return nil, "", errors.New("kyc service: digilocker verify-account preflight unsupported by cashfree client")
		}
		if _, err := verifier.VerifyDigiLockerAccount(ctx, cashfree.VerifyDigiLockerAccountInput{
			VerificationID: vendorRefID,
		}); err != nil {
			return nil, "", fmt.Errorf("kyc service: digilocker verify-account preflight: %w", err)
		}
	}

	pending, err := s.repo.InitiateVerificationTx(ctx, tenantID, consent.ID, domain.KYCMethodDigiLocker, "cashfree_secure_id", vendorRefID, actor)
	if err != nil {
		return nil, "", fmt.Errorf("kyc service: initiate digilocker verification: %w", err)
	}

	sess, err := s.cashfree.CreateDigiLockerURL(ctx, cashfree.CreateDigiLockerURLInput{
		VerificationID: vendorRefID,
		RedirectURL:    redirectURL,
		UserFlow:       userFlow,
	})
	if err != nil {
		if isAmbiguousKYCError(err) {
			_ = s.repo.MarkRequestUnknownTx(ctx, pending.ID, err.Error(), actor, leaseToken)
			pending.Status = domain.KYCStatusRequestUnknown
			return pending, "", fmt.Errorf("kyc service: create digilocker url ambiguous state: %w", err)
		}
		_ = s.repo.FailVerificationTx(ctx, pending.ID, "CREATE_URL_FAILED", actor, leaseToken)
		return nil, "", fmt.Errorf("kyc service: create digilocker url: %w", err)
	}

	if sess.ReferenceID != "" {
		if refErr := s.repo.UpdateCFReferenceIDTx(ctx, pending.ID, sess.ReferenceID); refErr != nil {
			slog.Error("kyc service: failed persisting digilocker cf_reference_id; marking request_unknown for reconciliation",
				"verification_id", pending.ID, "cf_reference_id", sess.ReferenceID, "err", refErr)
			_ = s.repo.MarkRequestUnknownTx(ctx, pending.ID, fmt.Sprintf("cf_reference_id persistence failed: %v", refErr), actor, leaseToken)
			pending.Status = domain.KYCStatusRequestUnknown
			return pending, "", fmt.Errorf("kyc service: persist digilocker reference id: %w", refErr)
		}
		pending.CFReferenceID = &sess.ReferenceID
	}

	return pending, sess.URL, nil
}

func isAmbiguousTransportError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof")
}

func isAmbiguousKYCError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var apiErr *cashfree.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode >= 500 {
			return true
		}
	}
	return isAmbiguousTransportError(err)
}

// ProcessDigiLockerCallback processes a status change delivered by webhook or polling.
// It handles all 5 Cashfree DigiLocker event types.
func isVerificationNotFound(err error) bool {
	return errors.Is(err, domain.ErrVerificationNotFound) || errors.Is(err, pgx.ErrNoRows)
}

func (s *Service) ProcessDigiLockerCallback(ctx context.Context, verificationIDStr, cfRefID, eventType, status, actor string) (*domain.KYCVerification, error) {
	var vByVendor, vByCF *domain.KYCVerification
	var err error

	verificationIDStr = strings.TrimSpace(verificationIDStr)
	cfRefID = strings.TrimSpace(cfRefID)

	if verificationIDStr != "" {
		vByVendor, err = s.repo.GetVerificationByVendorRefID(ctx, verificationIDStr)
		if err != nil && !isVerificationNotFound(err) {
			return nil, fmt.Errorf("kyc service: lookup verification by vendor ref %q: %w", verificationIDStr, err)
		}
	}
	if cfRefID != "" {
		vByCF, err = s.repo.GetVerificationByCFReferenceID(ctx, cfRefID)
		if err != nil && !isVerificationNotFound(err) {
			return nil, fmt.Errorf("kyc service: lookup verification by cf ref %q: %w", cfRefID, err)
		}
	}
	if vByVendor != nil && vByCF != nil && vByVendor.ID != vByCF.ID {
		return nil, fmt.Errorf("kyc service: conflicting digilocker callback identifiers (%s vs %s)", verificationIDStr, cfRefID)
	}
	v := vByVendor
	if v == nil {
		v = vByCF
	}
	if v == nil {
		return nil, domain.ErrVerificationNotFound
	}
	if v.CFReferenceID != nil && *v.CFReferenceID != "" && cfRefID != "" && *v.CFReferenceID != cfRefID {
		return nil, fmt.Errorf("kyc service: digilocker callback cf_reference_id mismatch (%s vs %s)", *v.CFReferenceID, cfRefID)
	}

	// Terminal states and completed retrieval (pending_manual_review) are immutable to callbacks.
	if v.Status == domain.KYCStatusPendingManualReview ||
		v.Status == domain.KYCStatusVerified ||
		v.Status == domain.KYCStatusRejected ||
		v.Status == domain.KYCStatusRevoked ||
		v.Status == domain.KYCStatusExpired {
		return v, nil
	}

	// Handle link expiration / user consent failure events
	switch eventType {
	case domain.WebhookEventDigiLockerLinkExpired:
		if err := s.repo.FailVerificationTx(ctx, v.ID, "LINK_EXPIRED", actor); err != nil {
			return nil, fmt.Errorf("kyc service: fail verification on link expired: %w", err)
		}
		return s.repo.GetVerificationByID(ctx, v.ID)
	case domain.WebhookEventDigiLockerConsentDenied:
		if err := s.repo.FailVerificationTx(ctx, v.ID, "CONSENT_DENIED", actor); err != nil {
			return nil, fmt.Errorf("kyc service: fail verification on consent denied: %w", err)
		}
		return s.repo.GetVerificationByID(ctx, v.ID)
	case domain.WebhookEventDigiLockerConsentExpired:
		if err := s.repo.FailVerificationTx(ctx, v.ID, "CONSENT_EXPIRED", actor); err != nil {
			return nil, fmt.Errorf("kyc service: fail verification on consent expired: %w", err)
		}
		return s.repo.GetVerificationByID(ctx, v.ID)
	case domain.WebhookEventDigiLockerFailure:
		if err := s.repo.FailVerificationTx(ctx, v.ID, "VERIFICATION_FAILURE", actor); err != nil {
			return nil, fmt.Errorf("kyc service: fail verification on verification failure: %w", err)
		}
		return s.repo.GetVerificationByID(ctx, v.ID)
	}

	if status == string(cashfree.DigiLockerExpired) {
		if err := s.repo.FailVerificationTx(ctx, v.ID, "LINK_EXPIRED", actor); err != nil {
			return nil, fmt.Errorf("kyc service: fail verification on expired status: %w", err)
		}
		return s.repo.GetVerificationByID(ctx, v.ID)
	}
	if status == string(cashfree.DigiLockerConsentDenied) {
		if err := s.repo.FailVerificationTx(ctx, v.ID, "CONSENT_DENIED", actor); err != nil {
			return nil, fmt.Errorf("kyc service: fail verification on consent denied status: %w", err)
		}
		return s.repo.GetVerificationByID(ctx, v.ID)
	}

	// On SUCCESS / AUTHENTICATED: retrieve document from Cashfree
	if eventType == domain.WebhookEventDigiLockerSuccess || status == string(cashfree.DigiLockerAuthenticated) {
		if s.cashfree == nil {
			return nil, errors.New("kyc service: cashfree client unconfigured")
		}
		doc, err := s.cashfree.GetDigiLockerAadhaar(ctx, v.VendorReferenceID)
		if err != nil {
			if errors.Is(err, cashfree.ErrDigiLockerPending) {
				return v, nil
			}
			return nil, fmt.Errorf("kyc service: get digilocker aadhaar: %w", err)
		}

		if doc.Status == cashfree.DigiLockerAadhaarNotLinked {
			if err := s.repo.FailVerificationTx(ctx, v.ID, "AADHAAR_NOT_LINKED", actor); err != nil {
				return nil, fmt.Errorf("kyc service: fail verification on aadhaar not linked: %w", err)
			}
			return s.repo.GetVerificationByID(ctx, v.ID)
		}

		tenantName, _ := s.repo.GetTenantName(ctx, v.TenantID)
		nameMismatch := !domain.IsNameMatch(tenantName, doc.Name)

		canonicalDOB, isDedupable, _ := domain.NormalizeDOB(doc.DOB)
		var majorityDate *time.Time
		var identityHash string
		if isDedupable {
			majorityDate, _ = domain.MajorityDateFromDOB(canonicalDOB)
			normName := domain.NormalizeName(doc.Name)
			identityHash = domain.ComputeIdentityHash(s.cfg.IdentitySecret, normName, canonicalDOB, doc.Gender, doc.MaskedUID)
		}

		// ADR-005 INVARIANT: All verification methods (including DigiLocker) require
		// explicit manual owner review. Route every successful DigiLocker retrieval to
		// pending_manual_review with provider evidence attached; never auto-verify.
		_ = isDedupable
		_ = majorityDate
		ocrJSON, _ := json.Marshal(map[string]any{
			"document_type": "aadhaar",
			"source":        "digilocker",
			"name":          doc.Name,
			"dob":           doc.DOB,
			"gender":        doc.Gender,
			"masked_uid":    doc.MaskedUID,
			"name_mismatch": nameMismatch,
		})
		return s.repo.SubmitManualReviewTx(ctx, v.ID, doc.MaskedUID, identityHash, string(ocrJSON), "", doc.PhotoBytes, "image/jpeg", actor, "aadhaar")
	}

	return v, nil
}

// PollDigiLockerStatus checks the live status of an in-flight DigiLocker session.
func (s *Service) PollDigiLockerStatus(ctx context.Context, tenantID uuid.UUID, actor string) (*domain.KYCVerification, error) {
	v, err := s.repo.GetActiveVerificationByTenant(ctx, tenantID)
	if err != nil || v == nil {
		return nil, domain.ErrVerificationNotFound
	}
	if (v.Status != domain.KYCStatusPending && v.Status != domain.KYCStatusRequestUnknown) || v.Method != domain.KYCMethodDigiLocker {
		return v, nil
	}
	if s.cashfree == nil {
		return v, nil
	}
	res, err := s.cashfree.GetDigiLockerStatus(ctx, v.VendorReferenceID)
	if err != nil {
		return nil, fmt.Errorf("kyc service: poll digilocker status: %w", err)
	}
	if res.ReferenceID != "" && (v.CFReferenceID == nil || *v.CFReferenceID != res.ReferenceID) {
		if refErr := s.repo.UpdateCFReferenceIDTx(ctx, v.ID, res.ReferenceID); refErr != nil {
			slog.Error("kyc service: failed persisting polled digilocker cf_reference_id",
				"verification_id", v.ID, "cf_reference_id", res.ReferenceID, "err", refErr)
			return nil, fmt.Errorf("kyc service: persist polled digilocker reference id: %w", refErr)
		}
		v.CFReferenceID = &res.ReferenceID
	}
	if res.Status.IsTerminal() {
		return s.ProcessDigiLockerCallback(ctx, v.VendorReferenceID, res.ReferenceID, "", string(res.Status), actor)
	}
	return v, nil
}

// CanTenantPay determines if the tenant can make rent payments under the current KYC state and property policy.
func (s *Service) CanTenantPay(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	v, err := s.repo.GetActiveVerificationByTenant(ctx, tenantID)
	if err != nil || v == nil {
		return false, nil
	}
	if v.IsValid(time.Now().UTC()) {
		return true, nil
	}
	// Fail closed: only a verified, non-expired identity can pay. A document in manual review
	// does not grant payment access, and an expired verification blocks payment immediately.
	return false, nil
}

// GetVerificationByCFReferenceID retrieves a verification record by Cashfree's gateway reference ID.
func (s *Service) GetVerificationByCFReferenceID(ctx context.Context, cfRefID string) (*domain.KYCVerification, error) {
	return s.repo.GetVerificationByCFReferenceID(ctx, cfRefID)
}
