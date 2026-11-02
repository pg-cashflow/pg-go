package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizeUTR(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		expected  string
		expectErr bool
	}{
		{
			name:     "lowercase with trailing space",
			raw:      "abc123 ",
			expected: "ABC123",
		},
		{
			name:     "already uppercase",
			raw:      "ABC123",
			expected: "ABC123",
		},
		{
			name:     "with leading and trailing spaces",
			raw:      "   xyz9876543210   ",
			expected: "XYZ9876543210",
		},
		{
			name:      "empty string",
			raw:       "   ",
			expectErr: true,
		},
		{
			name:      "too short (less than 6 chars)",
			raw:       "ABC",
			expectErr: true,
		},
		{
			name:      "invalid characters (special symbols)",
			raw:       "ABC123!@#",
			expectErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := NormalizeUTR(tc.raw)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got nil", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", tc.raw, err)
			}
			if res != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, res)
			}
		})
	}
}

func TestCalculatePaymentStatus(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	asOf := time.Date(2026, 10, 5, 12, 0, 0, 0, loc)

	t.Run("fully paid", func(t *testing.T) {
		now := time.Now().UTC()
		d := &Due{
			ID:      uuid.New(),
			Status:  DueStatusPaid,
			PaidAt:  &now,
			Amount:  0,
			DueDate: asOf.AddDate(0, 0, -5),
		}
		if status := d.CalculatePaymentStatus(asOf); status != PaymentStatusPaid {
			t.Fatalf("expected paid, got %s", status)
		}
	})

	t.Run("overdue", func(t *testing.T) {
		d := &Due{
			ID:      uuid.New(),
			Status:  DueStatusPending,
			Amount:  500000,
			DueDate: asOf.AddDate(0, 0, -2), // 2 days in the past in IST
		}
		if status := d.CalculatePaymentStatus(asOf); status != PaymentStatusOverdue {
			t.Fatalf("expected overdue, got %s", status)
		}
	})

	t.Run("due on same day", func(t *testing.T) {
		d := &Due{
			ID:      uuid.New(),
			Status:  DueStatusPending,
			Amount:  500000,
			DueDate: time.Date(2026, 10, 5, 18, 0, 0, 0, loc),
		}
		if status := d.CalculatePaymentStatus(asOf); status != PaymentStatusDue {
			t.Fatalf("expected due, got %s", status)
		}
	})

	t.Run("due in future", func(t *testing.T) {
		d := &Due{
			ID:      uuid.New(),
			Status:  DueStatusPending,
			Amount:  500000,
			DueDate: asOf.AddDate(0, 0, 3),
		}
		if status := d.CalculatePaymentStatus(asOf); status != PaymentStatusDue {
			t.Fatalf("expected due, got %s", status)
		}
	})
}
