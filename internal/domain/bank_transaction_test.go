package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestComputeBankTxnDedupHash_Deterministic(t *testing.T) {
	propID := uuid.New()
	date := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	bal := int64(15000000)

	h1 := domain.ComputeBankTxnDedupHash(propID, date, 1500000, "credit", "UPI12345", &bal, 1)
	h2 := domain.ComputeBankTxnDedupHash(propID, date, 1500000, "credit", "UPI12345", &bal, 1)

	if h1 == "" || h1 != h2 {
		t.Fatalf("expected identical deterministic hashes, got %q and %q", h1, h2)
	}
}

func TestComputeBankTxnDedupHash_ClosingBalanceDifferentiates(t *testing.T) {
	propID := uuid.New()
	date := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	bal1 := int64(15000000)
	bal2 := int64(16500000)

	h1 := domain.ComputeBankTxnDedupHash(propID, date, 1500000, "credit", "UPI12345", &bal1, 1)
	h2 := domain.ComputeBankTxnDedupHash(propID, date, 1500000, "credit", "UPI12345", &bal2, 1)

	if h1 == h2 {
		t.Fatalf("expected different hashes when closing balance differs, but both were %q", h1)
	}
}

func TestComputeBankTxnDedupHash_OccurrenceDifferentiatesWhenBalanceMissing(t *testing.T) {
	propID := uuid.New()
	date := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	// No balance column available
	h1 := domain.ComputeBankTxnDedupHash(propID, date, 1500000, "credit", "UPI12345", nil, 1)
	h2 := domain.ComputeBankTxnDedupHash(propID, date, 1500000, "credit", "UPI12345", nil, 2)

	if h1 == h2 {
		t.Fatalf("expected different hashes for occurrence 1 vs 2, but both were %q", h1)
	}
}

func TestComputeBankTxnDedupHash_ReuploadIdempotence(t *testing.T) {
	propID := uuid.New()
	date := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	// Re-uploading the file without balance assigns occurrence 1 to row 1 and occurrence 2 to row 2 again
	h1FirstUpload := domain.ComputeBankTxnDedupHash(propID, date, 1500000, "credit", "UPI12345", nil, 1)
	h1SecondUpload := domain.ComputeBankTxnDedupHash(propID, date, 1500000, "credit", "UPI12345", nil, 1)

	if h1FirstUpload != h1SecondUpload {
		t.Fatalf("expected idempotent re-upload hash match, got %q vs %q", h1FirstUpload, h1SecondUpload)
	}
}
