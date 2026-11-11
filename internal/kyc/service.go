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
	ErrNoActiveConsent      = errors.New("kyc: no active consent — record consent before initiating verification")
	ErrQRSignatureInvalid   = errors.New("kyc: QR signature verification failed — UIDAI key not configured or payload corrupted")
	ErrQRDataIncomplete     = errors.New("kyc: QR data incomplete — required fields (name, uid) missing")
	ErrCashfreeUnavailable  = errors.New("kyc: verification unavailable — cashfree is not configured")
	ErrInvalidAadhaarNumber = errors.New("kyc: invalid aadhaar number — must be 12 digits and satisfy Verhoeff checksum")
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
	UpdateVendorRefIDTx(ctx context.Context, verificationID uuid.UUID, vendorRefID string) error
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
	CountAndRecordOTPAttemptTx(ctx context.Context, tenantID uuid.UUID, maxAllowed int, window time.Duration) (int, error)
	RefundOTPAttemptTx(ctx context.Context, tenantID uuid.UUID) error
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
	if fenced, ok := s.repo.(interface {
		ReleaseInFlightLockWithToken(ctx context.Context, tenantID, leaseToken uuid.UUID) error
	}); ok {
		_ = fenced.ReleaseInFlightLockWithToken(ctx, tenantID, leaseToken)
		return
	}
	_ = s.repo.ReleaseInFlightLock(ctx, tenantID)
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
		if existing.ConsentVersion == consentVersion && existing.IsActive() {
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
	var expiresAt time.Time
	if s.cfg.VerificationValidityDays > 0 {
		expiresAt = now.AddDate(0, 0, s.cfg.VerificationValidityDays)
	}
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
//   - In automatic verification, demographic fields (Name, DOB, Gender) are ephemeral inputs to ComputeIdentityHash and IsNameMatch; zero demographic persistence.
//   - In manual review, demographic OCR data is stored encrypted and wiped upon review decision (approval, rejection, expiry, or revocation).
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

	// 2. Acquire distributed in-flight lease FIRST (60s TTL, fenced multi-instance guard)
	leaseToken, locked, err := s.acquireLease(ctx, tenantID, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("kyc service: acquire in-flight lock: %w", err)
	}
	if !locked {
		return nil, domain.ErrVerificationInProgress
	}
	defer func() {
		s.releaseLease(context.Background(), tenantID, leaseToken)
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

	// 4. Create durable pending verification record BEFORE dispatching billable Smart OCR call.
	// This prevents orphaning a billed operation if a local DB failure occurs after provider success.
	tenantName, err := s.repo.GetTenantName(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("kyc service: fetch tenant name: %w", err)
	}

	intentRefID := fmt.Sprintf("intent_ocr_%s", uuid.New().String())
	pending, err := s.repo.InitiateVerificationTx(ctx,
		tenantID, consent.ID,
		domain.KYCMethodOCR,
		"cashfree_smart_ocr", intentRefID, actor,
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: initiate ocr verification intent: %w", err)
	}

	var resp *cashfree.SmartOCRResponse
	if s.cashfree != nil {
		resp, err = s.cashfree.UploadAadhaarDocument(ctx, bytes.NewReader(fileBytes), filename)
		if err != nil {
			_ = s.repo.FailVerificationTx(ctx, pending.ID, "OCR_UPLOAD_FAILED", actor)
			return nil, fmt.Errorf("kyc service: upload aadhaar document: %w", err)
		}
	}

	var vendorRefID string
	var ocrBytes []byte
	var maskedUID string
	var identityHash string

	if resp == nil || resp.Name == "" || resp.MaskedUID == "" {
		// KYC-2: Synthetic reference ensures vendor_ref_id is collision-free under concurrent uploads.
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
			"name":          tenantName,
			"masked_uid":    maskedUID,
			"name_mismatch": nameMismatch,
			"ocr_status":    "UNAVAILABLE",
		})
	} else {
		slog.InfoContext(ctx, "cashfree smart ocr completed",
			"tenant_id", tenantID,
			"cashfree_reference_id", resp.ReferenceID,
			"vendor_status", resp.Status,
			"message_code", resp.MessageCode,
			"qr_status", resp.QRStatus,
			"has_photo", len(resp.PhotoBytes) > 0,
		)
		vendorRefID = fmt.Sprintf("cf_ocr_%v", resp.ReferenceID)
		nameMismatch := !domain.IsNameMatch(tenantName, resp.Name)
		canonicalDOB, _, _ := domain.NormalizeDOB(resp.DOB)
		name := domain.NormalizeName(resp.Name)
		identityHash = domain.ComputeIdentityHash(s.cfg.IdentitySecret, name, canonicalDOB, resp.Gender, resp.MaskedUID)
		maskedUID = resp.MaskedUID
		ocrBytes, _ = json.Marshal(map[string]any{
			"name":          resp.Name,
			"dob":           resp.DOB,
			"gender":        resp.Gender,
			"masked_uid":    resp.MaskedUID,
			"qr_status":     resp.QRStatus,
			"name_mismatch": nameMismatch,
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
		slog.ErrorContext(ctx, "kyc service: failed to persist vendor_ref_id after retries; failing verification",
			"verification_id", pending.ID,
			"vendor_ref_id", vendorRefID,
			"err", updateRefErr,
		)
		_ = s.repo.FailVerificationTx(ctx, pending.ID, "VENDOR_REF_PERSISTENCE_FAILED", actor)
		return nil, fmt.Errorf("kyc service: persist vendor ref id: %w", updateRefErr)
	}

	docURL := fmt.Sprintf("/api/owner/kyc/%s/document", pending.ID)
	underReview, err := s.repo.SubmitManualReviewTx(ctx,
		pending.ID, maskedUID, identityHash,
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
		s.releaseLease(context.Background(), tenantID, leaseToken)
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
		} else if active.Status == domain.KYCStatusPending && time.Since(active.CreatedAt) < 60*time.Second {
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
		fail := s.classifyAndRecordAadhaarFailure(ctx, tenantID, &pending.ID, httpStatus, msg, actor, err)
		if fail.Class == cashfree.FailureSystemFault {
			_ = s.repo.RefundOTPAttemptTx(ctx, tenantID)
		}
		_ = s.repo.FailVerificationTx(ctx, pending.ID, fail.ReasonCode, actor)
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
		_ = s.repo.FailVerificationTx(ctx, pending.ID, fail.ReasonCode, actor)
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
		slog.Error("CRITICAL: failed to update vendor reference id after Cashfree success",
			"tenant_id", tenantID,
			"ref_id", res.RefID,
			"vendor_ref_id", vendorRefID,
			"err", updateErr,
		)
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
	leaseToken, locked, err := s.acquireLease(ctx, tenantID, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("kyc service: acquire in-flight lock: %w", err)
	}
	if !locked {
		return nil, domain.ErrVerificationInProgress
	}
	defer func() {
		s.releaseLease(context.Background(), tenantID, leaseToken)
	}()

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
		return nil, s.classifyAndRecordAadhaarFailure(ctx, tenantID, &pending.ID, httpStatus, msg, actor, err)
	}

	// Fail closed unless status == "VALID", before reading any demographic identity data
	if res == nil || !strings.EqualFold(strings.TrimSpace(res.Status), "VALID") {
		statusStr := "UNKNOWN"
		msg := ""
		if res != nil {
			statusStr = res.Status
			msg = res.Message
			if msg == "" {
				msg = res.Status
			}
		}
		underlying := fmt.Errorf("aadhaar otp verification not valid: status=%s, message=%s", statusStr, msg)
		return nil, s.classifyAndRecordAadhaarFailure(ctx, tenantID, &pending.ID, 200, msg, actor, underlying)
	}

	canonicalDOB, isDedupable, _ := domain.NormalizeDOB(res.DOB)
	majorityDate, mdErr := domain.MajorityDateFromDOB(canonicalDOB)
	if mdErr != nil {
		slog.Warn("kyc: unparseable dob received from aadhaar otp", "tenant_id", tenantID, "err", mdErr)
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
	var expiresAt time.Time
	if s.cfg.VerificationValidityDays > 0 {
		expiresAt = now.AddDate(0, 0, s.cfg.VerificationValidityDays)
	}

	// AUTO-VERIFIED: Government authenticated via direct UIDAI OTP
	var verified *domain.KYCVerification
	var completeErr error
	for attempt := 0; attempt < 3; attempt++ {
		verified, completeErr = s.repo.CompleteVerificationTx(ctx,
			pending.ID, res.MaskedUID, identityHash, isDedupable,
			s.cfg.HashKeyVersion, nil, nameMismatch, res.PhotoBytes,
			now, expiresAt, majorityDate, actor,
		)
		if completeErr == nil {
			break
		}
		if errors.Is(completeErr, domain.ErrVerificationNotPending) {
			current, checkErr := s.repo.GetVerificationByVendorRefID(ctx, vendorRefID)
			if checkErr == nil && current != nil && current.Status == domain.KYCStatusVerified {
				return current, nil
			}
		}
		slog.Warn("kyc: CompleteVerificationTx failed, retrying",
			"verification_id", pending.ID,
			"attempt", attempt+1,
			"err", completeErr,
		)
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	if completeErr != nil {
		return nil, fmt.Errorf("kyc service: complete aadhaar otp: %w", completeErr)
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
func (s *Service) RecordGuardianConsent(ctx context.Context, tenantID uuid.UUID, guardianName, guardianPhone, relation, refID string, actor string) (*domain.Tenant, error) {
	return s.repo.RecordGuardianConsentTx(ctx, tenantID, guardianName, guardianPhone, relation, refID, actor)
}
