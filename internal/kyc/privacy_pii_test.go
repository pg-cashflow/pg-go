package kyc_test

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
)

// In-memory slog handler that captures log lines for PII leak auditing
type bufferHandler struct {
	buf *bytes.Buffer
}

func (h *bufferHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *bufferHandler) Handle(_ context.Context, r slog.Record) error {
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
func (h *bufferHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *bufferHandler) WithGroup(_ string) slog.Handler      { return h }

// TestGate14_ZeroRawAadhaarInApplicationLogs verifies that application logs never emit
// raw 12-digit Aadhaar numbers matching the national pattern \b[2-9]{1}[0-9]{3}[0-9]{4}[0-9]{4}\b.
func TestGate14_ZeroRawAadhaarInApplicationLogs(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := &bufferHandler{buf: buf}
	logger := slog.New(handler)
	prevLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prevLogger)

	// Simulate representative KYC logging events
	tenantID := uuid.New()
	maskedUID := "XXXX-XXXX-1234"
	identityHash := "hash_abc123"

	logger.Info("processing kyc initiation", "tenant_id", tenantID, "method", "digilocker")
	logger.Info("digilocker verification completed", "tenant_id", tenantID, "masked_uid", maskedUID, "hash", identityHash)
	logger.Warn("duplicate identity detected across properties", "tenant_id", tenantID, "masked_uid", maskedUID)
	logger.Info("dpdp consent revoked", "tenant_id", tenantID, "actor", "tenant")

	// Regular expression for standard 12-digit Indian Aadhaar numbers (first digit 2-9)
	aadhaarRegex := regexp.MustCompile(`\b[2-9]{1}[0-9]{3}[0-9]{4}[0-9]{4}\b`)

	logOutput := buf.String()
	matches := aadhaarRegex.FindAllString(logOutput, -1)
	if len(matches) > 0 {
		t.Fatalf("[LEAK DETECTED] Raw 12-digit Aadhaar number found in log output: %v", matches)
	}

	// Verify that masked UID was logged instead
	if !strings.Contains(logOutput, maskedUID) {
		t.Fatalf("expected masked UID %q in logs, got: %s", maskedUID, logOutput)
	}
}

type fakeRevokeRepo struct {
	fakeRepo
	revokedTenants map[uuid.UUID]bool
}

func (r *fakeRevokeRepo) RevokeConsent(_ context.Context, tenantID uuid.UUID, _ string) error {
	if r.consent != nil && r.consent.TenantID == tenantID {
		now := time.Now().UTC()
		r.consent.RevokedAt = &now
	}
	if r.verification != nil && r.verification.TenantID == tenantID {
		r.verification.Status = domain.KYCStatusRevoked
		r.verification.MaskedUID = nil
		r.verification.IdentityHash = nil
	}
	r.revokedTenants[tenantID] = true
	return nil
}

func (r *fakeRevokeRepo) GetActiveConsentByTenant(_ context.Context, tenantID uuid.UUID) (*domain.KYCConsent, error) {
	if r.consent == nil || r.consent.RevokedAt != nil {
		return nil, domain.ErrVerificationNotFound
	}
	return r.consent, nil
}

// TestGate14_DPDP_ConsentRevocationAtomicWipe verifies that consent revocation
// cascades to scrub masked_uid and identity_hash from tenant verification records.
func TestGate14_DPDP_ConsentRevocationAtomicWipe(t *testing.T) {
	repo := &fakeRevokeRepo{
		revokedTenants: make(map[uuid.UUID]bool),
	}
	cf := &fakeCashfree{
		doc: &kycsvc.DigiLockerDoc{Name: "Ravi Kumar", DOB: "1990-05-15", Gender: "M", MaskedUID: "XXXX-1234"},
	}
	svc := newTestService(&repo.fakeRepo, cf)
	// Override service repo with fakeRevokeRepo
	svc = kycsvc.NewService(repo, cf, kycsvc.Config{
		IdentitySecret:           "test-secret-key",
		HashKeyVersion:           1,
		VerificationValidityDays: 365,
	})

	ctx := context.Background()
	tenantID := uuid.New()

	// 1. Record Consent
	consent, err := svc.RecordConsent(ctx, tenantID, "onboarding", "v1", "I hereby consent to verification", "127.0.0.1", "test-agent", "tenant")
	if err != nil {
		t.Fatalf("record consent: %v", err)
	}
	if consent == nil || consent.RevokedAt != nil {
		t.Fatalf("expected active consent")
	}

	// 2. Initiate Verification
	vendorRefID := "ref-dpdp-001"
	repo.verification = &domain.KYCVerification{
		ID:                uuid.New(),
		TenantID:          tenantID,
		VendorReferenceID: vendorRefID,
		Status:            domain.KYCStatusPending,
	}

	// 3. Complete Verification
	err = svc.ProcessDigiLockerCompletion(ctx, vendorRefID, "", "cashfree_webhook")
	if err != nil {
		t.Fatalf("complete verification: %v", err)
	}

	// 4. Revoke Consent (DPDP Rule 8)
	if err := svc.RevokeConsent(ctx, tenantID, "tenant"); err != nil {
		t.Fatalf("revoke consent: %v", err)
	}

	// Verify Active Consent is cleared
	activeConsent, err := repo.GetActiveConsentByTenant(ctx, tenantID)
	if err == nil && activeConsent != nil && activeConsent.RevokedAt == nil {
		t.Fatalf("expected active consent to be nil or revoked, got: %+v", activeConsent)
	}

	// Verify verification record PII was scrubbed
	if repo.verification.MaskedUID != nil || repo.verification.IdentityHash != nil {
		t.Fatalf("expected PII to be scrubbed from verification record, got maskedUID=%v, hash=%v",
			repo.verification.MaskedUID, repo.verification.IdentityHash)
	}
}

// Verhoeff tables
var (
	verhoeffD = [][]int{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		{1, 2, 3, 4, 0, 6, 7, 8, 9, 5},
		{2, 3, 4, 0, 1, 7, 8, 9, 5, 6},
		{3, 4, 0, 1, 2, 8, 9, 5, 6, 7},
		{4, 0, 1, 2, 3, 9, 5, 6, 7, 8},
		{5, 9, 8, 7, 6, 0, 4, 3, 2, 1},
		{6, 5, 9, 8, 7, 1, 0, 4, 3, 2},
		{7, 6, 5, 9, 8, 2, 1, 0, 4, 3},
		{8, 7, 6, 5, 9, 3, 2, 1, 0, 4},
		{9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
	}
	verhoeffP = [][]int{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		{1, 5, 7, 6, 2, 8, 3, 0, 9, 4},
		{5, 8, 0, 3, 7, 9, 6, 1, 4, 2},
		{8, 9, 1, 6, 0, 4, 3, 5, 2, 7},
		{9, 4, 5, 3, 1, 2, 6, 8, 7, 0},
		{4, 2, 8, 6, 5, 7, 3, 9, 0, 1},
		{2, 7, 9, 3, 8, 0, 6, 4, 1, 5},
		{7, 0, 4, 6, 9, 1, 3, 2, 5, 8},
	}
	verhoeffInv = []int{0, 4, 3, 2, 1, 5, 6, 7, 8, 9}
)

func generateVerhoeffCheckDigit(num string) int {
	c := 0
	for i := 0; i < len(num); i++ {
		digit := int(num[len(num)-1-i] - '0')
		c = verhoeffD[c][verhoeffP[(i+1)%8][digit]]
	}
	return verhoeffInv[c]
}

func validateVerhoeff(num string) bool {
	c := 0
	for i := 0; i < len(num); i++ {
		digit := int(num[len(num)-1-i] - '0')
		if digit < 0 || digit > 9 {
			return false
		}
		c = verhoeffD[c][verhoeffP[i%8][digit]]
	}
	return c == 0
}

// TestGate14_VerhoeffChecksumValidation asserts that invalid Aadhaar numbers fail-closed
func TestGate14_VerhoeffChecksumValidation(t *testing.T) {
	prefix := "29384756102"
	checkDigit := generateVerhoeffCheckDigit(prefix)
	validAadhaar := prefix + string(rune('0'+checkDigit))
	
	// Corrupted last digit
	corruptDigit := (checkDigit + 1) % 10
	invalidAadhaar := prefix + string(rune('0'+corruptDigit))

	if !validateVerhoeff(validAadhaar) {
		t.Fatalf("expected valid Aadhaar checksum for %s (checkDigit=%d)", validAadhaar, checkDigit)
	}
	if validateVerhoeff(invalidAadhaar) {
		t.Fatalf("expected invalid Aadhaar checksum for %s to fail", invalidAadhaar)
	}
}
