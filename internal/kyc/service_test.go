package kyc_test

import (
	"context"
	"encoding/json"
	"errors"
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
	recordFailureAndCountFn func(reasonCode string) (int, error)
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
func (f *fakeRepo) CompleteVerificationTx(_ context.Context, id uuid.UUID, maskedUID, _ string, _ bool, _ int16, qrStatus *string, nameMismatch bool, attestedPhotoBytes []byte, _, _ time.Time, _ *time.Time, _ string) (*domain.KYCVerification, error) {
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
func (f *fakeRepo) SubmitManualReviewTx(_ context.Context, id uuid.UUID, maskedUID, _ string, ocrData, docURL string, docBytes []byte, docContentType string, _ string) (*domain.KYCVerification, error) {
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
	return v, nil
}
func (f *fakeRepo) GetKYCDocument(_ context.Context, _ uuid.UUID) ([]byte, string, error) {
	if len(f.attestedPhoto) > 0 {
		return f.attestedPhoto, "image/jpeg", nil
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


type fakeCashfree struct {
	ocrResp       *cashfree.SmartOCRResponse
	ocrErr        error
	ocrCalled     bool
	verifyOTPErr  error
	verifyOTPResp *cashfree.AadhaarOTPVerifyResult
}

func (f *fakeCashfree) UploadAadhaarDocument(_ context.Context, _ io.Reader, _ string) (*cashfree.SmartOCRResponse, error) {
	f.ocrCalled = true
	return f.ocrResp, f.ocrErr
}
func (f *fakeCashfree) RequestAadhaarOTP(_ context.Context, _ string) (*cashfree.AadhaarOTPInitResult, error) {
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

// ---- Tests -------------------------------------------------------------------

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

	// 2. Idempotency: when active consent exists for this version, returns existing without error
	repo.consent = c
	c2, err := svc.RecordConsent(context.Background(), tenantID,
		"aadhaar_kyc", "v1.0", "", "127.0.0.1", "Mozilla/5.0", "tenant:abc")
	if err != nil {
		t.Fatalf("unexpected error on idempotent call: %v", err)
	}
	if c2.ID != c.ID {
		t.Errorf("expected idempotent consent to return same ID %s, got %s", c.ID, c2.ID)
	}
	if c2.ConsentTextHash != expectedHash {
		t.Errorf("expected stable hash %s, got %s", expectedHash, c2.ConsentTextHash)
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

	refID, err := svc.InitiateAadhaarOTP(context.Background(), tenantID, "123456789012", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("unexpected error initiating aadhaar otp: %v", err)
	}
	if refID != "123456" {
		t.Errorf("expected refID '123456', got %s", refID)
	}
	if repo.verification == nil || repo.verification.Method != domain.KYCMethodAadhaarOTP {
		t.Errorf("expected verification method aadhaar_otp, got %v", repo.verification)
	}
}

func TestCompleteAadhaarOTP_AutoVerified(t *testing.T) {
	tenantID := uuid.New()
	verifID := uuid.New()
	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:                verifID,
			TenantID:          tenantID,
			Status:            domain.KYCStatusPending,
			VendorReferenceID: "cf_aadhaar_otp_ref_100",
		},
	}
	cf := &fakeCashfree{}
	svc := newTestService(repo, cf)

	v, err := svc.CompleteAadhaarOTP(context.Background(), tenantID, "ref_100", "654321", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("unexpected error completing aadhaar otp: %v", err)
	}
	if v.Status != domain.KYCStatusVerified {
		t.Fatalf("expected status verified (auto), got %s", v.Status)
	}
}

func TestCompleteAadhaarOTP_FailureClassificationAndRetryBudget(t *testing.T) {
	tenantID := uuid.New()
	verifID := uuid.New()

	basePendingVerif := func() *domain.KYCVerification {
		return &domain.KYCVerification{
			ID:                verifID,
			TenantID:          tenantID,
			Status:            domain.KYCStatusPending,
			VendorReferenceID: "cf_aadhaar_otp_ref_fail",
		}
	}

	t.Run("Wrong OTP 1st attempt is recoverable with 2 attempts left and 50s cooldown", func(t *testing.T) {
		repo := &fakeRepo{
			verification: basePendingVerif(),
			recordFailureAndCountFn: func(code string) (int, error) {
				if code != "WRONG_OTP" {
					t.Errorf("unexpected code: %s", code)
				}
				return 1, nil // 1st failure
			},
		}
		cf := &fakeCashfree{
			verifyOTPErr: &cashfree.APIError{
				StatusCode: 400,
				Body:       `{"message": "OTP entered is invalid"}`,
			},
		}
		svc := newTestService(repo, cf)

		_, err := svc.CompleteAadhaarOTP(context.Background(), tenantID, "ref_fail", "000000", "tenant:"+tenantID.String())
		if err == nil {
			t.Fatal("expected failure")
		}
		var fail *kycsvc.AadhaarVerifyFailure
		if !errors.As(err, &fail) {
			t.Fatalf("expected AadhaarVerifyFailure, got %T: %v", err, err)
		}
		if fail.ReasonCode != "WRONG_OTP" {
			t.Errorf("expected WRONG_OTP, got %s", fail.ReasonCode)
		}
		if !fail.Retryable {
			t.Error("expected retryable true on attempt 1")
		}
		if fail.AttemptsRemaining != 2 {
			t.Errorf("expected 2 attempts remaining, got %d", fail.AttemptsRemaining)
		}
		if fail.CooldownSeconds != 50 {
			t.Errorf("expected 50s cooldown, got %d", fail.CooldownSeconds)
		}
	})

	t.Run("Wrong OTP 3rd attempt burns retry budget (0 remaining, retryable: false)", func(t *testing.T) {
		repo := &fakeRepo{
			verification: basePendingVerif(),
			recordFailureAndCountFn: func(_ string) (int, error) {
				return 3, nil // 3rd failure
			},
		}
		cf := &fakeCashfree{
			verifyOTPErr: &cashfree.APIError{
				StatusCode: 400,
				Body:       `{"message": "OTP entered is invalid"}`,
			},
		}
		svc := newTestService(repo, cf)

		_, err := svc.CompleteAadhaarOTP(context.Background(), tenantID, "ref_fail", "000000", "tenant:"+tenantID.String())
		var fail *kycsvc.AadhaarVerifyFailure
		if !errors.As(err, &fail) {
			t.Fatalf("expected AadhaarVerifyFailure, got %T", err)
		}
		if fail.AttemptsRemaining != 0 {
			t.Errorf("expected 0 attempts remaining, got %d", fail.AttemptsRemaining)
		}
		if fail.Retryable {
			t.Error("expected retryable false when attempts exhausted")
		}
	})

	t.Run("Invalid Aadhaar on repeat upgrades to FailureTerminal with 0 attempts remaining", func(t *testing.T) {
		repo := &fakeRepo{
			verification: basePendingVerif(),
			recordFailureAndCountFn: func(_ string) (int, error) {
				return 2, nil // 2nd failure of INVALID_AADHAAR
			},
		}
		cf := &fakeCashfree{
			verifyOTPErr: &cashfree.APIError{
				StatusCode: 200,
				Body:       `{"message": "Invalid Aadhaar Card"}`,
			},
		}
		svc := newTestService(repo, cf)

		_, err := svc.CompleteAadhaarOTP(context.Background(), tenantID, "ref_fail", "111111", "tenant:"+tenantID.String())
		var fail *kycsvc.AadhaarVerifyFailure
		if !errors.As(err, &fail) {
			t.Fatalf("expected AadhaarVerifyFailure, got %T", err)
		}
		if fail.ReasonCode != "INVALID_AADHAAR" {
			t.Errorf("expected INVALID_AADHAAR, got %s", fail.ReasonCode)
		}
		if fail.Class != cashfree.FailureTerminal {
			t.Errorf("expected upgraded FailureTerminal, got %s", fail.Class)
		}
		if fail.Retryable {
			t.Error("expected retryable false on repeated invalid aadhaar")
		}
		if fail.AttemptsRemaining != 0 {
			t.Errorf("expected 0 attempts remaining, got %d", fail.AttemptsRemaining)
		}
	})

	t.Run("Mobile not linked is immediately terminal without burning rate limit tokens", func(t *testing.T) {
		repo := &fakeRepo{
			verification: basePendingVerif(),
			recordFailureAndCountFn: func(_ string) (int, error) {
				return 1, nil
			},
		}
		cf := &fakeCashfree{
			verifyOTPErr: &cashfree.APIError{
				StatusCode: 200,
				Body:       `{"message": "Aadhaar number does not have mobile number"}`,
			},
		}
		svc := newTestService(repo, cf)

		_, err := svc.CompleteAadhaarOTP(context.Background(), tenantID, "ref_fail", "123456", "tenant:"+tenantID.String())
		var fail *kycsvc.AadhaarVerifyFailure
		if !errors.As(err, &fail) {
			t.Fatalf("expected AadhaarVerifyFailure, got %T", err)
		}
		if fail.ReasonCode != "MOBILE_NOT_LINKED" {
			t.Errorf("expected MOBILE_NOT_LINKED, got %s", fail.ReasonCode)
		}
		if fail.Class != cashfree.FailureTerminal {
			t.Errorf("expected FailureTerminal, got %s", fail.Class)
		}
		if fail.Retryable {
			t.Error("expected retryable false for mobile not linked")
		}
		if fail.AttemptsRemaining != 0 {
			t.Errorf("expected 0 attempts remaining, got %d", fail.AttemptsRemaining)
		}
	})

	t.Run("Cooldown active throttle does not burn retry attempt and is retryable", func(t *testing.T) {
		failureLogged := false
		repo := &fakeRepo{
			verification: basePendingVerif(),
			recordFailureAndCountFn: func(_ string) (int, error) {
				failureLogged = true
				return 1, nil
			},
		}
		cf := &fakeCashfree{
			verifyOTPErr: &cashfree.APIError{
				StatusCode: 409,
				Body:       `{"message": "Otp generated for this aadhaar, please try after some time"}`,
			},
		}
		svc := newTestService(repo, cf)

		_, err := svc.CompleteAadhaarOTP(context.Background(), tenantID, "ref_fail", "123456", "tenant:"+tenantID.String())
		var fail *kycsvc.AadhaarVerifyFailure
		if !errors.As(err, &fail) {
			t.Fatalf("expected AadhaarVerifyFailure, got %T", err)
		}
		if failureLogged {
			t.Error("COOLDOWN_ACTIVE should not be logged to failure table or burn retry budget")
		}
		if fail.ReasonCode != "COOLDOWN_ACTIVE" {
			t.Errorf("expected COOLDOWN_ACTIVE, got %s", fail.ReasonCode)
		}
		if !fail.Retryable {
			t.Error("expected retryable true for cooldown active")
		}
		if fail.CooldownSeconds != 45 {
			t.Errorf("expected 45s cooldown, got %d", fail.CooldownSeconds)
		}
		if fail.AttemptsRemaining != 3 {
			t.Errorf("expected full retry budget (3) remaining, got %d", fail.AttemptsRemaining)
		}
	})
}

func TestRejectManualReview_Success(t *testing.T) {
	tenantID := uuid.New()
	verifID := uuid.New()
	repo := &fakeRepo{
		verification: &domain.KYCVerification{
			ID:       verifID,
			TenantID: tenantID,
			Status:   domain.KYCStatusPendingManualReview,
		},
	}
	svc := newTestService(repo, &fakeCashfree{})

	reviewerID := uuid.New()
	rejected, err := svc.RejectManualReview(context.Background(), verifID, uuid.New(), reviewerID, "Blurry document", "owner:admin")
	if err != nil {
		t.Fatalf("unexpected error rejecting: %v", err)
	}
	if rejected.Status != domain.KYCStatusRejected {
		t.Fatalf("expected status rejected, got %s", rejected.Status)
	}
	if rejected.RejectionReason == nil || *rejected.RejectionReason != "Blurry document" {
		t.Errorf("expected rejection reason 'Blurry document', got %v", rejected.RejectionReason)
	}
}

func TestCompleteAadhaarOTP_InFlightLockContention(t *testing.T) {
	tenantID := uuid.New()
	verifID := uuid.New()
	lockAcquired := false
	repo := &fakeRepo{
		lockAcquired: &lockAcquired,
		verification: &domain.KYCVerification{
			ID:                verifID,
			TenantID:          tenantID,
			Status:            domain.KYCStatusPending,
			VendorReferenceID: "cf_aadhaar_otp_ref_100",
		},
	}
	svc := newTestService(repo, &fakeCashfree{})

	_, err := svc.CompleteAadhaarOTP(context.Background(), tenantID, "ref_100", "123456", "tenant:"+tenantID.String())
	if !errors.Is(err, domain.ErrVerificationInProgress) {
		t.Fatalf("expected ErrVerificationInProgress, got %v", err)
	}
}


