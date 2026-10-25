package domain

import "testing"

func TestNormalizeRefundStatus(t *testing.T) {
	cases := map[string]string{
		"ONHOLD":         RefundStatusOnHold,
		"onhold":         RefundStatusOnHold,
		"ON_HOLD":        RefundStatusOnHold,
		"on_hold":        RefundStatusOnHold,
		"  on_hold  ":    RefundStatusOnHold,
		"  ONHOLD  ":     RefundStatusOnHold,
		"SUCCESS":        RefundStatusSucceeded,
		"success":        RefundStatusSucceeded,
		"succeeded":      RefundStatusSucceeded,
		"CANCELED":       RefundStatusCancelled,
		"CANCELLED":      RefundStatusCancelled,
		"cancelled":      RefundStatusCancelled,
		"PENDING":        RefundStatusPending,
		"pending":        RefundStatusPending,
		"FAILED":         RefundStatusFailed,
		"failed":         RefundStatusFailed,
		"INITIATED":      RefundStatusInitiated,
		"initiated":      RefundStatusInitiated,
		"unknown_status": RefundStatusPending,
		"":               RefundStatusPending,
	}
	for in, want := range cases {
		if got := NormalizeRefundStatus(in); got != want {
			t.Errorf("NormalizeRefundStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
