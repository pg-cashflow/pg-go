package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"  Rahul   Sharma  ", "rahul sharma"},
		{"PRIYA VERMA", "priya verma"},
		{"Amit\t\nKumar", "amit kumar"},
		{"José   Silva", "josé silva"},
	}

	for _, tt := range tests {
		got := NormalizeName(tt.input)
		if got != tt.expected {
			t.Errorf("NormalizeName(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestNormalizeDOB(t *testing.T) {
	tests := []struct {
		input         string
		wantDOB       string
		wantDedupable bool
		wantErr       bool
	}{
		{"1995-08-15", "1995-08-15", true, false},
		{"15-08-1995", "1995-08-15", true, false},
		{"15/08/1995", "1995-08-15", true, false},
		{"1995/08/15", "1995-08-15", true, false},
		{"1995", "1995-01-01", false, false},
		{"invalid-dob", "", false, true},
		{"", "", false, true},
	}

	for _, tt := range tests {
		gotDOB, gotDedup, err := NormalizeDOB(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("NormalizeDOB(%q) error = %v; wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if gotDOB != tt.wantDOB || gotDedup != tt.wantDedupable {
				t.Errorf("NormalizeDOB(%q) = (%q, %v); want (%q, %v)", tt.input, gotDOB, gotDedup, tt.wantDOB, tt.wantDedupable)
			}
		}
	}
}

func TestComputeIdentityHash(t *testing.T) {
	secret := "super-secret-key-12345"
	h1 := ComputeIdentityHash(secret, "Rahul Sharma", "1995-08-15", "M", "1234")
	h2 := ComputeIdentityHash(secret, "  rahul   SHARMA  ", "1995-08-15", "m", "1234")
	h3 := ComputeIdentityHash("different-secret", "Rahul Sharma", "1995-08-15", "M", "1234")

	if h1 == "" {
		t.Fatal("expected non-empty identity hash")
	}
	if h1 != h2 {
		t.Errorf("expected identity hashes to match for normalized inputs; got %s != %s", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("expected different secret to produce different hash; got %s == %s", h1, h3)
	}
}

func TestComputeAuditHash(t *testing.T) {
	tenantID := uuid.New()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	h1 := ComputeAuditHash("", tenantID, "system", "verification_initiated", "method=digilocker", now)
	if h1 == "" {
		t.Fatal("expected non-empty audit hash")
	}

	h2 := ComputeAuditHash(h1, tenantID, "system", "verification_completed", "masked_uid=1234", now.Add(time.Minute))
	if h2 == "" || h2 == h1 {
		t.Fatalf("expected valid next audit chain hash; got %s", h2)
	}
}

func TestKYCVerification_IsValid(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	past := now.Add(-24 * time.Hour)

	vValid := KYCVerification{
		Status:    KYCStatusVerified,
		ExpiresAt: &future,
	}
	if !vValid.IsValid(now) {
		t.Errorf("expected verification to be valid")
	}

	vExpired := KYCVerification{
		Status:    KYCStatusVerified,
		ExpiresAt: &past,
	}
	if vExpired.IsValid(now) {
		t.Errorf("expected verification to be invalid (expired)")
	}

	vPending := KYCVerification{
		Status: KYCStatusPending,
	}
	if vPending.IsValid(now) {
		t.Errorf("expected pending verification to be invalid")
	}
}
