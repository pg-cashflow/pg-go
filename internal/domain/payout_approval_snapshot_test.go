package domain

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/google/uuid"
)

func generateTestKey(t *testing.T, size int) []byte {
	t.Helper()
	key := make([]byte, size)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("failed generating test key: %v", err)
	}
	return key
}

func sampleSnapshot() *PayoutApprovalSnapshot {
	batchID := uuid.New()
	propID := uuid.New()
	ownerID := uuid.New()
	payee1 := uuid.New()
	payee2 := uuid.New()
	item1 := uuid.New()
	item2 := uuid.New()

	return &PayoutApprovalSnapshot{
		SchemaVersion: CurrentSnapshotSchemaVersion,
		BatchID:       batchID,
		PropertyID:    propID,
		ApprovedBy:    ownerID,
		ApprovedAt:    time.Unix(1730000000, 0).UTC(),
		TotalAmount:   750000,
		ItemCount:     2,
		Items: []ApprovedItemSnapshot{
			{
				ItemID:            item1,
				PayeeID:           payee1,
				ReferenceNumber:   "REF-101",
				AmountPaise:       500000,
				Purpose:           "Vendor Invoice #42",
				PeriodLabel:       "2026-10",
				AccountNumberHash: "hash_bank_acct_abc123",
				IFSC:              "HDFC0001234",
			},
			{
				ItemID:            item2,
				PayeeID:           payee2,
				ReferenceNumber:   "REF-102",
				AmountPaise:       250000,
				Purpose:           "Maintenance Staff",
				PeriodLabel:       "2026-10",
				AccountNumberHash: "hash_upi_vpa_xyz789",
				UPIVPA:            "staff@okaxis",
			},
		},
	}
}

func TestPayoutApprovalSnapshot_SigningAndVerification(t *testing.T) {
	key := generateTestKey(t, 32)
	snap := sampleSnapshot()

	sig, err := SignApprovalSnapshot(key, snap)
	if err != nil {
		t.Fatalf("expected successful signature, got error: %v", err)
	}
	if sig == "" {
		t.Fatal("expected non-empty signature")
	}

	// Verify valid signature
	if err := VerifyApprovalSnapshot(key, snap, sig); err != nil {
		t.Fatalf("expected valid verification, got error: %v", err)
	}
}

func TestPayoutApprovalSnapshot_KeyLengthRequirement(t *testing.T) {
	snap := sampleSnapshot()

	// Short key (<32 bytes) must fail closed
	shortKey := generateTestKey(t, 31)
	if _, err := SignApprovalSnapshot(shortKey, snap); err != ErrInvalidSigningKey {
		t.Fatalf("expected ErrInvalidSigningKey on sign, got %v", err)
	}

	validKey := generateTestKey(t, 32)
	sig, err := SignApprovalSnapshot(validKey, snap)
	if err != nil {
		t.Fatalf("sign failed: %v", err)
	}

	if err := VerifyApprovalSnapshot(shortKey, snap, sig); err != ErrInvalidSigningKey {
		t.Fatalf("expected ErrInvalidSigningKey on verify, got %v", err)
	}
}

func TestPayoutApprovalSnapshot_MissingSignature(t *testing.T) {
	key := generateTestKey(t, 32)
	snap := sampleSnapshot()

	if err := VerifyApprovalSnapshot(key, snap, ""); err != ErrSignatureMismatch {
		t.Fatalf("expected ErrSignatureMismatch for empty signature, got %v", err)
	}
	if err := VerifyApprovalSnapshot(key, snap, "   "); err != ErrSignatureMismatch {
		t.Fatalf("expected ErrSignatureMismatch for whitespace signature, got %v", err)
	}
}

func TestPayoutApprovalSnapshot_TamperDetection(t *testing.T) {
	key := generateTestKey(t, 32)
	base := sampleSnapshot()
	validSig, err := SignApprovalSnapshot(key, base)
	if err != nil {
		t.Fatalf("sign failed: %v", err)
	}

	tests := []struct {
		name      string
		modifyFn  func(s *PayoutApprovalSnapshot)
		wrongKey  bool
	}{
		{
			name: "tampered amount",
			modifyFn: func(s *PayoutApprovalSnapshot) {
				s.Items[0].AmountPaise += 100 // +1 rupee
			},
		},
		{
			name: "tampered payee",
			modifyFn: func(s *PayoutApprovalSnapshot) {
				s.Items[0].PayeeID = uuid.New()
			},
		},
		{
			name: "tampered account fingerprint",
			modifyFn: func(s *PayoutApprovalSnapshot) {
				s.Items[0].AccountNumberHash = "hash_attacker_account"
			},
		},
		{
			name: "tampered IFSC",
			modifyFn: func(s *PayoutApprovalSnapshot) {
				s.Items[0].IFSC = "ICIC0009999"
			},
		},
		{
			name: "tampered UPI VPA",
			modifyFn: func(s *PayoutApprovalSnapshot) {
				s.Items[1].UPIVPA = "attacker@upi"
			},
		},
		{
			name: "tampered reference number",
			modifyFn: func(s *PayoutApprovalSnapshot) {
				s.Items[0].ReferenceNumber = "REF-999-TAMPERED"
			},
		},
		{
			name: "wrong signing key",
			wrongKey: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutated := sampleSnapshot()
			verifyKey := key
			if tc.wrongKey {
				verifyKey = generateTestKey(t, 32)
			} else {
				tc.modifyFn(mutated)
			}

			err := VerifyApprovalSnapshot(verifyKey, mutated, validSig)
			if err != ErrSignatureMismatch {
				t.Fatalf("expected ErrSignatureMismatch on %s, got %v", tc.name, err)
			}
		})
	}
}

func TestPayoutApprovalSnapshot_ItemOrderIndependence(t *testing.T) {
	key := generateTestKey(t, 32)
	snap1 := sampleSnapshot()

	// Clone snap1 into snap2 and reverse the items slice
	snap2 := &PayoutApprovalSnapshot{
		SchemaVersion: snap1.SchemaVersion,
		BatchID:       snap1.BatchID,
		PropertyID:    snap1.PropertyID,
		ApprovedBy:    snap1.ApprovedBy,
		ApprovedAt:    snap1.ApprovedAt,
		TotalAmount:   snap1.TotalAmount,
		ItemCount:     snap1.ItemCount,
		Items:         []ApprovedItemSnapshot{snap1.Items[1], snap1.Items[0]},
	}

	sig1, err := SignApprovalSnapshot(key, snap1)
	if err != nil {
		t.Fatalf("sign snap1: %v", err)
	}
	sig2, err := SignApprovalSnapshot(key, snap2)
	if err != nil {
		t.Fatalf("sign snap2: %v", err)
	}

	if sig1 != sig2 {
		t.Fatalf("canonical representation should produce identical signature regardless of item slice ordering: %s vs %s", sig1, sig2)
	}

	if err := VerifyApprovalSnapshot(key, snap2, sig1); err != nil {
		t.Fatalf("expected verify success across permuted item order, got: %v", err)
	}
}
