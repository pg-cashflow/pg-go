package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCalculatePaymentOptions(t *testing.T) {
	tenantID := uuid.New()
	propID := uuid.New()
	now := time.Now().UTC()

	// 1. Zero open dues
	t.Run("Zero open dues", func(t *testing.T) {
		opts := CalculatePaymentOptions([]*Due{})
		if len(opts) != 0 {
			t.Fatalf("expected 0 options, got %d", len(opts))
		}
	})

	// 2. Only paid/waived dues
	t.Run("Only paid and waived dues", func(t *testing.T) {
		dues := []*Due{
			{ID: uuid.New(), Status: DueStatusPaid, Amount: 0, OriginalAmount: 550000},
			{ID: uuid.New(), Status: DueStatusWaived, Amount: 0, OriginalAmount: 550000},
		}
		opts := CalculatePaymentOptions(dues)
		if len(opts) != 0 {
			t.Fatalf("expected 0 options, got %d", len(opts))
		}
	})

	// 3. Exactly 1 open due
	t.Run("1 open due", func(t *testing.T) {
		d1 := &Due{
			ID:             uuid.New(),
			DueCode:        "DUE001",
			TenantID:       tenantID,
			PropertyID:     propID,
			Amount:         550000,
			OriginalAmount: 550000,
			Status:         DueStatusPending,
			DueDate:        now,
			Kind:           DueKindRent,
		}
		opts := CalculatePaymentOptions([]*Due{d1})
		if len(opts) != 1 {
			t.Fatalf("expected 1 option, got %d", len(opts))
		}
		if opts[0].OptionType != OptionOldest1 || opts[0].AmountPaise != 550000 || opts[0].DueCount != 1 {
			t.Fatalf("unexpected option 0: %+v", opts[0])
		}
		if opts[0].DueIDs[0] != d1.ID {
			t.Fatalf("expected due ID %v, got %v", d1.ID, opts[0].DueIDs[0])
		}
	})

	// 4. Exactly 2 open dues (FIFO order)
	t.Run("2 open dues", func(t *testing.T) {
		d1 := &Due{
			ID:             uuid.New(),
			DueCode:        "DUE001",
			TenantID:       tenantID,
			PropertyID:     propID,
			Amount:         550000,
			OriginalAmount: 550000,
			Status:         DueStatusPending,
			DueDate:        now.Add(-30 * 24 * time.Hour), // Older
			Kind:           DueKindRent,
		}
		d2 := &Due{
			ID:             uuid.New(),
			DueCode:        "DUE002",
			TenantID:       tenantID,
			PropertyID:     propID,
			Amount:         600000,
			OriginalAmount: 600000,
			Status:         DueStatusPending,
			DueDate:        now, // Newer
			Kind:           DueKindRent,
		}
		// Pass in reverse order to ensure FIFO sort operates
		opts := CalculatePaymentOptions([]*Due{d2, d1})
		if len(opts) != 2 {
			t.Fatalf("expected 2 options, got %d", len(opts))
		}
		// Option 1: Oldest 1
		if opts[0].OptionType != OptionOldest1 || opts[0].AmountPaise != 550000 || opts[0].DueCount != 1 {
			t.Fatalf("unexpected option 0: %+v", opts[0])
		}
		if opts[0].DueIDs[0] != d1.ID {
			t.Fatalf("expected oldest due d1, got %v", opts[0].DueIDs[0])
		}
		// Option 2: Clear All 2
		if opts[1].OptionType != OptionAll || opts[1].AmountPaise != 1150000 || opts[1].DueCount != 2 {
			t.Fatalf("unexpected option 1: %+v", opts[1])
		}
		if opts[1].DueIDs[0] != d1.ID || opts[1].DueIDs[1] != d2.ID {
			t.Fatalf("expected [d1, d2], got %v", opts[1].DueIDs)
		}
	})

	// 5. 3 open dues (1, 2, all)
	t.Run("3 open dues", func(t *testing.T) {
		d1 := &Due{
			ID:             uuid.New(),
			DueCode:        "DUE001",
			Amount:         500000,
			Status:         DueStatusPending,
			DueDate:        now.Add(-60 * 24 * time.Hour),
		}
		d2 := &Due{
			ID:             uuid.New(),
			DueCode:        "DUE002",
			Amount:         550000,
			Status:         DueStatusPending,
			DueDate:        now.Add(-30 * 24 * time.Hour),
		}
		d3 := &Due{
			ID:             uuid.New(),
			DueCode:        "DUE003",
			Amount:         600000,
			Status:         DueStatusPartial,
			DueDate:        now,
		}
		opts := CalculatePaymentOptions([]*Due{d3, d1, d2})
		if len(opts) != 3 {
			t.Fatalf("expected 3 options, got %d", len(opts))
		}
		// Option 1: Oldest 1
		if opts[0].OptionType != OptionOldest1 || opts[0].AmountPaise != 500000 || opts[0].DueCount != 1 {
			t.Fatalf("opt0: %+v", opts[0])
		}
		// Option 2: Oldest 2
		if opts[1].OptionType != OptionOldest2 || opts[1].AmountPaise != 1050000 || opts[1].DueCount != 2 {
			t.Fatalf("opt1: %+v", opts[1])
		}
		if opts[1].DueIDs[0] != d1.ID || opts[1].DueIDs[1] != d2.ID {
			t.Fatalf("opt1 dueIDs: %v", opts[1].DueIDs)
		}
		// Option 3: Clear All 3
		if opts[2].OptionType != OptionAll || opts[2].AmountPaise != 1650000 || opts[2].DueCount != 3 {
			t.Fatalf("opt2: %+v", opts[2])
		}
		if opts[2].DueIDs[0] != d1.ID || opts[2].DueIDs[1] != d2.ID || opts[2].DueIDs[2] != d3.ID {
			t.Fatalf("opt2 dueIDs: %v", opts[2].DueIDs)
		}
	})
}
