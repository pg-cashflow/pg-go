package kyc_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/aadhaar"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
)

// ---- Fakes ------------------------------------------------------------------

func TestGetStatusRejectsExpiredVerificationWithoutExpiryJob(t *testing.T) {
	for _, expired := range []bool{true, false} {
		expires := time.Now().Add(time.Hour)
		if expired {
			expires = time.Now().Add(-time.Hour)
		}
		repo := &fakeRepo{verification: &domain.KYCVerification{Status: domain.KYCStatusVerified, ExpiresAt: &expires}}
		svc := kycsvc.NewService(repo, nil, kycsvc.Config{})
		v, _, err := svc.GetStatus(context.Background(), uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		if expired && v.Status != domain.KYCStatusExpired {
			t.Fatal("expired KYC still passed the read-time gate")
		}
		if !expired && v.Status != domain.KYCStatusVerified {
			t.Fatal("valid KYC rejected")
		}
		if repo.verification.Status != domain.KYCStatusVerified {
			t.Fatal("read-time projection mutated repository state")
		}
	}
}

type fakeRepo struct {
	consent                 *domain.KYCConsent
	verification            *domain.KYCVerification
	latestVerif             *domain.KYCVerification
	auditLogs               []domain.KYCAuditLog
	tenantName              string
	attestedPhoto           []byte
	docBytes                []byte
	docContentType          string
	lockAcquired            *bool
	lockAcquireErr          error
	lockReleaseErr          error
	releasedLockIDs         []uuid.UUID
	updateVendorRefErr      error
	updateVendorRefAttempts int
	recordConsentFn         func(c *domain.KYCConsent) error
	initiateVTxFn           func() (*domain.KYCVerification, error)
	completeVTxFn           func(id uuid.UUID) (*domain.KYCVerification, error)
	failVTxFn               func(id uuid.UUID, reason string) error
	clearDupFn              func(id uuid.UUID, reason string) error
	recordFailureAndCountFn func(reasonCode string) (int, error)
	otpAttemptCount         int
	countAndRecordOTPFn     func(maxAllowed int) (int, error)
	initialVendorRefID      string
	getByVendorRefErr       error
	getByCFRefErr           error
	altVerificationByCF     *domain.KYCVerification
	submitManualReviewCalls int
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
	if f.verification != nil {
		if f.verification.Status == domain.KYCStatusVerified {
			if f.verification.ExpiresAt == nil || time.Now().UTC().Before(*f.verification.ExpiresAt) {
				return nil, domain.ErrAlreadyVerified
			}
		} else if f.verification.Status == domain.KYCStatusPending ||
			f.verification.Status == domain.KYCStatusPendingManualReview ||
			f.verification.Status == domain.KYCStatusRequestUnknown {
			return nil, domain.ErrVerificationInProgress
		}
	}
	f.initialVendorRefID = vendorRefID
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
func (f *fakeRepo) UpdateVendorRefIDTx(_ context.Context, verificationID uuid.UUID, vendorRefID string) error {
	f.updateVendorRefAttempts++
	if f.updateVendorRefErr != nil {
		return f.updateVendorRefErr
	}
	if f.verification != nil && f.verification.ID == verificationID {
		f.verification.VendorReferenceID = vendorRefID
	}
	return nil
}

func (f *fakeRepo) CompleteVerificationTx(_ context.Context, id uuid.UUID, maskedUID, _ string, _ bool, _ int16, qrStatus *string, nameMismatch bool, attestedPhotoBytes []byte, _, _ time.Time, _ *time.Time, _ string, _ ...uuid.UUID) (*domain.KYCVerification, error) {
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
func (f *fakeRepo) FailVerificationTx(_ context.Context, id uuid.UUID, reason, _ string, _ ...uuid.UUID) error {
	if f.failVTxFn != nil {
		return f.failVTxFn(id, reason)
	}
	if f.verification != nil && f.verification.ID == id {
		f.verification.Status = domain.KYCStatusFailed
		f.verification.FailureReason = &reason
	}
	return nil
}

func (f *fakeRepo) MarkRequestUnknownTx(_ context.Context, id uuid.UUID, reason, _ string, _ ...uuid.UUID) error {
	if f.verification != nil && f.verification.ID == id {
		f.verification.Status = domain.KYCStatusRequestUnknown
		f.verification.FailureReason = &reason
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
	if f.getByVendorRefErr != nil {
		return nil, f.getByVendorRefErr
	}
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
func (f *fakeRepo) SubmitManualReviewTx(_ context.Context, id uuid.UUID, maskedUID, _ string, ocrData, docURL string, docBytes []byte, docContentType string, _ string, _ string, _ ...uuid.UUID) (*domain.KYCVerification, error) {
	f.submitManualReviewCalls++
	var raw struct {
		NameMismatch bool `json:"name_mismatch"`
	}
	_ = json.Unmarshal([]byte(ocrData), &raw)
	v := &domain.KYCVerification{
		ID:           id,
		Status:       domain.KYCStatusPendingManualReview,
		MaskedUID:    &maskedUID,
		OCRData:      &ocrData,
		DocumentURL:  &docURL,
		NameMismatch: raw.NameMismatch,
	}
	f.latestVerif = v
	if f.verification != nil && f.verification.ID == id {
		f.verification = v
	}
	f.docBytes = docBytes
	f.docContentType = docContentType
	return v, nil
}
func (f *fakeRepo) GetKYCDocument(_ context.Context, _ uuid.UUID) ([]byte, string, error) {
	if len(f.attestedPhoto) > 0 {
		return f.attestedPhoto, "image/jpeg", nil
	}
	if len(f.docBytes) > 0 {
		ct := f.docContentType
		if ct == "" {
			ct = "image/jp2"
		}
		return f.docBytes, ct, nil
	}
	return []byte("dummy-doc"), "application/pdf", nil
}
func (f *fakeRepo) VerifyPropertyOwnership(_ context.Context, _, _ uuid.UUID) (bool, error) {
	return true, nil
}
func (f *fakeRepo) ApproveManualReviewTx(_ context.Context, id, _, reviewerID uuid.UUID, _ string) (*domain.KYCVerification, error) {
	now := time.Now().UTC()
	v := &domain.KYCVerification{
		ID:         id,
		Status:     domain.KYCStatusVerified,
		ReviewedBy: &reviewerID,
		ReviewedAt: &now,
		VerifiedAt: &now,
	}
	f.latestVerif = v
	if f.verification != nil && f.verification.ID == id {
		f.verification = v
	}
	f.docBytes = nil
	f.docContentType = ""
	return v, nil
}
func (f *fakeRepo) RejectManualReviewTx(_ context.Context, id, _, reviewerID uuid.UUID, reason, _ string) (*domain.KYCVerification, error) {
	now := time.Now().UTC()
	v := &domain.KYCVerification{
		ID:              id,
		Status:          domain.KYCStatusRejected,
		ReviewedBy:      &reviewerID,
		ReviewedAt:      &now,
		RejectionReason: &reason,
	}
	f.latestVerif = v
	if f.verification != nil && f.verification.ID == id {
		f.verification = v
	}
	f.docBytes = nil
	f.docContentType = ""
	return v, nil
}
func (f *fakeRepo) ListPendingReviews(_ context.Context, _ uuid.UUID) ([]domain.KYCVerification, error) {
	if f.verification != nil && f.verification.Status == domain.KYCStatusPendingManualReview {
		return []domain.KYCVerification{*f.verification}, nil
	}
	return nil, nil
}
func (f *fakeRepo) GetVerificationByID(_ context.Context, id uuid.UUID) (*domain.KYCVerification, error) {
	if f.verification != nil && f.verification.ID == id {
		return f.verification, nil
	}
	return nil, domain.ErrVerificationNotFound
}
func (f *fakeRepo) RecordFailureAndCount(_ context.Context, _ uuid.UUID, _ domain.KYCMethod, reasonCode string, _ int) (int, error) {
	if f.recordFailureAndCountFn != nil {
		return f.recordFailureAndCountFn(reasonCode)
	}
	return 1, nil
}
func (f *fakeRepo) CountAndRecordOTPAttemptTx(_ context.Context, _ uuid.UUID, maxAllowed int, _ time.Duration) (int, error) {
	if f.countAndRecordOTPFn != nil {
		return f.countAndRecordOTPFn(maxAllowed)
	}
	if f.otpAttemptCount >= maxAllowed {
		return f.otpAttemptCount, domain.ErrDailyOTPLimitExceeded
	}
	f.otpAttemptCount++
	return f.otpAttemptCount, nil
}
func (f *fakeRepo) RefundOTPAttemptTx(_ context.Context, _ uuid.UUID) error {
	if f.otpAttemptCount > 0 {
		f.otpAttemptCount--
	}
	return nil
}
func (f *fakeRepo) RecordGuardianConsentTx(_ context.Context, tenantID uuid.UUID, guardianName, guardianPhone, relation, refID string, _ string) (*domain.Tenant, error) {
	now := time.Now().UTC()
	return &domain.Tenant{
		ID:                        tenantID,
		GuardianName:              &guardianName,
		GuardianPhone:             &guardianPhone,
		GuardianRelation:          &relation,
		GuardianKYCReferenceID:    &refID,
		GuardianConsentVerifiedAt: &now,
	}, nil
}
func (f *fakeRepo) GetVerificationByCFReferenceID(_ context.Context, cfRefID string) (*domain.KYCVerification, error) {
	if f.getByCFRefErr != nil {
		return nil, f.getByCFRefErr
	}
	if f.altVerificationByCF != nil && f.altVerificationByCF.CFReferenceID != nil && *f.altVerificationByCF.CFReferenceID == cfRefID {
		return f.altVerificationByCF, nil
	}
	if f.verification != nil && f.verification.CFReferenceID != nil && *f.verification.CFReferenceID == cfRefID {
		return f.verification, nil
	}
	return nil, domain.ErrVerificationNotFound
}
func (f *fakeRepo) UpdateCFReferenceIDTx(_ context.Context, _ uuid.UUID, cfRefID string) error {
	if f.verification != nil {
		f.verification.CFReferenceID = &cfRefID
	}
	return nil
}

type fakeCashfree struct {
	ocrResp           *cashfree.SmartOCRResponse
	ocrErr            error
	ocrCalled         bool
	requestOTPErr     error
	requestOTPResp    *cashfree.AadhaarOTPInitResult
	requestOTPCalled  bool
	verifyOTPErr      error
	verifyOTPResp     *cashfree.AadhaarOTPVerifyResult
	digiLockerSession *cashfree.DigiLockerSession
	digiLockerErr     error
	digiLockerStatus  *cashfree.DigiLockerStatusResult
	digiLockerDoc     *cashfree.DigiLockerAadhaar
}

func (f *fakeCashfree) CreateDigiLockerURL(_ context.Context, in cashfree.CreateDigiLockerURLInput) (*cashfree.DigiLockerSession, error) {
	if f.digiLockerErr != nil {
		return nil, f.digiLockerErr
	}
	if f.digiLockerSession != nil {
		return f.digiLockerSession, nil
	}
	return &cashfree.DigiLockerSession{
		VerificationID: in.VerificationID,
		ReferenceID:    "12345",
		URL:            "https://verification.cashfree.com/dgl/mock123",
		Status:         cashfree.DigiLockerPending,
	}, nil
}
func (f *fakeCashfree) GetDigiLockerStatus(_ context.Context, verificationID string) (*cashfree.DigiLockerStatusResult, error) {
	if f.digiLockerErr != nil {
		return nil, f.digiLockerErr
	}
	if f.digiLockerStatus != nil {
		return f.digiLockerStatus, nil
	}
	return &cashfree.DigiLockerStatusResult{
		VerificationID: verificationID,
		ReferenceID:    "12345",
		Status:         cashfree.DigiLockerAuthenticated,
	}, nil
}
func (f *fakeCashfree) GetDigiLockerAadhaar(_ context.Context, verificationID string) (*cashfree.DigiLockerAadhaar, error) {
	if f.digiLockerErr != nil {
		return nil, f.digiLockerErr
	}
	if f.digiLockerDoc != nil {
		return f.digiLockerDoc, nil
	}
	return &cashfree.DigiLockerAadhaar{
		VerificationID: verificationID,
		ReferenceID:    "12345",
		Status:         cashfree.DigiLockerDocSuccess,
		MaskedUID:      "xxxxxxxx1234",
		Name:           "Ravi Kumar",
		DOB:            "15-06-1995",
		Gender:         "M",
	}, nil
}

func (f *fakeCashfree) UploadAadhaarDocument(_ context.Context, _ io.Reader, _ string) (*cashfree.SmartOCRResponse, error) {
	f.ocrCalled = true
	return f.ocrResp, f.ocrErr
}
func (f *fakeCashfree) UploadDocument(_ context.Context, _ io.Reader, _ string, _ string, _ ...string) (*cashfree.SmartOCRResponse, error) {
	f.ocrCalled = true
	return f.ocrResp, f.ocrErr
}
func (f *fakeCashfree) RequestAadhaarOTP(_ context.Context, _ string) (*cashfree.AadhaarOTPInitResult, error) {
	f.requestOTPCalled = true
	if f.requestOTPErr != nil {
		return nil, f.requestOTPErr
	}
	if f.requestOTPResp != nil {
		return f.requestOTPResp, nil
	}
	return &cashfree.AadhaarOTPInitResult{
		RefID:   "123456",
		Status:  "SUCCESS",
		Message: "OTP sent successfully",
	}, nil
}
func (f *fakeCashfree) VerifyAadhaarOTP(_ context.Context, refID, _ string) (*cashfree.AadhaarOTPVerifyResult, error) {
	if f.verifyOTPErr != nil {
		return nil, f.verifyOTPErr
	}
	if f.verifyOTPResp != nil {
		return f.verifyOTPResp, nil
	}
	return &cashfree.AadhaarOTPVerifyResult{
		RefID:     refID,
		Status:    "VALID",
		Name:      "Ravi Kumar",
		DOB:       "1990-01-01",
		Gender:    "M",
		MaskedUID: "1234",
	}, nil
}

func newTestService(repo *fakeRepo, cf kycsvc.CashfreeKYCClient) *kycsvc.Service {
	return kycsvc.NewService(repo, cf, kycsvc.Config{
		IdentitySecret:           "test-secret-key",
		HashKeyVersion:           1,
		VerificationValidityDays: 365,
	})
}

func TestInitiateAadhaarOTP_ProviderFaultRefundsQuota(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		otpAttemptCount: 1,
	}
	cf := &fakeCashfree{
		requestOTPErr: &cashfree.APIError{
			StatusCode: 500,
			Body:       "internal gateway server error",
		},
	}
	svc := newTestService(repo, cf)

	checkDigit, _ := aadhaar.GenerateVerhoeffCheckDigit("29384756102")
	validAadhaar := fmt.Sprintf("29384756102%d", checkDigit)

	_, err := svc.InitiateAadhaarOTP(context.Background(), tenantID, validAadhaar, "tenant:"+tenantID.String())
	if err == nil {
		t.Fatal("expected provider error, got nil")
	}
	// Initial count was 1, CountAndRecord incremented to 2, provider error should refund back to 1
	if repo.otpAttemptCount != 1 {
		t.Fatalf("expected attempt count to be refunded to 1, got %d", repo.otpAttemptCount)
	}
}

func TestCompleteAadhaarOTP_DisabledByADR005(t *testing.T) {
	tenantID := uuid.New()
	svc := newTestService(&fakeRepo{}, &fakeCashfree{})
	_, err := svc.CompleteAadhaarOTP(context.Background(), tenantID, "ref_100", "654321", "tenant:"+tenantID.String())
	if !errors.Is(err, kycsvc.ErrAadhaarOTPDisabled) {
		t.Fatalf("expected ErrAadhaarOTPDisabled, got %v", err)
	}
}

func TestRecordConsent(t *testing.T) {
	repo := &fakeRepo{}
	svc := newTestService(repo, &fakeCashfree{})
	tenantID := uuid.New()

	c, err := svc.RecordConsent(context.Background(), tenantID,
		"aadhaar_kyc", "v1.0", "I consent to Aadhaar verification",
		"127.0.0.1", "Mozilla/5.0", "tenant:abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.ID == uuid.Nil {
		t.Error("consent ID should be set after recording")
	}
	if c.ConsentTextHash == "" {
		t.Error("consent text hash should not be empty")
	}

	// 1. Stable hash: calling again with same text generates exact same hash
	expectedHash := c.ConsentTextHash

	// 2. Idempotency: when active consent matches version, purpose, and hash, returns existing
	repo.consent = c
	c2, err := svc.RecordConsent(context.Background(), tenantID,
		"aadhaar_kyc", "v1.0", "I consent to Aadhaar verification", "127.0.0.1", "Mozilla/5.0", "tenant:abc")
	if err != nil {
		t.Fatalf("unexpected error on idempotent call: %v", err)
	}
	if c2.ID != c.ID {
		t.Errorf("expected idempotent consent to return same ID %s, got %s", c.ID, c2.ID)
	}
	if c2.ConsentTextHash != expectedHash {
		t.Errorf("expected stable hash %s, got %s", expectedHash, c2.ConsentTextHash)
	}

	// 3. Different purpose or language (text hash) does NOT reuse previous consent
	c3, err := svc.RecordConsent(context.Background(), tenantID,
		"aadhaar_kyc", "v1.0", "నన్ను ధృవీకరించడానికి నేను సమ్మతిస్తున్నాను", "127.0.0.1", "Mozilla/5.0", "tenant:abc")
	if err != nil {
		t.Fatalf("unexpected error recording new language consent: %v", err)
	}
	if c3.ID == c.ID {
		t.Errorf("expected different consent record for new language text, got same ID %s", c3.ID)
	}
	if c3.ConsentTextHash == expectedHash {
		t.Errorf("expected different hash for Telugu text, got %s", c3.ConsentTextHash)
	}

	// 4. Unwhitelisted purpose is strictly rejected
	_, err = svc.RecordConsent(context.Background(), tenantID,
		"unapproved_marketing_purpose", "v1.0", "Some text", "127.0.0.1", "Mozilla/5.0", "tenant:abc")
	if !errors.Is(err, kycsvc.ErrInvalidConsentPurpose) {
		t.Errorf("expected ErrInvalidConsentPurpose for unapproved purpose, got %v", err)
	}
}

func TestVerifySecureQR_NoConsent(t *testing.T) {
	repo := &fakeRepo{}
	svc := newTestService(repo, &fakeCashfree{})

	// Any raw QR string Ã¢â‚¬â€ should fail on consent gate before QR decode.
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

// TestVerifySecureQR_BypassRejection covers all KYC-01 bypass vectors.
// Every non-Secure-QR payload must be rejected before writing any DB record.
func TestVerifySecureQR_BypassRejection(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{ID: uuid.New(), TenantID: tenantID},
	}
	svc := newTestService(repo, &fakeCashfree{})

	tests := []struct {
		name    string
		payload string
	}{
		{
			name:    "xml_like_with_valid_fields",
			payload: `<PrintLetterBarcodeData uid="123412341234" name="Ravi Kumar" gender="M" dob="1990-01-01" />`,
		},
		{
			name:    "attribute_style_payload",
			payload: `name="Ravi Kumar" uid="123412341234" gender="M" dob="1990-01-01"`,
		},
		{
			name:    "plain_text_with_real_looking_data",
			payload: "Ravi Kumar DOB:1990-01-01 Gender:M UID:xxxx1234",
		},
		{
			name:    "empty_payload",
			payload: "",
		},
		{
			// All-digits but short — does not pass looksLikeSecureQR (len < 50),
			// so rejected immediately as ErrQRSignatureInvalid.
			name:    "all_digits_short_no_key",
			payload: "12345678901234567890",
		},
		{
			// All-digits, long enough to look like Secure QR, but no key configured → Verified=false.
			name:    "all_digits_long_no_key_configured",
			payload: "12345678901234567890123456789012345678901234567890123456789012345678901234567890",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.VerifySecureQR(context.Background(), tenantID, tc.payload, "tenant:abc")
			if err == nil {
				t.Fatalf("expected error for bypass payload %q, got nil", tc.name)
			}
			if !errors.Is(err, kycsvc.ErrQRSignatureInvalid) && !errors.Is(err, kycsvc.ErrQRDataIncomplete) {
				t.Errorf("expected ErrQRSignatureInvalid or ErrQRDataIncomplete, got %v", err)
			}
			// Critically: verify no DB write happened (no pending verification created).
			if repo.verification != nil {
				t.Errorf("bypass payload %q must not create any KYC verification record", tc.name)
			}
		})
	}
}

func TestVerifySecureQR_RoutesToManualReviewAndPurgesOnApproval(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if err := aadhaar.SetSecureQRPublicKeyPEM(string(pemBytes)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = aadhaar.SetSecureQRPublicKeyPEM("") })

	tenantID := uuid.New()
	propID := uuid.New()
	reviewerID := uuid.New()

	repo := &fakeRepo{
		tenantName: "Ravi Kumar",
		consent: &domain.KYCConsent{
			ID:        uuid.New(),
			TenantID:  tenantID,
			RevokedAt: nil,
		},
	}
	svc := newTestService(repo, &fakeCashfree{})

	// 1. Build a valid Secure QR payload with embedded J2K photo bytes
	j2kPhoto := []byte{0xFF, 0x4F, 0xFF, 0x51, 0x11, 0x22, 0xFF, 0x33, 0x44, 0xFF, 0xD9}
	var data []byte
	data = append(data, 0) // no email/mobile
	data = append(data, []byte("555544443333")...)
	data = append(data, 255)
	data = append(data, []byte("Ravi Kumar")...) // clean name match
	data = append(data, 255)
	data = append(data, []byte("M")...)
	data = append(data, 255)
	data = append(data, []byte("1994")...)
	data = append(data, 255) // separator before photo
	data = append(data, j2kPhoto...)

	sum := sha256.Sum256(data)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	buf := append(append([]byte{}, data...), sig...)
	rawQR := new(big.Int).SetBytes(buf).Text(10)

	// 2. Execute VerifySecureQR
	verif, err := svc.VerifySecureQR(context.Background(), tenantID, rawQR, "tenant:ravi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3. Assert it does NOT auto-verify, but lands in pending_manual_review
	if verif.Status != domain.KYCStatusPendingManualReview {
		t.Fatalf("expected status %s, got %s", domain.KYCStatusPendingManualReview, verif.Status)
	}
	if !bytes.Equal(repo.docBytes, j2kPhoto) {
		t.Fatalf("expected photo bytes %x, got %x", j2kPhoto, repo.docBytes)
	}
	if repo.docContentType != "image/jp2" {
		t.Fatalf("expected content type image/jp2, got %s", repo.docContentType)
	}

	// 4. Assert owner can inspect document photo bytes before decision
	docBytes, ct, err := svc.GetKYCDocument(context.Background(), verif.ID, propID)
	if err != nil {
		t.Fatalf("failed to retrieve kyc document: %v", err)
	}
	if !bytes.Equal(docBytes, j2kPhoto) || ct != "image/jp2" {
		t.Fatalf("GetKYCDocument mismatch: got ct=%s, bytes=%x", ct, docBytes)
	}

	// 5. Owner approves manual review
	approved, err := svc.ApproveManualReview(context.Background(), verif.ID, propID, reviewerID, "owner:admin")
	if err != nil {
		t.Fatalf("ApproveManualReview failed: %v", err)
	}
	if approved.Status != domain.KYCStatusVerified {
		t.Fatalf("expected status %s, got %s", domain.KYCStatusVerified, approved.Status)
	}

	// 6. DPDP data minimisation: photo bytes must be purged to nil upon approval
	if repo.docBytes != nil {
		t.Fatalf("expected photo bytes to be purged to nil after approval, got %x", repo.docBytes)
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

	v, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("%PDF-1.4 dummy pdf"), "aadhaar.pdf", "application/pdf", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Status != domain.KYCStatusPendingManualReview {
		t.Fatalf("expected status pending_manual_review, got %s", v.Status)
	}
	if v.NameMismatch {
		t.Errorf("expected name_mismatch=false for exact name match")
	}

	// Owner approves the manual review
	reviewerID := uuid.New()
	approved, err := svc.ApproveManualReview(context.Background(), v.ID, uuid.New(), reviewerID, "owner:admin")
	if err != nil {
		t.Fatalf("approve manual review error: %v", err)
	}
	if approved.Status != domain.KYCStatusVerified {
		t.Fatalf("expected status verified after approval, got %s", approved.Status)
	}
}

func TestVerifyAadhaarDocument_VendorRefPersistenceFailurePreservesRequestUnknown(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		tenantName:         "Ravi Kumar",
		updateVendorRefErr: errors.New("db connection lost"),
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

	v, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("%PDF-1.4 dummy pdf"), "aadhaar.pdf", "application/pdf", "tenant:"+tenantID.String())
	if err == nil {
		t.Fatalf("expected error on vendor ref persistence failure, got nil")
	}
	if v == nil || v.Status != domain.KYCStatusRequestUnknown {
		t.Fatalf("expected verification with request_unknown status, got %+v", v)
	}
	if repo.updateVendorRefAttempts != 3 {
		t.Fatalf("expected 3 updateVendorRef attempts with retry, got %d", repo.updateVendorRefAttempts)
	}
	if repo.verification == nil || repo.verification.Status != domain.KYCStatusRequestUnknown {
		t.Fatalf("expected verification to be preserved as request_unknown, got %+v", repo.verification)
	}
	if repo.verification.FailureReason == nil || !strings.Contains(*repo.verification.FailureReason, "VENDOR_REF_PERSISTENCE_FAILED") {
		t.Fatalf("expected failure reason to contain VENDOR_REF_PERSISTENCE_FAILED, got %v", repo.verification.FailureReason)
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

	v, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("%PDF-1.4 dummy pdf"), "aadhaar.pdf", "application/pdf", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.NameMismatch {
		t.Errorf("expected name_mismatch=true for different names")
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

	_, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("%PDF-1.4 dummy"), "aadhaar.pdf", "application/pdf", "tenant:"+tenantID.String())
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

	_, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("%PDF-1.4 dummy"), "aadhaar.pdf", "application/pdf", "tenant:"+tenantID.String())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(repo.releasedLockIDs) != 1 || repo.releasedLockIDs[0] != tenantID {
		t.Fatalf("expected lease lock to be released via defer on error, got released IDs: %v", repo.releasedLockIDs)
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

	_, err := svc.VerifyAadhaarDocument(context.Background(), tenantID, strings.NewReader("\xff\xd8\xff dummy"), "card.jpg", "image/jpeg", "tenant:"+tenantID.String())
	if !errors.Is(err, domain.ErrVerificationInProgress) {
		t.Fatalf("expected ErrVerificationInProgress on cooldown, got %v", err)
	}
	if len(repo.releasedLockIDs) != 1 || repo.releasedLockIDs[0] != tenantID {
		t.Fatalf("expected lease lock to be released on cooldown return, got: %v", repo.releasedLockIDs)
	}
}

func TestService_VerifyDocument_ActiveStateGuards(t *testing.T) {
	cases := []struct {
		name      string
		status    domain.KYCStatus
		age       time.Duration
		method    domain.KYCMethod
		expectErr error
	}{
		{
			name:      "pending at 1 second blocks new attempt",
			status:    domain.KYCStatusPending,
			age:       1 * time.Second,
			expectErr: domain.ErrVerificationInProgress,
		},
		{
			name:      "pending at 61 seconds blocks new attempt",
			status:    domain.KYCStatusPending,
			age:       61 * time.Second,
			expectErr: domain.ErrVerificationInProgress,
		},
		{
			name:      "pending at 24 hours blocks new attempt",
			status:    domain.KYCStatusPending,
			age:       24 * time.Hour,
			expectErr: domain.ErrVerificationInProgress,
		},
		{
			name:      "request_unknown blocks new attempt",
			status:    domain.KYCStatusRequestUnknown,
			age:       5 * time.Minute,
			expectErr: domain.ErrVerificationInProgress,
		},
		{
			name:      "pending_manual_review blocks new attempt",
			status:    domain.KYCStatusPendingManualReview,
			age:       10 * time.Minute,
			expectErr: domain.ErrVerificationInProgress,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
					Status:    tc.status,
					CreatedAt: time.Now().UTC().Add(-tc.age),
				},
			}
			cf := &fakeCashfree{}
			svc := newTestService(repo, cf)

			// 1. OCR attempt blocked
			_, err := svc.VerifyDocument(context.Background(), tenantID, strings.NewReader("\xff\xd8\xff dummy"), "card.jpg", "image/jpeg", "aadhaar", "tenant:"+tenantID.String())
			if !errors.Is(err, tc.expectErr) {
				t.Fatalf("VerifyDocument: expected %v, got %v", tc.expectErr, err)
			}
			if cf.ocrCalled {
				t.Fatal("expected Cashfree OCR to not be called when active verification exists")
			}

			// 2. Aadhaar OTP attempt blocked
			_, err = svc.InitiateAadhaarOTP(context.Background(), tenantID, "999999999999", "tenant:"+tenantID.String())
			if !errors.Is(err, tc.expectErr) {
				t.Fatalf("InitiateAadhaarOTP: expected %v, got %v", tc.expectErr, err)
			}
			if cf.requestOTPCalled {
				t.Fatal("expected Cashfree OTP to not be called when active verification exists")
			}
		})
	}
}

func TestVerifyDocument_PostProviderPersistenceFailure_PreservesRequestUnknown(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       consentID,
			TenantID: tenantID,
		},
		updateVendorRefErr: errors.New("db connection failure on vendor ref"),
	}
	cf := &fakeCashfree{
		ocrResp: &cashfree.SmartOCRResponse{
			ReferenceID: "cf_mock_ref_12345",
			Status:      "SUCCESS",
			Name:        "Test Tenant",
			MaskedUID:   "XXXXXXXX1234",
		},
	}
	svc := newTestService(repo, cf)

	verif, err := svc.VerifyDocument(context.Background(), tenantID, strings.NewReader("\xff\xd8\xff dummy"), "card.jpg", "image/jpeg", "aadhaar", "tenant:"+tenantID.String())
	if err == nil {
		t.Fatal("expected error on persistence failure, got nil")
	}

	// Must preserve request_unknown status, NOT failed
	if verif == nil {
		t.Fatal("expected non-nil verification record returned with request_unknown")
	}
	if verif.Status != domain.KYCStatusRequestUnknown {
		t.Fatalf("expected status %s, got %s", domain.KYCStatusRequestUnknown, verif.Status)
	}

	// Verify repo verification record was transitioned to request_unknown, NOT failed
	if repo.verification.Status != domain.KYCStatusRequestUnknown {
		t.Fatalf("expected repo status to be request_unknown, got %s", repo.verification.Status)
	}
	if repo.verification.FailureReason == nil || !strings.Contains(*repo.verification.FailureReason, "VENDOR_REF_PERSISTENCE_FAILED") {
		t.Fatalf("expected FailureReason to contain VENDOR_REF_PERSISTENCE_FAILED, got %v", repo.verification.FailureReason)
	}
}

func TestInitiateAadhaarOTP_AmbiguousTimeoutOr5xx_PreservesRequestUnknownAndBlocksRetry(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       consentID,
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{
		requestOTPErr: &cashfree.APIError{
			StatusCode: 504,
			Body:       "gateway timeout contacting uidai",
		},
	}
	svc := newTestService(repo, cf)

	checkDigit, _ := aadhaar.GenerateVerhoeffCheckDigit("29384756102")
	validAadhaar := fmt.Sprintf("29384756102%d", checkDigit)

	_, err := svc.InitiateAadhaarOTP(context.Background(), tenantID, validAadhaar, "tenant:"+tenantID.String())
	if err == nil {
		t.Fatal("expected error on 504 gateway timeout, got nil")
	}

	if repo.verification == nil {
		t.Fatal("expected verification record in repo, got nil")
	}
	if repo.verification.Status != domain.KYCStatusRequestUnknown {
		t.Fatalf("expected status %s, got %s", domain.KYCStatusRequestUnknown, repo.verification.Status)
	}
	if repo.verification.FailureReason == nil || !strings.Contains(*repo.verification.FailureReason, "AMBIGUOUS_PROVIDER_OUTCOME") {
		t.Fatalf("expected FailureReason to contain AMBIGUOUS_PROVIDER_OUTCOME, got %v", repo.verification.FailureReason)
	}

	// Active state guard: subsequent attempt MUST be blocked with ErrVerificationInProgress
	_, retryErr := svc.InitiateAadhaarOTP(context.Background(), tenantID, validAadhaar, "tenant:"+tenantID.String())
	if !errors.Is(retryErr, domain.ErrVerificationInProgress) {
		t.Fatalf("expected ErrVerificationInProgress on retry while request_unknown, got %v", retryErr)
	}
}

func TestInitiateAadhaarOTP_VendorRefPersistenceFailure_PreservesRequestUnknown(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       consentID,
			TenantID: tenantID,
		},
		updateVendorRefErr: errors.New("database connection refused on vendor ref write"),
	}
	cf := &fakeCashfree{
		requestOTPResp: &cashfree.AadhaarOTPInitResult{
			RefID:   "998877",
			Status:  "SUCCESS",
			Message: "OTP sent",
		},
	}
	svc := newTestService(repo, cf)

	checkDigit, _ := aadhaar.GenerateVerhoeffCheckDigit("29384756102")
	validAadhaar := fmt.Sprintf("29384756102%d", checkDigit)

	_, err := svc.InitiateAadhaarOTP(context.Background(), tenantID, validAadhaar, "tenant:"+tenantID.String())
	if err == nil {
		t.Fatal("expected error on vendor ref persistence failure, got nil")
	}

	if repo.verification == nil {
		t.Fatal("expected verification record in repo, got nil")
	}
	if repo.verification.Status != domain.KYCStatusRequestUnknown {
		t.Fatalf("expected status %s, got %s", domain.KYCStatusRequestUnknown, repo.verification.Status)
	}
	if repo.verification.FailureReason == nil || !strings.Contains(*repo.verification.FailureReason, "VENDOR_REF_PERSISTENCE_FAILED") {
		t.Fatalf("expected FailureReason to contain VENDOR_REF_PERSISTENCE_FAILED, got %v", repo.verification.FailureReason)
	}
}

func TestInitiateDigiLocker_API500_PreservesRequestUnknown(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       consentID,
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{
		digiLockerErr: &cashfree.APIError{
			StatusCode: 500,
			Body:       "internal provider failure creating session",
		},
	}
	svc := newTestService(repo, cf)

	verif, _, err := svc.InitiateDigiLockerVerification(context.Background(), tenantID, "https://example.com/return", "tenant:"+tenantID.String(), "")
	if err == nil {
		t.Fatal("expected error on DigiLocker 500, got nil")
	}

	if verif == nil || verif.Status != domain.KYCStatusRequestUnknown {
		t.Fatalf("expected returned verification to be request_unknown, got %+v", verif)
	}
	if repo.verification == nil || repo.verification.Status != domain.KYCStatusRequestUnknown {
		t.Fatalf("expected repo verification status request_unknown, got %+v", repo.verification)
	}
}

func TestVerifyDocument_CanonicalCorrelationIDPersistedBeforeDispatch(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       consentID,
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{
		ocrResp: &cashfree.SmartOCRResponse{
			ReferenceID: "cf_mock_ref_9988",
			Status:      "SUCCESS",
			Name:        "Test Tenant",
			MaskedUID:   "XXXXXXXX1234",
		},
	}
	svc := newTestService(repo, cf)

	_, err := svc.VerifyDocument(context.Background(), tenantID, strings.NewReader("\xff\xd8\xff dummy"), "card.jpg", "image/jpeg", "aadhaar", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if repo.verification == nil {
		t.Fatal("expected repo verification, got nil")
	}
	if !strings.HasPrefix(repo.initialVendorRefID, "cf_ocr_") {
		t.Errorf("expected initial persisted vendorRefID to have prefix cf_ocr_, got %q", repo.initialVendorRefID)
	}
}

func TestInitiateAadhaarOTP_HappyPath(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       consentID,
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{}
	svc := newTestService(repo, cf)

	checkDigit, err := aadhaar.GenerateVerhoeffCheckDigit("29384756102")
	if err != nil {
		t.Fatalf("failed to generate check digit: %v", err)
	}
	validAadhaar := fmt.Sprintf("29384756102%d", checkDigit)

	refID, err := svc.InitiateAadhaarOTP(context.Background(), tenantID, validAadhaar, "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("unexpected error initiating aadhaar otp: %v", err)
	}
	if refID != "123456" {
		t.Errorf("expected refID '123456', got %s", refID)
	}
	if repo.verification == nil || repo.verification.Method != domain.KYCMethodAadhaarOTP {
		t.Errorf("expected verification method aadhaar_otp, got %v", repo.verification)
	}

	// Verify invalid Aadhaar numbers fail fast without calling gateway
	_, err = svc.InitiateAadhaarOTP(context.Background(), tenantID, "123456789012", "tenant:"+tenantID.String())
	if !errors.Is(err, kycsvc.ErrInvalidAadhaarNumber) {
		t.Errorf("expected ErrInvalidAadhaarNumber for invalid aadhaar, got %v", err)
	}
}

func TestInitiateAadhaarOTP_DailyLimitExceeded(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		otpAttemptCount: domain.MaxDailyAadhaarOTPRequests, // already at limit
	}
	cf := &fakeCashfree{}
	svc := newTestService(repo, cf)

	checkDigit, _ := aadhaar.GenerateVerhoeffCheckDigit("29384756102")
	validAadhaar := fmt.Sprintf("29384756102%d", checkDigit)

	_, err := svc.InitiateAadhaarOTP(context.Background(), tenantID, validAadhaar, "tenant:"+tenantID.String())
	if !errors.Is(err, domain.ErrDailyOTPLimitExceeded) {
		t.Fatalf("expected ErrDailyOTPLimitExceeded when at quota, got %v", err)
	}
	if cf.requestOTPCalled {
		t.Fatal("expected Cashfree RequestAadhaarOTP NOT to be called when daily quota is exceeded")
	}
}

func TestInitiateAadhaarOTP_PersistRetryOnTransientError(t *testing.T) {
	tenantID := uuid.New()
	attempts := 0
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		initiateVTxFn: func() (*domain.KYCVerification, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("transient db network hiccup")
			}
			return &domain.KYCVerification{
				ID:                uuid.New(),
				TenantID:          tenantID,
				Method:            domain.KYCMethodAadhaarOTP,
				VendorReferenceID: "cf_aadhaar_otp_123456",
				Status:            domain.KYCStatusPending,
			}, nil
		},
	}
	cf := &fakeCashfree{}
	svc := newTestService(repo, cf)

	checkDigit, _ := aadhaar.GenerateVerhoeffCheckDigit("29384756102")
	validAadhaar := fmt.Sprintf("29384756102%d", checkDigit)

	refID, err := svc.InitiateAadhaarOTP(context.Background(), tenantID, validAadhaar, "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if refID != "123456" {
		t.Fatalf("expected refID '123456', got %s", refID)
	}
	if attempts != 2 {
		t.Fatalf("expected exactly 2 initiate attempts (1 failure + 1 retry), got %d", attempts)
	}
}

func TestInitiateAadhaarOTP_CashfreeValidation(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()

	baseRepo := func() *fakeRepo {
		return &fakeRepo{
			consent: &domain.KYCConsent{
				ID:       consentID,
				TenantID: tenantID,
			},
		}
	}

	checkDigit, _ := aadhaar.GenerateVerhoeffCheckDigit("29384756102")
	validAadhaar := fmt.Sprintf("29384756102%d", checkDigit)

	t.Run("Aadhaar not linked to mobile returns 200 SUCCESS but fails closed to MOBILE_NOT_LINKED without creating DB record", func(t *testing.T) {
		repo := baseRepo()
		cf := &fakeCashfree{
			requestOTPResp: &cashfree.AadhaarOTPInitResult{
				RefID:   "",
				Status:  "SUCCESS",
				Message: "Aadhaar not linked to mobile number",
			},
		}
		svc := newTestService(repo, cf)

		_, err := svc.InitiateAadhaarOTP(context.Background(), tenantID, validAadhaar, "tenant:"+tenantID.String())
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var fail *kycsvc.AadhaarVerifyFailure
		if !errors.As(err, &fail) {
			t.Fatalf("expected AadhaarVerifyFailure, got %T: %v", err, err)
		}
		if fail.ReasonCode != "MOBILE_NOT_LINKED" {
			t.Errorf("expected ReasonCode MOBILE_NOT_LINKED, got %s", fail.ReasonCode)
		}
		if fail.Class != cashfree.FailureTerminal {
			t.Errorf("expected FailureTerminal, got %v", fail.Class)
		}
		if fail.Retryable {
			t.Errorf("expected Retryable false, got true")
		}
		if repo.verification == nil || repo.verification.Status != domain.KYCStatusFailed {
			t.Errorf("expected verification record to be marked failed, but got: %v", repo.verification)
		}
	})

	t.Run("Invalid Aadhaar Card message in 200 SUCCESS maps to INVALID_AADHAAR and applies cooldown without creating DB record", func(t *testing.T) {
		repo := baseRepo()
		cf := &fakeCashfree{
			requestOTPResp: &cashfree.AadhaarOTPInitResult{
				RefID:   "",
				Status:  "SUCCESS",
				Message: "Invalid Aadhaar Card",
			},
		}
		svc := newTestService(repo, cf)

		_, err := svc.InitiateAadhaarOTP(context.Background(), tenantID, validAadhaar, "tenant:"+tenantID.String())
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var fail *kycsvc.AadhaarVerifyFailure
		if !errors.As(err, &fail) {
			t.Fatalf("expected AadhaarVerifyFailure, got %T: %v", err, err)
		}
		if fail.ReasonCode != "INVALID_AADHAAR" {
			t.Errorf("expected ReasonCode INVALID_AADHAAR, got %s", fail.ReasonCode)
		}
		if fail.Class != cashfree.FailureRecoverable {
			t.Errorf("expected FailureRecoverable, got %v", fail.Class)
		}
		if fail.CooldownSeconds != 30 {
			t.Errorf("expected CooldownSeconds 30, got %d", fail.CooldownSeconds)
		}
		if repo.verification == nil || repo.verification.Status != domain.KYCStatusFailed {
			t.Errorf("expected verification record to be marked failed, but got: %v", repo.verification)
		}
	})

	t.Run("Empty ref_id in SUCCESS response fails closed and marks attempt failed", func(t *testing.T) {
		repo := baseRepo()
		cf := &fakeCashfree{
			requestOTPResp: &cashfree.AadhaarOTPInitResult{
				RefID:   "",
				Status:  "SUCCESS",
				Message: "OTP sent successfully",
			},
		}
		svc := newTestService(repo, cf)

		_, err := svc.InitiateAadhaarOTP(context.Background(), tenantID, validAadhaar, "tenant:"+tenantID.String())
		if err == nil {
			t.Fatal("expected error for empty ref_id, got nil")
		}
		if repo.verification == nil || repo.verification.Status != domain.KYCStatusFailed {
			t.Errorf("expected verification record to be marked failed, but got: %v", repo.verification)
		}
	})
}

// ---- DigiLocker Tests (ASD-STE100 & Lauren Tan Trust Pyramid) ----------------

func TestInitiateDigiLockerVerification_Success(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       consentID,
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{
		digiLockerSession: &cashfree.DigiLockerSession{
			VerificationID: "cf_dgl_test",
			ReferenceID:    "cf_ref_999",
			URL:            "https://verification.cashfree.com/dgl/session123",
			Status:         cashfree.DigiLockerPending,
		},
	}
	svc := newTestService(repo, cf)

	v, url, err := svc.InitiateDigiLockerVerification(context.Background(), tenantID, "https://app.test/kyc/return", "signup", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("unexpected error initiating digilocker: %v", err)
	}
	if url != "https://verification.cashfree.com/dgl/session123" {
		t.Errorf("unexpected redirect url: %s", url)
	}
	if v == nil || v.Status != domain.KYCStatusPending {
		t.Fatalf("expected pending verification record, got %+v", v)
	}
	if v.Method != domain.KYCMethodDigiLocker {
		t.Errorf("expected method digilocker, got %s", v.Method)
	}
	if v.CFReferenceID == nil || *v.CFReferenceID != "cf_ref_999" {
		t.Errorf("expected cf_reference_id cf_ref_999, got %v", v.CFReferenceID)
	}
}

func TestInitiateDigiLockerVerification_MissingConsent_FailsClosed(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		consent: nil, // no active consent
	}
	svc := newTestService(repo, &fakeCashfree{})

	_, _, err := svc.InitiateDigiLockerVerification(context.Background(), tenantID, "https://app.test/kyc/return", "", "tenant:"+tenantID.String())
	if !errors.Is(err, kycsvc.ErrNoActiveConsent) {
		t.Fatalf("expected ErrNoActiveConsent, got %v", err)
	}
}

func TestInitiateDigiLockerVerification_AmbiguousTimeout_SetsRequestUnknown(t *testing.T) {
	tenantID := uuid.New()
	consentID := uuid.New()
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       consentID,
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{
		digiLockerErr: context.DeadlineExceeded,
	}
	svc := newTestService(repo, cf)

	v, url, err := svc.InitiateDigiLockerVerification(context.Background(), tenantID, "https://app.test/kyc/return", "signup", "tenant:"+tenantID.String())
	if err == nil {
		t.Fatalf("expected error on timeout, got nil")
	}
	if url != "" {
		t.Errorf("expected empty url, got %s", url)
	}
	if v == nil || v.Status != domain.KYCStatusRequestUnknown {
		t.Fatalf("expected verification status request_unknown, got %+v", v)
	}
	if repo.verification.Status != domain.KYCStatusRequestUnknown {
		t.Fatalf("expected repo verification status request_unknown, got %s", repo.verification.Status)
	}
}

func TestProcessDigiLockerCallback_All5Events(t *testing.T) {
	tenantID := uuid.New()
	verifID := uuid.New()
	vendorRef := "cf_dgl_abc"
	cfRef := "cf_ref_777"

	tests := []struct {
		name       string
		event      string
		status     string
		wantStatus domain.KYCStatus
		wantReason string
	}{
		{
			name:       "link expired webhook",
			event:      domain.WebhookEventDigiLockerLinkExpired,
			wantStatus: domain.KYCStatusFailed,
			wantReason: "LINK_EXPIRED",
		},
		{
			name:       "consent denied webhook",
			event:      domain.WebhookEventDigiLockerConsentDenied,
			wantStatus: domain.KYCStatusFailed,
			wantReason: "CONSENT_DENIED",
		},
		{
			name:       "consent expired webhook",
			event:      domain.WebhookEventDigiLockerConsentExpired,
			wantStatus: domain.KYCStatusFailed,
			wantReason: "CONSENT_EXPIRED",
		},
		{
			name:       "general failure webhook",
			event:      domain.WebhookEventDigiLockerFailure,
			wantStatus: domain.KYCStatusFailed,
			wantReason: "VERIFICATION_FAILURE",
		},
		{
			name:       "polling expired status",
			status:     string(cashfree.DigiLockerExpired),
			wantStatus: domain.KYCStatusFailed,
			wantReason: "LINK_EXPIRED",
		},
		{
			name:       "polling consent denied status",
			status:     string(cashfree.DigiLockerConsentDenied),
			wantStatus: domain.KYCStatusFailed,
			wantReason: "CONSENT_DENIED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{
				verification: &domain.KYCVerification{
					ID:                verifID,
					TenantID:          tenantID,
					VendorReferenceID: vendorRef,
					CFReferenceID:     &cfRef,
					Status:            domain.KYCStatusPending,
					Method:            domain.KYCMethodDigiLocker,
				},
			}
			svc := newTestService(repo, &fakeCashfree{})

			v, err := svc.ProcessDigiLockerCallback(context.Background(), vendorRef, cfRef, tt.event, tt.status, "system:webhook")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.Status != tt.wantStatus {
				t.Errorf("expected status %s, got %s", tt.wantStatus, v.Status)
			}
			if v.FailureReason == nil || *v.FailureReason != tt.wantReason {
				t.Errorf("expected failure reason %s, got %v", tt.wantReason, v.FailureReason)
			}
		})
	}
}

func TestProcessDigiLockerCallback_SuccessWithMatchingName_RoutesToManualReview(t *testing.T) {
	tenantID := uuid.New()
	verifID := uuid.New()
	vendorRef := "cf_dgl_success_1"
	cfRef := "cf_ref_1001"

	repo := &fakeRepo{
		tenantName: "Ravi Kumar",
		verification: &domain.KYCVerification{
			ID:                verifID,
			TenantID:          tenantID,
			VendorReferenceID: vendorRef,
			CFReferenceID:     &cfRef,
			Status:            domain.KYCStatusPending,
			Method:            domain.KYCMethodDigiLocker,
		},
	}
	cf := &fakeCashfree{
		digiLockerDoc: &cashfree.DigiLockerAadhaar{
			VerificationID: vendorRef,
			ReferenceID:    cfRef,
			Status:         cashfree.DigiLockerDocSuccess,
			Name:           "Ravi Kumar",
			DOB:            "12-05-1996",
			Gender:         "M",
			MaskedUID:      "xxxxxxxx8888",
		},
	}
	svc := newTestService(repo, cf)

	v, err := svc.ProcessDigiLockerCallback(context.Background(), vendorRef, cfRef, domain.WebhookEventDigiLockerSuccess, "", "system:webhook")
	if err != nil {
		t.Fatalf("unexpected callback error: %v", err)
	}
	if v.Status != domain.KYCStatusPendingManualReview {
		t.Fatalf("expected status pending_manual_review on matching name per ADR-005, got %s", v.Status)
	}
	if v.NameMismatch {
		t.Errorf("expected NameMismatch false when name matches")
	}
	if v.MaskedUID == nil || *v.MaskedUID != "xxxxxxxx8888" {
		t.Errorf("expected masked uid xxxxxxxx8888, got %v", v.MaskedUID)
	}
	canPay, payErr := svc.CanTenantPay(context.Background(), tenantID)
	if payErr != nil {
		t.Fatalf("unexpected CanTenantPay error: %v", payErr)
	}
	if canPay {
		t.Fatalf("expected CanTenantPay to return false while KYC is pending_manual_review")
	}
}

func TestProcessDigiLockerCallback_NameMismatch_EscalatesToManualReview(t *testing.T) {
	tenantID := uuid.New()
	verifID := uuid.New()
	vendorRef := "cf_dgl_mismatch_1"
	cfRef := "cf_ref_1002"

	repo := &fakeRepo{
		tenantName: "Ravi Kumar",
		verification: &domain.KYCVerification{
			ID:                verifID,
			TenantID:          tenantID,
			VendorReferenceID: vendorRef,
			CFReferenceID:     &cfRef,
			Status:            domain.KYCStatusPending,
			Method:            domain.KYCMethodDigiLocker,
		},
	}
	cf := &fakeCashfree{
		digiLockerDoc: &cashfree.DigiLockerAadhaar{
			VerificationID: vendorRef,
			ReferenceID:    cfRef,
			Status:         cashfree.DigiLockerDocSuccess,
			Name:           "Suresh Raina", // Name completely different from tenant profile
			DOB:            "12-05-1996",
			Gender:         "M",
			MaskedUID:      "xxxxxxxx9999",
		},
	}
	svc := newTestService(repo, cf)

	// STRICT INVARIANT: Name mismatch must route to pending_manual_review, NOT auto-verify!
	v, err := svc.ProcessDigiLockerCallback(context.Background(), vendorRef, cfRef, domain.WebhookEventDigiLockerSuccess, "", "system:webhook")
	if err != nil {
		t.Fatalf("unexpected callback error: %v", err)
	}
	if v.Status != domain.KYCStatusPendingManualReview {
		t.Fatalf("CRITICAL SECURITY INVARIANT VIOLATED: name mismatch auto-verified! Expected pending_manual_review, got %s", v.Status)
	}
	if !v.NameMismatch {
		t.Error("expected NameMismatch flag set to true")
	}
}

func TestProcessDigiLockerCallback_AadhaarNotLinked_FailsClosed(t *testing.T) {
	tenantID := uuid.New()
	verifID := uuid.New()
	vendorRef := "cf_dgl_not_linked"
	cfRef := "cf_ref_1003"

	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:                verifID,
			TenantID:          tenantID,
			VendorReferenceID: vendorRef,
			CFReferenceID:     &cfRef,
			Status:            domain.KYCStatusPending,
			Method:            domain.KYCMethodDigiLocker,
		},
	}
	cf := &fakeCashfree{
		digiLockerDoc: &cashfree.DigiLockerAadhaar{
			VerificationID: vendorRef,
			ReferenceID:    cfRef,
			Status:         cashfree.DigiLockerAadhaarNotLinked,
		},
	}
	svc := newTestService(repo, cf)

	v, err := svc.ProcessDigiLockerCallback(context.Background(), vendorRef, cfRef, domain.WebhookEventDigiLockerSuccess, "", "system:webhook")
	if err != nil {
		t.Fatalf("unexpected callback error: %v", err)
	}
	if v.Status != domain.KYCStatusFailed {
		t.Fatalf("expected status failed when aadhaar not linked, got %s", v.Status)
	}
	if v.FailureReason == nil || *v.FailureReason != "AADHAAR_NOT_LINKED" {
		t.Errorf("expected failure reason AADHAAR_NOT_LINKED, got %v", v.FailureReason)
	}
}

func TestCanTenantPay_GatingMatrix(t *testing.T) {
	tenantID := uuid.New()
	now := time.Now().UTC()
	future := now.Add(48 * time.Hour)
	past := now.Add(-48 * time.Hour)

	t.Run("verified tenant without expiry can pay", func(t *testing.T) {
		repo := &fakeRepo{
			verification: &domain.KYCVerification{
				TenantID: tenantID,
				Status:   domain.KYCStatusVerified,
			},
		}
		svc := newTestService(repo, &fakeCashfree{})
		canPay, err := svc.CanTenantPay(context.Background(), tenantID)
		if err != nil || !canPay {
			t.Fatalf("expected verified tenant without expiry to be allowed to pay, got canPay=%v, err=%v", canPay, err)
		}
	})

	t.Run("verified tenant with future expiry can pay", func(t *testing.T) {
		repo := &fakeRepo{
			verification: &domain.KYCVerification{
				TenantID:  tenantID,
				Status:    domain.KYCStatusVerified,
				ExpiresAt: &future,
			},
		}
		svc := newTestService(repo, &fakeCashfree{})
		canPay, err := svc.CanTenantPay(context.Background(), tenantID)
		if err != nil || !canPay {
			t.Fatalf("expected verified tenant with future expiry to be allowed to pay, got canPay=%v, err=%v", canPay, err)
		}
	})

	t.Run("verified tenant with past expiry cannot pay", func(t *testing.T) {
		repo := &fakeRepo{
			verification: &domain.KYCVerification{
				TenantID:  tenantID,
				Status:    domain.KYCStatusVerified,
				ExpiresAt: &past,
			},
		}
		svc := newTestService(repo, &fakeCashfree{})
		canPay, err := svc.CanTenantPay(context.Background(), tenantID)
		if err != nil || canPay {
			t.Fatalf("expected verified tenant with past expiry to be blocked from paying, got canPay=%v, err=%v", canPay, err)
		}
	})

	t.Run("verified tenant expired at now cannot pay", func(t *testing.T) {
		exactNow := time.Now().UTC()
		repo := &fakeRepo{
			verification: &domain.KYCVerification{
				TenantID:  tenantID,
				Status:    domain.KYCStatusVerified,
				ExpiresAt: &exactNow,
			},
		}
		svc := newTestService(repo, &fakeCashfree{})
		canPay, err := svc.CanTenantPay(context.Background(), tenantID)
		if err != nil || canPay {
			t.Fatalf("expected verified tenant expired at now to be blocked from paying, got canPay=%v, err=%v", canPay, err)
		}
	})

	t.Run("pending tenant cannot pay", func(t *testing.T) {
		repo := &fakeRepo{
			verification: &domain.KYCVerification{
				TenantID: tenantID,
				Status:   domain.KYCStatusPending,
			},
		}
		svc := newTestService(repo, &fakeCashfree{})
		canPay, err := svc.CanTenantPay(context.Background(), tenantID)
		if err != nil || canPay {
			t.Fatalf("expected pending tenant blocked from paying, got canPay=%v, err=%v", canPay, err)
		}
	})

	t.Run("manual review tenant cannot pay", func(t *testing.T) {
		repo := &fakeRepo{
			verification: &domain.KYCVerification{
				TenantID: tenantID,
				Status:   domain.KYCStatusPendingManualReview,
			},
		}
		svc := newTestService(repo, &fakeCashfree{})
		canPay, err := svc.CanTenantPay(context.Background(), tenantID)
		if err != nil || canPay {
			t.Fatalf("expected manual-review tenant blocked from paying, got canPay=%v, err=%v", canPay, err)
		}
	})
}

func TestProcessDigiLockerCallback_CorrelationAndErrorSafety(t *testing.T) {
	t.Run("preserves infrastructure error from vendor ref lookup instead of returning ErrVerificationNotFound", func(t *testing.T) {
		dbErr := errors.New("connection reset by peer")
		repo := &fakeRepo{getByVendorRefErr: dbErr}
		svc := newTestService(repo, &fakeCashfree{})

		_, err := svc.ProcessDigiLockerCallback(context.Background(), "cf_dgl_1", "cf_ref_1", domain.WebhookEventDigiLockerSuccess, "", "system:webhook")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if errors.Is(err, domain.ErrVerificationNotFound) {
			t.Fatalf("expected infrastructure error, got ErrVerificationNotFound: %v", err)
		}
		if !errors.Is(err, dbErr) {
			t.Fatalf("expected wrapped dbErr, got %v", err)
		}
	})

	t.Run("preserves infrastructure error from cf ref lookup instead of returning ErrVerificationNotFound", func(t *testing.T) {
		dbErr := errors.New("db timeout")
		repo := &fakeRepo{getByCFRefErr: dbErr}
		svc := newTestService(repo, &fakeCashfree{})

		_, err := svc.ProcessDigiLockerCallback(context.Background(), "", "cf_ref_1", domain.WebhookEventDigiLockerSuccess, "", "system:webhook")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if errors.Is(err, domain.ErrVerificationNotFound) {
			t.Fatalf("expected infrastructure error, got ErrVerificationNotFound: %v", err)
		}
		if !errors.Is(err, dbErr) {
			t.Fatalf("expected wrapped dbErr, got %v", err)
		}
	})

	t.Run("rejects conflicting vendor_reference_id and cf_reference_id", func(t *testing.T) {
		cfRefOther := "cf_ref_other"
		repo := &fakeRepo{
			verification: &domain.KYCVerification{
				ID:                uuid.New(),
				TenantID:          uuid.New(),
				VendorReferenceID: "cf_dgl_a",
				Status:            domain.KYCStatusPending,
				Method:            domain.KYCMethodDigiLocker,
			},
			altVerificationByCF: &domain.KYCVerification{
				ID:                uuid.New(),
				TenantID:          uuid.New(),
				VendorReferenceID: "cf_dgl_b",
				CFReferenceID:     &cfRefOther,
				Status:            domain.KYCStatusPending,
				Method:            domain.KYCMethodDigiLocker,
			},
		}
		svc := newTestService(repo, &fakeCashfree{})

		_, err := svc.ProcessDigiLockerCallback(context.Background(), "cf_dgl_a", cfRefOther, domain.WebhookEventDigiLockerSuccess, "", "system:webhook")
		if err == nil {
			t.Fatal("expected conflict error when vendor_reference_id and cf_reference_id resolve to different records")
		}
		if repo.submitManualReviewCalls != 0 {
			t.Fatalf("expected 0 manual review submissions, got %d", repo.submitManualReviewCalls)
		}
	})

	t.Run("duplicate success callback on pending_manual_review is a no-op", func(t *testing.T) {
		cfRef := "cf_ref_dup"
		repo := &fakeRepo{
			verification: &domain.KYCVerification{
				ID:                uuid.New(),
				TenantID:          uuid.New(),
				VendorReferenceID: "cf_dgl_dup",
				CFReferenceID:     &cfRef,
				Status:            domain.KYCStatusPendingManualReview,
				Method:            domain.KYCMethodDigiLocker,
			},
		}
		cf := &fakeCashfree{
			digiLockerErr: errors.New("should not call GetDigiLockerAadhaar on duplicate callback"),
		}
		svc := newTestService(repo, cf)

		v, err := svc.ProcessDigiLockerCallback(context.Background(), "cf_dgl_dup", cfRef, domain.WebhookEventDigiLockerSuccess, "", "system:webhook")
		if err != nil {
			t.Fatalf("expected no-op success, got error: %v", err)
		}
		if v.Status != domain.KYCStatusPendingManualReview {
			t.Fatalf("expected pending_manual_review, got %s", v.Status)
		}
		if repo.submitManualReviewCalls != 0 {
			t.Fatalf("expected 0 manual review submissions, got %d", repo.submitManualReviewCalls)
		}
	})
}
