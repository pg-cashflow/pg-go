package kyc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
)

// ---- Fakes ------------------------------------------------------------------

type fakeRepo struct {
	consent         *domain.KYCConsent
	verification    *domain.KYCVerification
	latestVerif     *domain.KYCVerification
	auditLogs       []domain.KYCAuditLog
	tenantName      string
	attestedPhoto   []byte
	lockAcquired    *bool
	lockAcquireErr  error
	lockReleaseErr  error
	releasedLockIDs []uuid.UUID
	recordConsentFn func(c *domain.KYCConsent) error
	initiateVTxFn   func() (*domain.KYCVerification, error)
	completeVTxFn   func(id uuid.UUID) (*domain.KYCVerification, error)
	failVTxFn       func(id uuid.UUID, reason string) error
	clearDupFn      func(id uuid.UUID, reason string) error
}

func (f *fakeRepo) RecordConsent(_ context.Context, c *domain.KYCConsent, _ string) error {
	if f.recordConsentFn != nil {
		return f.recordConsentFn(c)
	}
	c.ID = uuid.New()
	f.consent = c
	return nil
}
func (f *fakeRepo) GetActiveConsentByTenant(_ context.Context, _ uuid.UUID) (*domain.KYCConsent, error) {
	if f.consent == nil {
		return nil, domain.ErrVerificationNotFound
	}
	return f.consent, nil
}
func (f *fakeRepo) RevokeConsent(_ context.Context, _ uuid.UUID, _ string) error { return nil }

func (f *fakeRepo) InitiateVerificationTx(_ context.Context, tenantID, consentID uuid.UUID, method domain.KYCMethod, _, vendorRefID, _ string) (*domain.KYCVerification, error) {
	if f.initiateVTxFn != nil {
		return f.initiateVTxFn()
	}
	v := &domain.KYCVerification{
		ID:                uuid.New(),
		TenantID:          tenantID,
		ConsentID:         consentID,
		Method:            method,
		VendorReferenceID: vendorRefID,
		Status:            domain.KYCStatusPending,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}
	f.verification = v
	return v, nil
}
func (f *fakeRepo) CompleteVerificationTx(_ context.Context, id uuid.UUID, maskedUID, _ string, _ bool, _ int16, qrStatus *string, nameMismatch bool, attestedPhotoBytes []byte, _, _ time.Time, _ string) (*domain.KYCVerification, error) {
	if f.completeVTxFn != nil {
		return f.completeVTxFn(id)
	}
	now := time.Now().UTC()
	refID := ""
	tenantID := uuid.Nil
	if f.verification != nil {
		refID = f.verification.VendorReferenceID
		tenantID = f.verification.TenantID
	}
	v := &domain.KYCVerification{
		ID:                 id,
		TenantID:           tenantID,
		VendorReferenceID:  refID,
		Status:             domain.KYCStatusVerified,
		MaskedUID:          &maskedUID,
		QRStatus:           qrStatus,
		NameMismatch:       nameMismatch,
		AttestedPhotoBytes: attestedPhotoBytes,
		PhotoStored:        len(attestedPhotoBytes) > 0,
		VerifiedAt:         &now,
	}
	f.latestVerif = v
	if f.verification != nil && f.verification.ID == id {
		f.verification = v
	}
	return v, nil
}
func (f *fakeRepo) FailVerificationTx(_ context.Context, id uuid.UUID, reason, _ string) error {
	if f.failVTxFn != nil {
		return f.failVTxFn(id, reason)
	}
	return nil
}
func (f *fakeRepo) GetActiveVerificationByTenant(_ context.Context, _ uuid.UUID) (*domain.KYCVerification, error) {
	if f.verification == nil {
		return nil, domain.ErrVerificationNotFound
	}
	return f.verification, nil
}
func (f *fakeRepo) GetVerificationByVendorRefID(_ context.Context, vendorRefID string) (*domain.KYCVerification, error) {
	if f.verification == nil || f.verification.VendorReferenceID != vendorRefID {
		return nil, domain.ErrVerificationNotFound
	}
	return f.verification, nil
}
func (f *fakeRepo) GetLatestVerificationByTenant(_ context.Context, _ uuid.UUID) (*domain.KYCVerification, error) {
	if f.latestVerif != nil {
		return f.latestVerif, nil
	}
	if f.verification != nil {
		return f.verification, nil
	}
	return nil, domain.ErrVerificationNotFound
}
func (f *fakeRepo) ClearDuplicateFlag(_ context.Context, id uuid.UUID, _, reason string) error {
	if f.clearDupFn != nil {
		return f.clearDupFn(id, reason)
	}
	return nil
}
func (f *fakeRepo) ListAuditLogsByTenant(_ context.Context, _ uuid.UUID) ([]domain.KYCAuditLog, error) {
	return f.auditLogs, nil
}
func (f *fakeRepo) TryAcquireInFlightLock(_ context.Context, _ uuid.UUID, _ time.Duration) (bool, error) {
	if f.lockAcquireErr != nil {
		return false, f.lockAcquireErr
	}
	if f.lockAcquired != nil {
		return *f.lockAcquired, nil
	}
	return true, nil
}
func (f *fakeRepo) ReleaseInFlightLock(_ context.Context, tenantID uuid.UUID) error {
	f.releasedLockIDs = append(f.releasedLockIDs, tenantID)
	return f.lockReleaseErr
}
func (f *fakeRepo) GetTenantName(_ context.Context, _ uuid.UUID) (string, error) {
	if f.tenantName != "" {
		return f.tenantName, nil
	}
	return "Ravi Kumar", nil
}
func (f *fakeRepo) GetAttestedPhoto(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return f.attestedPhoto, nil
}

type fakeCashfree struct {
	linkURL    string
	linkErr    error
	linkCalled bool
	doc        *kycsvc.DigiLockerDoc
	docErr     error
	status     *cashfree.DigiLockerStatus
	statusErr  error
	ocrResp    *cashfree.SmartOCRResponse
	ocrErr     error
	ocrCalled  bool
}

func (f *fakeCashfree) CreateDigiLockerLink(_ context.Context, _, _ string) (string, error) {
	f.linkCalled = true
	return f.linkURL, f.linkErr
}
func (f *fakeCashfree) GetDigiLockerDocument(_ context.Context, _ string) (*kycsvc.DigiLockerDoc, error) {
	return f.doc, f.docErr
}
func (f *fakeCashfree) GetDigiLockerStatus(_ context.Context, _ string) (*cashfree.DigiLockerStatus, error) {
	return f.status, f.statusErr
}
func (f *fakeCashfree) UploadAadhaarDocument(_ context.Context, _ io.Reader, _ string) (*cashfree.SmartOCRResponse, error) {
	f.ocrCalled = true
	return f.ocrResp, f.ocrErr
}

func newTestService(repo *fakeRepo, cf kycsvc.CashfreeKYCClient) *kycsvc.Service {
	return kycsvc.NewService(repo, cf, kycsvc.Config{
		IdentitySecret:           "test-secret-key",
		HashKeyVersion:           1,
		VerificationValidityDays: 365,
		DigiLockerRedirectURL:    "https://app.example.com/kyc/callback",
	})
}

// ---- Tests -------------------------------------------------------------------

func TestRecordConsent(t *testing.T) {
	repo := &fakeRepo{}
	svc := newTestService(repo, &fakeCashfree{})

	c, err := svc.RecordConsent(context.Background(), uuid.New(),
		"aadhaar_kyc", "v1", "I consent to Aadhaar verification",
		"127.0.0.1", "Mozilla/5.0", "tenant:abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.ID == uuid.Nil {
		t.Error("consent ID should be set after recording")
	}
}

func TestInitiateDigiLocker_NoConsent(t *testing.T) {
	repo := &fakeRepo{} // no consent
	svc := newTestService(repo, &fakeCashfree{linkURL: "https://digi"})

	_, err := svc.InitiateDigiLocker(context.Background(), uuid.New(), "tenant:abc")
	if !errors.Is(err, kycsvc.ErrNoActiveConsent) {
		t.Errorf("expected ErrNoActiveConsent, got %v", err)
	}
}

func TestInitiateDigiLocker_HappyPath(t *testing.T) {
	repo := &fakeRepo{
		consent: &domain.KYCConsent{ID: uuid.New(), TenantID: uuid.New()},
	}
	cf := &fakeCashfree{linkURL: "https://digilocker.gov.in/verify?session=xyz"}
	svc := newTestService(repo, cf)

	url, err := svc.InitiateDigiLocker(context.Background(), repo.consent.TenantID, "tenant:abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url != cf.linkURL {
		t.Errorf("expected %s, got %s", cf.linkURL, url)
	}
	if repo.verification == nil {
		t.Error("expected a pending verification to be created in repo")
	}
	if repo.verification.Status != domain.KYCStatusPending {
		t.Errorf("expected pending, got %s", repo.verification.Status)
	}
}

func TestProcessDigiLockerCompletion_Idempotency(t *testing.T) {
	// If already verified, ProcessDigiLockerCompletion must be a no-op (nil error)
	// and must not call GetDigiLockerDocument.
	docCallCount := 0
	cf := &fakeCashfree{
		doc: &kycsvc.DigiLockerDoc{Name: "Ravi Kumar", DOB: "1990-05-15", Gender: "M", MaskedUID: "XXXX-1234"},
	}
	cf_counted := &countingCashfree{inner: cf, onDoc: func() { docCallCount++ }}

	verified := domain.KYCStatusVerified
	tenantID := uuid.New()
	vendorRefID := "ver-already-done"
	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:                uuid.New(),
			TenantID:          tenantID,
			VendorReferenceID: vendorRefID,
			Status:            verified,
		},
	}
	svc := newTestService(repo, cf_counted)

	err := svc.ProcessDigiLockerCompletion(context.Background(), vendorRefID, "", "cashfree_webhook")
	if err != nil {
		t.Fatalf("expected nil (no-op), got %v", err)
	}
	if docCallCount != 0 {
		t.Errorf("expected 0 document fetches for already-verified record, got %d", docCallCount)
	}
}

func TestProcessDigiLockerCompletion_FailedEvent(t *testing.T) {
	failCalled := false
	tenantID := uuid.New()
	vendorRefID := "ver-fail"
	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:                uuid.New(),
			TenantID:          tenantID,
			VendorReferenceID: vendorRefID,
			Status:            domain.KYCStatusPending,
		},
		failVTxFn: func(_ uuid.UUID, reason string) error {
			failCalled = true
			if reason == "" {
				t.Error("reason should not be empty for failure events")
			}
			return nil
		},
	}
	svc := newTestService(repo, &fakeCashfree{})

	err := svc.ProcessDigiLockerCompletion(context.Background(), vendorRefID, "user declined", "cashfree_webhook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !failCalled {
		t.Error("expected FailVerificationTx to be called for VERIFICATION_FAILED event")
	}
}

func TestProcessDigiLockerCompletion_HappyPath(t *testing.T) {
	tenantID := uuid.New()
	vendorRefID := "ver-ok"
	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:                uuid.New(),
			TenantID:          tenantID,
			VendorReferenceID: vendorRefID,
			Status:            domain.KYCStatusPending,
		},
	}
	cf := &fakeCashfree{
		doc: &kycsvc.DigiLockerDoc{
			Name: "Ravi Kumar", DOB: "1990-05-15", Gender: "M", MaskedUID: "XXXX-1234",
		},
	}
	svc := newTestService(repo, cf)

	err := svc.ProcessDigiLockerCompletion(context.Background(), vendorRefID, "", "cashfree_webhook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.latestVerif == nil {
		t.Error("expected verification to be completed")
	}
}

func TestVerifySecureQR_NoConsent(t *testing.T) {
	repo := &fakeRepo{}
	svc := newTestService(repo, &fakeCashfree{})

	// Any raw QR string — should fail on consent gate before QR decode.
	_, err := svc.VerifySecureQR(context.Background(), uuid.New(), "some-qr-payload", "tenant:abc")
	if !errors.Is(err, kycsvc.ErrNoActiveConsent) {
		t.Errorf("expected ErrNoActiveConsent, got %v", err)
	}
}

func TestVerifySecureQR_InvalidQR(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{ID: uuid.New(), TenantID: tenantID},
	}
	svc := newTestService(repo, &fakeCashfree{})

	// A garbage string that looks like a Secure QR (all digits) but fails RSA verify.
	_, err := svc.VerifySecureQR(context.Background(), tenantID, "12345678901234567890", "tenant:abc")
	// Should fail with either QRSignatureInvalid or QRDataIncomplete.
	if err == nil {
		t.Fatal("expected error for invalid QR payload")
	}
}

func TestClearDuplicateFlag_RequiresReason(t *testing.T) {
	repo := &fakeRepo{}
	svc := newTestService(repo, &fakeCashfree{})

	err := svc.ClearDuplicateFlag(context.Background(), uuid.New(), "owner:xyz", "")
	if err == nil {
		t.Fatal("expected error when reason is empty")
	}
}

func TestInitiateDigiLocker_NilCashfreeClient(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{ID: uuid.New(), TenantID: tenantID},
	}
	svc := newTestService(repo, nil)

	_, err := svc.InitiateDigiLocker(context.Background(), tenantID, "tenant:123")
	if !errors.Is(err, kycsvc.ErrDigiLockerUnavailable) {
		t.Fatalf("expected ErrDigiLockerUnavailable when cashfree client is nil, got %v", err)
	}
}

func TestProcessDigiLockerCompletion_NilCashfreeClient(t *testing.T) {
	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:                uuid.New(),
			Status:            domain.KYCStatusPending,
			VendorReferenceID: "ref-123",
		},
	}
	svc := newTestService(repo, nil)

	err := svc.ProcessDigiLockerCompletion(context.Background(), "ref-123", "", "webhook")
	if !errors.Is(err, kycsvc.ErrDigiLockerUnavailable) {
		t.Fatalf("expected ErrDigiLockerUnavailable when cashfree client is nil, got %v", err)
	}
}

type fakePermanentError struct {
	msg string
}

func (e *fakePermanentError) Error() string     { return e.msg }
func (e *fakePermanentError) IsPermanent() bool { return true }

type fakePermanentHTTPError struct {
	status int
}

func (e *fakePermanentHTTPError) Error() string       { return fmt.Sprintf("http %d", e.status) }
func (e *fakePermanentHTTPError) IsPermanent() bool   { return true }
func (e *fakePermanentHTTPError) HTTPStatusCode() int { return e.status }

func TestProcessDigiLockerCompletion_PermanentDocFetchError(t *testing.T) {
	var failedID uuid.UUID
	var failReason string
	verifID := uuid.New()
	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:                verifID,
			Status:            domain.KYCStatusPending,
			VendorReferenceID: "ref-perm-404",
		},
		failVTxFn: func(id uuid.UUID, reason string) error {
			failedID = id
			failReason = reason
			return nil
		},
	}
	cf := &fakeCashfree{
		docErr: &fakePermanentHTTPError{status: 404},
	}
	svc := newTestService(repo, cf)

	err := svc.ProcessDigiLockerCompletion(context.Background(), "ref-perm-404", "", "cashfree_webhook")
	if err != nil {
		t.Fatalf("expected nil error on permanent failure (so webhook returns 200), got: %v", err)
	}
	if failedID != verifID {
		t.Errorf("expected verification %s to be failed, got %s", verifID, failedID)
	}
	// Assert sanitized failure code to guarantee zero PII leakage into audit table
	if failReason != "document_fetch_failed_http_404" {
		t.Errorf("expected failReason 'document_fetch_failed_http_404', got %q", failReason)
	}
}

func TestProcessDigiLockerCompletion_TransientDocFetchError(t *testing.T) {
	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:                uuid.New(),
			Status:            domain.KYCStatusPending,
			VendorReferenceID: "ref-transient-500",
		},
	}
	cf := &fakeCashfree{
		docErr: errors.New("network timeout"),
	}
	svc := newTestService(repo, cf)

	err := svc.ProcessDigiLockerCompletion(context.Background(), "ref-transient-500", "", "cashfree_webhook")
	if err == nil {
		t.Fatal("expected error on transient failure (so webhook returns 500 for retry)")
	}
}

func TestProcessDigiLockerCompletion_SupersededSessionIsNoOp(t *testing.T) {
	docFetched := false
	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:                uuid.New(),
			Status:            domain.KYCStatusExpired, // Superseded sessions are transitioned to 'expired'
			VendorReferenceID: "ref-superseded-A",
		},
	}
	cf := &fakeCashfree{}
	countingCF := &countingCashfree{
		inner: cf,
		onDoc: func() { docFetched = true },
	}
	svc := newTestService(repo, countingCF)

	err := svc.ProcessDigiLockerCompletion(context.Background(), "ref-superseded-A", "", "cashfree_webhook")
	if err != nil {
		t.Fatalf("expected nil error (idempotent no-op for superseded session), got %v", err)
	}
	if docFetched {
		t.Errorf("expected no document fetch for already-superseded/expired verification")
	}
}

func TestProcessDigiLockerCompletion_TOCTOURace_SupersededDuringNetworkFetch(t *testing.T) {
	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:                uuid.New(),
			Status:            domain.KYCStatusPending, // Initially pending when webhook starts
			VendorReferenceID: "ref-race-123",
		},
		completeVTxFn: func(id uuid.UUID) (*domain.KYCVerification, error) {
			// Simulates tenant re-initiating or revoking during document fetch:
			// CompleteVerificationTx finds row is no longer pending
			return nil, domain.ErrVerificationNotPending
		},
	}
	cf := &fakeCashfree{
		doc: &kycsvc.DigiLockerDoc{
			Name:      "Test Tenant",
			DOB:       "1995-01-01",
			Gender:    "M",
			MaskedUID: "XXXX-XXXX-9999",
		},
	}
	svc := newTestService(repo, cf)

	// Webhook must gracefully return nil (200 OK) without retrying or failing
	err := svc.ProcessDigiLockerCompletion(context.Background(), "ref-race-123", "", "cashfree_webhook")
	if err != nil {
		t.Fatalf("expected nil error (idempotent no-op when superseded during fetch), got %v", err)
	}
}

func TestVerifyAadhaarDocument_Success(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		tenantName: "Ravi Kumar",
	}
	qrStatus := domain.QRStatusSecure
	cf := &fakeCashfree{
		ocrResp: &cashfree.SmartOCRResponse{
			ReferenceID: 123456,
			Status:      "SUCCESS",
			Name:        "Ravi Kumar",
			DOB:         "1990-01-01",
			Gender:      "M",
			MaskedUID:   "1234",
			QRStatus:    &qrStatus,
			PhotoBytes:  []byte{0xDE, 0xAD, 0xBE, 0xEF},
		},
	}
	svc := newTestService(repo, cf)

	v, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("dummy pdf"), "aadhaar.pdf", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Status != domain.KYCStatusVerified {
		t.Fatalf("expected status verified, got %s", v.Status)
	}
	if !v.PhotoStored || len(v.AttestedPhotoBytes) == 0 {
		t.Errorf("expected photo to be stored")
	}
	if v.NameMismatch {
		t.Errorf("expected name_mismatch=false for exact name match")
	}
	if v.TrustTier() != "document_secure_qr" {
		t.Errorf("expected trust tier document_secure_qr, got %s", v.TrustTier())
	}
}

func TestVerifyAadhaarDocument_NameMismatch(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		tenantName: "Priya Sharma",
	}
	cf := &fakeCashfree{
		ocrResp: &cashfree.SmartOCRResponse{
			ReferenceID: 123457,
			Status:      "SUCCESS",
			Name:        "Ravi Kumar",
			DOB:         "1990-01-01",
			Gender:      "M",
			MaskedUID:   "1234",
		},
	}
	svc := newTestService(repo, cf)

	v, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("dummy pdf"), "aadhaar.pdf", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.NameMismatch {
		t.Errorf("expected name_mismatch=true for different names")
	}
}

func TestGetDigiLockerReturnStatus_PendingToCompleted(t *testing.T) {
	tenantID := uuid.New()
	vendorRefID := "dl-return-123"
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		verification: &domain.KYCVerification{
			ID:                uuid.New(),
			TenantID:          tenantID,
			VendorReferenceID: vendorRefID,
			Status:            domain.KYCStatusPending,
		},
		tenantName: "Ravi Kumar",
	}
	cf := &fakeCashfree{
		status: &cashfree.DigiLockerStatus{Status: "COMPLETED"},
		doc: &kycsvc.DigiLockerDoc{
			Name:      "Ravi Kumar",
			DOB:       "1990-01-01",
			Gender:    "M",
			MaskedUID: "XXXX-XXXX-1234",
		},
	}
	svc := newTestService(repo, cf)

	v, err := svc.GetDigiLockerReturnStatus(context.Background(), vendorRefID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v == nil || v.Status != domain.KYCStatusVerified {
		t.Fatalf("expected completed verification, got %+v", v)
	}
}

func TestInitiateDigiLocker_InFlightLockContention(t *testing.T) {
	tenantID := uuid.New()
	locked := false
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		lockAcquired: &locked,
	}
	cf := &fakeCashfree{}
	svc := newTestService(repo, cf)

	_, err := svc.InitiateDigiLocker(context.Background(), tenantID, "tenant:"+tenantID.String())
	if !errors.Is(err, domain.ErrVerificationInProgress) {
		t.Fatalf("expected ErrVerificationInProgress, got %v", err)
	}
	if cf.linkCalled {
		t.Error("expected Cashfree CreateDigiLockerLink to NOT be called under lease contention")
	}
}

func TestInitiateDigiLocker_ReleaseLockOnError(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{
		linkErr: errors.New("upstream cashfree 503"),
	}
	svc := newTestService(repo, cf)

	_, err := svc.InitiateDigiLocker(context.Background(), tenantID, "tenant:"+tenantID.String())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(repo.releasedLockIDs) != 1 || repo.releasedLockIDs[0] != tenantID {
		t.Fatalf("expected lease lock to be released via defer on error, got released IDs: %v", repo.releasedLockIDs)
	}
}

func TestVerifyAadhaarDocument_InFlightLockContention(t *testing.T) {
	tenantID := uuid.New()
	locked := false
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		lockAcquired: &locked,
	}
	cf := &fakeCashfree{}
	svc := newTestService(repo, cf)

	_, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("dummy"), "aadhaar.pdf", "tenant:"+tenantID.String())
	if !errors.Is(err, domain.ErrVerificationInProgress) {
		t.Fatalf("expected ErrVerificationInProgress, got %v", err)
	}
	if cf.ocrCalled {
		t.Error("expected Cashfree UploadAadhaarDocument to NOT be called under lease contention")
	}
}

func TestVerifyAadhaarDocument_ReleaseLockOnError(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{
		ocrErr: errors.New("upstream ocr failure"),
	}
	svc := newTestService(repo, cf)

	_, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("dummy"), "aadhaar.pdf", "tenant:"+tenantID.String())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(repo.releasedLockIDs) != 1 || repo.releasedLockIDs[0] != tenantID {
		t.Fatalf("expected lease lock to be released via defer on error, got released IDs: %v", repo.releasedLockIDs)
	}
}

func TestService_InitiateDigiLocker_Cooldown(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       consentID,
			TenantID: tenantID,
		},
		verification: &domain.KYCVerification{
			ID:        uuid.New(),
			TenantID:  tenantID,
			Status:    domain.KYCStatusPending,
			CreatedAt: time.Now().UTC().Add(-10 * time.Second), // Created 10s ago (< 60s cooldown)
		},
	}
	cf := &fakeCashfree{linkURL: "https://test.cf/digilocker"}
	svc := newTestService(repo, cf)

	_, err := svc.InitiateDigiLocker(context.Background(), tenantID, "tenant:"+tenantID.String())
	if !errors.Is(err, domain.ErrVerificationInProgress) {
		t.Fatalf("expected ErrVerificationInProgress on cooldown, got %v", err)
	}
	if len(repo.releasedLockIDs) != 1 || repo.releasedLockIDs[0] != tenantID {
		t.Fatalf("expected lease lock to be released on cooldown return, got: %v", repo.releasedLockIDs)
	}
}

func TestService_VerifyAadhaarDocument_Cooldown(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       consentID,
			TenantID: tenantID,
		},
		verification: &domain.KYCVerification{
			ID:        uuid.New(),
			TenantID:  tenantID,
			Status:    domain.KYCStatusPending,
			CreatedAt: time.Now().UTC().Add(-20 * time.Second), // Created 20s ago (< 60s cooldown)
		},
	}
	cf := &fakeCashfree{}
	svc := newTestService(repo, cf)

	_, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("dummy"), "card.jpg", "tenant:"+tenantID.String())
	if !errors.Is(err, domain.ErrVerificationInProgress) {
		t.Fatalf("expected ErrVerificationInProgress on cooldown, got %v", err)
	}
	if len(repo.releasedLockIDs) != 1 || repo.releasedLockIDs[0] != tenantID {
		t.Fatalf("expected lease lock to be released on cooldown return, got: %v", repo.releasedLockIDs)
	}
}

// ---- Helpers ----------------------------------------------------------------

type countingCashfree struct {
	inner *fakeCashfree
	onDoc func()
}

func (c *countingCashfree) CreateDigiLockerLink(ctx context.Context, verificationID, redirectURL string) (string, error) {
	return c.inner.CreateDigiLockerLink(ctx, verificationID, redirectURL)
}
func (c *countingCashfree) GetDigiLockerDocument(ctx context.Context, verificationID string) (*kycsvc.DigiLockerDoc, error) {
	c.onDoc()
	return c.inner.GetDigiLockerDocument(ctx, verificationID)
}
func (c *countingCashfree) GetDigiLockerStatus(ctx context.Context, verificationID string) (*cashfree.DigiLockerStatus, error) {
	return c.inner.GetDigiLockerStatus(ctx, verificationID)
}
func (c *countingCashfree) UploadAadhaarDocument(ctx context.Context, fileReader io.Reader, filename string) (*cashfree.SmartOCRResponse, error) {
	return c.inner.UploadAadhaarDocument(ctx, fileReader, filename)
}
