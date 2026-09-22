// Package kyc orchestrates the Aadhaar identity verification lifecycle:
// consent recording, DigiLocker flow initiation, completion via Cashfree webhook,
// offline Secure QR fallback, DPDP consent revocation, and duplicate-flag clearance.
//
// ADR-004 invariants enforced here:
//   - Network calls (Cashfree document fetch) always precede DB transactions.
//   - Consent must be active before any verification can be initiated or QR verified.
//   - Webhook processing is idempotent: terminal-state verifications are no-ops.
package kyc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/aadhaar"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// Sentinel errors surfaced to HTTP handlers for client-safe 4xx responses.
var (
	ErrNoActiveConsent       = errors.New("kyc: no active consent — record consent before initiating verification")
	ErrQRSignatureInvalid    = errors.New("kyc: QR signature verification failed — UIDAI key not configured or payload corrupted")
	ErrQRDataIncomplete      = errors.New("kyc: QR data incomplete — required fields (name, uid) missing; use DigiLocker for full verification")
	ErrDigiLockerUnavailable = errors.New("kyc: digilocker verification unavailable — cashfree is not configured")
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
	CompleteVerificationTx(ctx context.Context, verificationID uuid.UUID, maskedUID, identityHash string, isDedupable bool, hashKeyVersion int16, verifiedAt, expiresAt time.Time, actor string) (*domain.KYCVerification, error)
	FailVerificationTx(ctx context.Context, verificationID uuid.UUID, reason, actor string) error

	GetActiveVerificationByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, error)
	GetVerificationByVendorRefID(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error)
	GetLatestVerificationByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, error)
	ClearDuplicateFlag(ctx context.Context, verificationID uuid.UUID, actor, reason string) error

	ListAuditLogsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.KYCAuditLog, error)
}

// CashfreeKYCClient is the Cashfree Secure ID dependency.
// Implemented by *cashfree.Client.
type CashfreeKYCClient interface {
	CreateDigiLockerLink(ctx context.Context, verificationID, redirectURL string) (string, error)
	GetDigiLockerDocument(ctx context.Context, verificationID string) (*DigiLockerDoc, error)
}

// DigiLockerDoc is a value-type shim so the kyc package does not import cashfree
// directly (avoiding an import cycle if cashfree were ever to depend on kyc).
type DigiLockerDoc struct {
	Name      string
	DOB       string
	Gender    string
	MaskedUID string
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

	// DigiLockerRedirectURL is the server-side callback URL injected into the
	// Cashfree Secure ID request. MUST be server-constructed from config —
	// never taken from a client request.
	DigiLockerRedirectURL string
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
// verification can proceed. Idempotent: calling it again just creates a new
// consent record (older records remain and may be individually revoked).
func (s *Service) RecordConsent(
	ctx context.Context,
	tenantID uuid.UUID,
	purpose, consentVersion, consentText,
	ip, userAgent, actor string,
) (*domain.KYCConsent, error) {
	hash := domain.ComputeAuditHash("", tenantID, actor, "consent_text", consentText, time.Now().UTC())
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

// --- DigiLocker flow ----------------------------------------------------------

// InitiateDigiLocker enforces the network-before-transaction invariant:
//  1. Verifies active consent exists (repo read, cheap).
//  2. Allocates a verification UUID as vendor_reference_id.
//  3. Calls Cashfree to create a DigiLocker session (network — outside any tx).
//  4. Only then opens the DB transaction to record the pending verification.
//
// The redirect URL is always server-constructed from Config.DigiLockerRedirectURL.
func (s *Service) InitiateDigiLocker(ctx context.Context, tenantID uuid.UUID, actor string) (verificationURL string, err error) {
	// 1. Consent gate.
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

	// 2. Allocate stable vendor reference ID.
	vendorRefID := uuid.New().String()

	// 3. Network call — OUTSIDE transaction (ADR-004).
	if s.cashfree == nil {
		return "", ErrDigiLockerUnavailable
	}
	url, err := s.cashfree.CreateDigiLockerLink(ctx, vendorRefID, s.cfg.DigiLockerRedirectURL)
	if err != nil {
		return "", fmt.Errorf("kyc service: create digilocker link: %w", err)
	}

	// 4. DB transaction: supersede any stale pending, insert new pending record.
	if _, err := s.repo.InitiateVerificationTx(ctx,
		tenantID, consent.ID,
		domain.KYCMethodDigiLocker,
		"cashfree_secure_id", vendorRefID, actor,
	); err != nil {
		return "", fmt.Errorf("kyc service: initiate verification tx: %w", err)
	}

	return url, nil
}

// ProcessDigiLockerCompletion is called by the webhook handler after Cashfree
// delivers a VERIFICATION_COMPLETED or VERIFICATION_FAILED event.
//
// Idempotency: if the vendorRefID's verification is already in a terminal state
// (verified/failed/expired/revoked), this is a safe no-op — returns nil.
//
// Network-before-transaction: document fetch from Cashfree happens before the
// DB CompleteVerificationTx call (ADR-004).
//
// failedReason is non-empty only for VERIFICATION_FAILED events.
func (s *Service) ProcessDigiLockerCompletion(ctx context.Context, vendorRefID, failedReason, actor string) error {
	// 1. Look up the pending verification by vendor reference.
	v, err := s.repo.GetVerificationByVendorRefID(ctx, vendorRefID)
	if err != nil {
		if errors.Is(err, domain.ErrVerificationNotFound) {
			// Unknown vendor ref — could be a misconfigured event; log and swallow.
			return fmt.Errorf("kyc service: unknown vendor_ref_id %q: %w", vendorRefID, err)
		}
		return fmt.Errorf("kyc service: lookup vendor ref: %w", err)
	}

	// 2. Idempotency short-circuit: terminal states are immutable.
	switch v.Status {
	case domain.KYCStatusVerified, domain.KYCStatusFailed,
		domain.KYCStatusExpired, domain.KYCStatusRevoked:
		return nil // already processed; Cashfree is retrying — safe no-op
	}

	// 3. Failure path — no document fetch needed.
	if failedReason != "" {
		if err := s.repo.FailVerificationTx(ctx, v.ID, failedReason, actor); err != nil {
			return fmt.Errorf("kyc service: fail verification: %w", err)
		}
		return nil
	}

	// 4. Fetch Aadhaar document from Cashfree — OUTSIDE transaction (ADR-004).
	if s.cashfree == nil {
		return ErrDigiLockerUnavailable
	}
	doc, err := s.cashfree.GetDigiLockerDocument(ctx, vendorRefID)
	if err != nil {
		if IsPermanentError(err) {
			// Permanent failure (e.g. 4xx bad reference, expired session, corrupted payload).
			// Transition verification to 'failed' in DB and append audit log so the tenant
			// is not stuck in 'pending' indefinitely.
			//
			// Zero PII Invariant: Do not interpolate raw upstream response text into the audit log.
			// Format as a safe, structured failure category so the immutable audit chain
			// never receives unvalidated or demographic response fragments.
			var failReason string
			type httpStatusCodeGetter interface {
				HTTPStatusCode() int
			}
			var sc httpStatusCodeGetter
			if errors.As(err, &sc) {
				failReason = fmt.Sprintf("document_fetch_failed_http_%d", sc.HTTPStatusCode())
			} else {
				failReason = "document_corrupt_or_incomplete"
			}

			if failErr := s.repo.FailVerificationTx(ctx, v.ID, failReason, actor); failErr != nil {
				return fmt.Errorf("kyc service: fail verification after permanent doc error: %w", failErr)
			}
			// Return nil so the webhook handler acknowledges with 200 OK to stop retries.
			return nil
		}
		return fmt.Errorf("kyc service: fetch digilocker document: %w", err)
	}

	// 5. Normalise demographics.
	canonicalDOB, isDedupable, err := domain.NormalizeDOB(doc.DOB)
	if err != nil {
		// Year-only DOB is handled inside NormalizeDOB (returns isDedupable=false).
		// This branch only fires on truly unparseable formats.
		canonicalDOB = ""
		isDedupable = false
	}
	name := domain.NormalizeName(doc.Name)
	gender := doc.Gender
	identityHash := domain.ComputeIdentityHash(s.cfg.IdentitySecret, name, canonicalDOB, gender, doc.MaskedUID)

	// 6. DB transaction: mark verified, run dedup, write audit.
	now := time.Now().UTC()
	expiresAt := now.AddDate(0, 0, s.cfg.VerificationValidityDays)
	if _, err := s.repo.CompleteVerificationTx(ctx,
		v.ID, doc.MaskedUID, identityHash, isDedupable,
		s.cfg.HashKeyVersion, now, expiresAt, actor,
	); err != nil {
		if errors.Is(err, domain.ErrVerificationNotPending) {
			// TOCTOU race guard: the verification was superseded by a new attempt or revoked
			// while the upstream network document fetch was in flight.
			// Treat as an idempotent terminal no-op: return nil so webhook acks 200 OK.
			return nil
		}
		return fmt.Errorf("kyc service: complete verification tx: %w", err)
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

	// 3. Normalise and compute identity hash.
	dobStr := data.DOB
	if dobStr == "" {
		dobStr = data.YOB // year-only fallback
	}
	canonicalDOB, isDedupable, _ := domain.NormalizeDOB(dobStr)
	name := domain.NormalizeName(data.Name)
	identityHash := domain.ComputeIdentityHash(s.cfg.IdentitySecret, name, canonicalDOB, data.Gender, data.UIDLast4)

	// 4. Initiate pending (supersedes any stale record).
	vendorRefID := uuid.New().String()
	pending, err := s.repo.InitiateVerificationTx(ctx,
		tenantID, consent.ID,
		domain.KYCMethodQR,
		"uidai_qr", vendorRefID, actor,
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: initiate qr verification: %w", err)
	}

	// 5. Immediately complete (synchronous — no async webhook).
	now := time.Now().UTC()
	expiresAt := now.AddDate(0, 0, s.cfg.VerificationValidityDays)
	verified, err := s.repo.CompleteVerificationTx(ctx,
		pending.ID, data.UIDLast4, identityHash, isDedupable,
		s.cfg.HashKeyVersion, now, expiresAt, actor,
	)
	if err != nil {
		return nil, fmt.Errorf("kyc service: complete qr verification: %w", err)
	}
	return verified, nil
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

// GetVerificationByVendorRefID is exposed so the webhook handler can perform
// the idempotency check before invoking ProcessDigiLockerCompletion.
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
