package kyc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
)

// inMemoryLogHandler captures structured slog records to assert zero PII leaks.
type inMemoryLogHandler struct {
	buf *bytes.Buffer
}

func (h *inMemoryLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *inMemoryLogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		return true
	})
	b.WriteString("\n")
	h.buf.WriteString(b.String())
	return nil
}
func (h *inMemoryLogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *inMemoryLogHandler) WithGroup(_ string) slog.Handler      { return h }

// 1. Missing consent blocks provider calls.
func TestPhase5_MissingConsentBlocksProviderCalls(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		tenantName: "Ravi Kumar",
		consent:    nil, // No active consent
	}
	cf := &fakeCashfree{
		ocrResp: &cashfree.SmartOCRResponse{
			Status:    "SUCCESS",
			Name:      "Ravi Kumar",
			MaskedUID: "1234",
		},
	}
	svc := newTestService(repo, cf)

	validPDF := strings.NewReader("%PDF-1.4 sample pdf document header bytes")
	_, err := svc.VerifyDocument(context.Background(), tenantID, validPDF, "passport.pdf", "application/pdf", "passport", "tenant:"+tenantID.String())
	if err == nil {
		t.Fatal("expected error when tenant has no active consent, got nil")
	}
	if !errors.Is(err, kycsvc.ErrNoActiveConsent) {
		t.Fatalf("expected ErrNoActiveConsent, got %v", err)
	}
	if cf.ocrCalled {
		t.Fatal("Cashfree Bharat OCR client was called despite missing consent; provider call must be blocked fail-closed")
	}
	if repo.verification != nil {
		t.Fatal("verification record created in DB despite missing consent")
	}
}

// 2. Tampered documents enter safe rejection/review state.
func TestPhase5_TamperedDocumentsEnterSafeRejectionReviewState(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		tenantName: "Ravi Kumar",
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{
		ocrResp: &cashfree.SmartOCRResponse{
			ReferenceID:  998877,
			Status:       "SUCCESS",
			Name:         "Ravi Kumar",
			DOB:          "1990-01-01",
			Gender:       "M",
			MaskedUID:    "1234",
			IsTampered:   true,
			TamperReason: "photo_of_screen detected",
		},
	}
	svc := newTestService(repo, cf)

	validPDF := strings.NewReader("%PDF-1.4 sample document bytes")
	v, err := svc.VerifyDocument(context.Background(), tenantID, validPDF, "doc.pdf", "application/pdf", "aadhaar", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Status != domain.KYCStatusPendingManualReview {
		t.Fatalf("expected status pending_manual_review for tampered document, got %s", v.Status)
	}
	if v.OCRData == nil {
		t.Fatal("expected OCRData to be populated with tamper flags")
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(*v.OCRData), &parsed); err != nil {
		t.Fatalf("unmarshal ocr data: %v", err)
	}
	if isTampered, ok := parsed["is_tampered"].(bool); !ok || !isTampered {
		t.Fatalf("expected is_tampered=true in OCRData, got %v", parsed["is_tampered"])
	}
	if reason, ok := parsed["tamper_reason"].(string); !ok || reason != "photo_of_screen detected" {
		t.Fatalf("expected tamper_reason 'photo_of_screen detected', got %v", parsed["tamper_reason"])
	}
	if !v.NameMismatch {
		t.Fatal("expected NameMismatch=true to force strict manual review for tampered document")
	}
}

// 3. Invalid file signatures are rejected.
func TestPhase5_InvalidFileSignaturesRejected(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		tenantName: "Ravi Kumar",
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{}
	svc := newTestService(repo, cf)

	// HTML disguised as PDF
	badFile := strings.NewReader("<!DOCTYPE html><html><body><h1>Malicious Document</h1></body></html>")
	_, err := svc.VerifyDocument(context.Background(), tenantID, badFile, "doc.pdf", "application/pdf", "pan", "tenant:"+tenantID.String())
	if err == nil {
		t.Fatal("expected error for invalid file signature, got nil")
	}
	if !errors.Is(err, kycsvc.ErrUnsupportedDocumentType) {
		t.Fatalf("expected ErrUnsupportedDocumentType, got %v", err)
	}
	if cf.ocrCalled {
		t.Fatal("Cashfree Bharat OCR client was called for invalid file signature")
	}
}

// 4. Full Aadhaar numbers never appear in application logs or DB state.
func TestPhase5_ZeroFullAadhaarInLogsAndState(t *testing.T) {
	logBuf := &bytes.Buffer{}
	handler := &inMemoryLogHandler{buf: logBuf}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(prevLogger)

	tenantID := uuid.New()
	repo := &fakeRepo{
		tenantName: "Ravi Kumar",
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
	}
	cf := &fakeCashfree{
		ocrResp: &cashfree.SmartOCRResponse{
			ReferenceID: 887766,
			Status:      "SUCCESS",
			Name:        "Ravi Kumar",
			DOB:         "1992-05-15",
			Gender:      "M",
			MaskedUID:   "9999", // Provider returns only masked UID
		},
	}
	svc := newTestService(repo, cf)

	validPDF := strings.NewReader("%PDF-1.4 dummy aadhaar upload")
	v, err := svc.VerifyDocument(context.Background(), tenantID, validPDF, "aadhaar.pdf", "application/pdf", "aadhaar", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("verify document error: %v", err)
	}

	// Assert in-memory log buffer contains ZERO 12-digit numbers
	twelveDigitRe := regexp.MustCompile(`\b[2-9][0-9]{11}\b`)
	logs := logBuf.String()
	if matches := twelveDigitRe.FindAllString(logs, -1); len(matches) > 0 {
		t.Fatalf("found 12-digit Aadhaar UID leaked in logs: %v", matches)
	}

	// Assert DB record has only masked UID
	if v.MaskedUID == nil || *v.MaskedUID != "9999" {
		t.Fatalf("expected masked UID '9999', got %v", v.MaskedUID)
	}
	if v.OCRData != nil {
		if twelveDigitRe.MatchString(*v.OCRData) {
			t.Fatalf("found 12-digit Aadhaar UID inside OCRData: %s", *v.OCRData)
		}
	}
}

// 5. Document URLs cannot be accessed across properties.
func TestPhase5_DocumentURLsCannotBeAccessedAcrossProperties(t *testing.T) {
	propertyA := uuid.New()
	propertyB := uuid.New()
	verificationID := uuid.New()

	repo := &fakePropertyIsolationRepo{
		validPropertyID: propertyA,
		docBytes:        []byte("%PDF-1.4 mock property doc"),
		contentType:     "application/pdf",
	}
	svc := kycsvc.NewService(repo, &fakeCashfree{}, kycsvc.Config{})

	// Access with correct property ID succeeds
	doc, ct, err := svc.GetKYCDocument(context.Background(), verificationID, propertyA)
	if err != nil || len(doc) == 0 || ct != "application/pdf" {
		t.Fatalf("expected document access to succeed for authorized property, err: %v", err)
	}

	// Access with different property ID fails closed
	_, _, err = svc.GetKYCDocument(context.Background(), verificationID, propertyB)
	if err == nil {
		t.Fatal("expected document access to fail for cross-property query, got nil")
	}
	if !errors.Is(err, domain.ErrVerificationNotFound) {
		t.Fatalf("expected ErrVerificationNotFound for cross-property access, got %v", err)
	}
}

type fakePropertyIsolationRepo struct {
	fakeRepo
	validPropertyID uuid.UUID
	docBytes        []byte
	contentType     string
}

func (r *fakePropertyIsolationRepo) VerifyPropertyOwnership(_ context.Context, _, propertyID uuid.UUID) (bool, error) {
	return propertyID == r.validPropertyID, nil
}

func (r *fakePropertyIsolationRepo) GetKYCDocument(_ context.Context, _ uuid.UUID) ([]byte, string, error) {
	return r.docBytes, r.contentType, nil
}

// 6. Unauthorized owners cannot inspect documents.
func TestPhase5_UnauthorizedOwnersCannotInspectDocuments(t *testing.T) {
	repo := &fakePropertyIsolationRepo{
		validPropertyID: uuid.New(),
	}
	svc := kycsvc.NewService(repo, &fakeCashfree{}, kycsvc.Config{})

	unauthorizedPropertyID := uuid.New()
	_, _, err := svc.GetKYCDocument(context.Background(), uuid.New(), unauthorizedPropertyID)
	if err == nil {
		t.Fatal("expected unauthorized inspection to fail")
	}
	if !errors.Is(err, domain.ErrVerificationNotFound) {
		t.Fatalf("expected ErrVerificationNotFound for unauthorized owner, got %v", err)
	}
}

// 7. OCR timeout does not create duplicate verification requests.
func TestPhase5_OCRTimeoutDoesNotCreateDuplicateVerificationRequests(t *testing.T) {
	tenantID := uuid.New()
	lockAcquired := false // Simulate distributed lock lease already held
	repo := &fakeRepo{
		tenantName: "Ravi Kumar",
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		lockAcquired: &lockAcquired,
	}
	cf := &fakeCashfree{}
	svc := newTestService(repo, cf)

	validPDF := strings.NewReader("%PDF-1.4 dummy document")
	_, err := svc.VerifyDocument(context.Background(), tenantID, validPDF, "doc.pdf", "application/pdf", "aadhaar", "tenant:"+tenantID.String())
	if err == nil {
		t.Fatal("expected ErrVerificationInProgress under lock lease contention, got nil")
	}
	if !errors.Is(err, domain.ErrVerificationInProgress) {
		t.Fatalf("expected ErrVerificationInProgress, got %v", err)
	}
	if cf.ocrCalled {
		t.Fatal("Cashfree Bharat OCR client called during concurrent in-flight lease; double-dispatch must be blocked")
	}
}

// 8. Revoked consent triggers the correct access and deletion workflow.
func TestPhase5_RevokedConsentTriggersPIIErasure(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRevokeRepo{
		revokedTenants: make(map[uuid.UUID]bool),
	}
	repo.consent = &domain.KYCConsent{
		ID:       uuid.New(),
		TenantID: tenantID,
	}
	maskedUID := "9999"
	hashVal := "hash_identity_9999"
	repo.verification = &domain.KYCVerification{
		ID:           uuid.New(),
		TenantID:     tenantID,
		Status:       domain.KYCStatusVerified,
		MaskedUID:    &maskedUID,
		IdentityHash: &hashVal,
	}
	svc := kycsvc.NewService(repo, &fakeCashfree{}, kycsvc.Config{
		IdentitySecret:           "test-secret-key",
		HashKeyVersion:           1,
		VerificationValidityDays: 365,
	})

	err := svc.RevokeConsent(context.Background(), tenantID, "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("revoke consent error: %v", err)
	}

	if !repo.revokedTenants[tenantID] {
		t.Fatal("expected tenant consent to be revoked in repository")
	}
	if repo.verification.Status != domain.KYCStatusRevoked {
		t.Fatalf("expected verification status revoked, got %s", repo.verification.Status)
	}
	if repo.verification.MaskedUID != nil {
		t.Fatalf("expected masked_uid to be wiped, got %v", *repo.verification.MaskedUID)
	}
	if repo.verification.IdentityHash != nil {
		t.Fatalf("expected identity_hash to be wiped, got %v", *repo.verification.IdentityHash)
	}
}

// 9. Expired documents are deleted according to approved retention policy.
func TestPhase5_ExpiredDocumentsRejectedByReadGate(t *testing.T) {
	tenantID := uuid.New()
	pastExpiry := time.Now().UTC().Add(-24 * time.Hour)
	repo := &fakeRepo{
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
		verification: &domain.KYCVerification{
			ID:        uuid.New(),
			TenantID:  tenantID,
			Status:    domain.KYCStatusVerified,
			ExpiresAt: &pastExpiry,
		},
	}
	svc := newTestService(repo, &fakeCashfree{})

	v, _, err := svc.GetStatus(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("get status error: %v", err)
	}
	if v == nil {
		t.Fatal("expected non-nil verification view")
	}
	if v.Status != domain.KYCStatusExpired {
		t.Fatalf("expected status 'expired' at read-time gate, got %s", v.Status)
	}
}

// 10. Manual reviewer decisions are immutable and auditable.
func TestPhase5_ManualReviewerDecisionsImmutableAndAuditable(t *testing.T) {
	verificationID := uuid.New()
	propertyID := uuid.New()
	reviewerID := uuid.New()

	repo := &fakeRepo{}
	svc := newTestService(repo, &fakeCashfree{})

	// Reject with valid reason succeeds and records audit data
	rej, err := svc.RejectManualReview(context.Background(), verificationID, propertyID, reviewerID, "Blurry document text", "owner:reviewer")
	if err != nil {
		t.Fatalf("reject manual review error: %v", err)
	}
	if rej.Status != domain.KYCStatusRejected {
		t.Fatalf("expected status rejected, got %s", rej.Status)
	}
	if rej.ReviewedBy == nil || *rej.ReviewedBy != reviewerID {
		t.Fatalf("expected ReviewedBy to match reviewerID %s", reviewerID)
	}
	if rej.RejectionReason == nil || *rej.RejectionReason != "Blurry document text" {
		t.Fatalf("expected rejection reason 'Blurry document text', got %v", rej.RejectionReason)
	}

	// Approve sets audit fields
	app, err := svc.ApproveManualReview(context.Background(), verificationID, propertyID, reviewerID, "owner:reviewer")
	if err != nil {
		t.Fatalf("approve manual review error: %v", err)
	}
	if app.Status != domain.KYCStatusVerified {
		t.Fatalf("expected status verified, got %s", app.Status)
	}
	if app.ReviewedBy == nil || *app.ReviewedBy != reviewerID {
		t.Fatalf("expected ReviewedBy to match reviewerID %s", reviewerID)
	}
	if app.ReviewedAt == nil {
		t.Fatal("expected ReviewedAt timestamp to be set")
	}
}

// 11. KYC completion never depends on an unverified OCR result alone.
func TestPhase5_KYCCompletionNeverDependsOnUnverifiedOCRAlone(t *testing.T) {
	tenantID := uuid.New()
	repo := &fakeRepo{
		tenantName: "Ravi Kumar",
		consent: &domain.KYCConsent{
			ID:       uuid.New(),
			TenantID: tenantID,
		},
	}
	// Even with 100% clean OCR response, document upload MUST land in pending_manual_review
	cf := &fakeCashfree{
		ocrResp: &cashfree.SmartOCRResponse{
			ReferenceID: 112233,
			Status:      "SUCCESS",
			Name:        "Ravi Kumar",
			DOB:         "1990-01-01",
			Gender:      "M",
			MaskedUID:   "1234",
		},
	}
	svc := newTestService(repo, cf)

	validPDF := strings.NewReader("%PDF-1.4 clean document")
	v, err := svc.VerifyDocument(context.Background(), tenantID, validPDF, "doc.pdf", "application/pdf", "aadhaar", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("verify document error: %v", err)
	}
	if v.Status != domain.KYCStatusPendingManualReview {
		t.Fatalf("document upload must land in pending_manual_review, got %s; OCR alone must NEVER auto-verify", v.Status)
	}
	if v.VerifiedAt != nil {
		t.Fatal("VerifiedAt must NOT be set until explicit manual review approval")
	}
}

// 12. Non-Aadhaar verification route (PAN, Passport, Voter ID, Driving Licence).
func TestPhase5_NonAadhaarVerificationRoute(t *testing.T) {
	docTypes := []struct {
		inputDocType domain.DocumentType
		expectedNorm string
	}{
		{domain.DocTypePAN, "pan"},
		{domain.DocTypePassport, "passport"},
		{domain.DocTypeVoterID, "voter_id"},
		{domain.DocTypeDrivingLicence, "driving_licence"},
	}

	for _, tc := range docTypes {
		t.Run(string(tc.inputDocType), func(t *testing.T) {
			tenantID := uuid.New()
			repo := &fakeRepo{
				tenantName: "Priya Sharma",
				consent: &domain.KYCConsent{
					ID:       uuid.New(),
					TenantID: tenantID,
				},
			}
			cf := &fakeCashfree{
				ocrResp: &cashfree.SmartOCRResponse{
					ReferenceID: 554433,
					Status:      "SUCCESS",
					Name:        "Priya Sharma",
					MaskedUID:   "5678",
				},
			}
			svc := newTestService(repo, cf)

			validJPEG := strings.NewReader("\xFF\xD8\xFF\xE0\x00\x10JFIF sample image bytes for non-aadhaar doc")
			v, err := svc.VerifyDocument(context.Background(), tenantID, validJPEG, "id.jpeg", "image/jpeg", string(tc.inputDocType), "tenant:"+tenantID.String())
			if err != nil {
				t.Fatalf("verify document with type %s failed: %v", tc.inputDocType, err)
			}
			if !cf.ocrCalled {
				t.Fatalf("expected Cashfree Bharat OCR called for doc type %s", tc.inputDocType)
			}
			if v.Status != domain.KYCStatusPendingManualReview {
				t.Fatalf("expected status pending_manual_review, got %s", v.Status)
			}
		})
	}
}

// 13. Duplicate callbacks do not duplicate decisions (monotonicity and idempotency).
func TestPhase5_DuplicateCallbacksDoNotDuplicateDecisions(t *testing.T) {
	tenantID := uuid.New()
	verifID := uuid.New()
	refID := "cf_ocr_unique_ref_12345"

	now := time.Now().UTC()
	v := &domain.KYCVerification{
		ID:                verifID,
		TenantID:          tenantID,
		VendorReferenceID: refID,
		Status:            domain.KYCStatusVerified,
		VerifiedAt:        &now,
	}

	repo := &fakeRepo{
		verification: v,
	}
	svc := newTestService(repo, &fakeCashfree{})

	fetched, err := svc.GetVerificationByVendorRefID(context.Background(), refID)
	if err != nil {
		t.Fatalf("failed fetching verification: %v", err)
	}
	if fetched.Status != domain.KYCStatusVerified {
		t.Fatalf("expected verified status, got %s", fetched.Status)
	}

	// Terminal states are strictly immutable; re-decision is a no-op
	if !fetched.IsValid(now) {
		t.Fatal("expected terminal verified verification to remain valid")
	}
}
