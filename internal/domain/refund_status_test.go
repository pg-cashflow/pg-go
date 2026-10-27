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

func TestMergeRefundStatus(t *testing.T) {
	cases := []struct{ cur, in, want string }{
		{RefundStatusInitiated, RefundStatusPending, RefundStatusPending},
		{RefundStatusPending, RefundStatusSucceeded, RefundStatusSucceeded},
		{RefundStatusPending, RefundStatusFailed, RefundStatusFailed},
		{RefundStatusOnHold, RefundStatusPending, RefundStatusPending},
		{RefundStatusSucceeded, RefundStatusPending, RefundStatusSucceeded},
		{RefundStatusSucceeded, RefundStatusFailed, RefundStatusSucceeded},
		{RefundStatusSucceeded, RefundStatusSucceeded, RefundStatusSucceeded},
		{RefundStatusFailed, RefundStatusPending, RefundStatusFailed},
		{RefundStatusFailed, RefundStatusCancelled, RefundStatusFailed},
		{RefundStatusFailed, RefundStatusSucceeded, RefundStatusSucceeded},
		{RefundStatusCancelled, RefundStatusInitiated, RefundStatusCancelled},
		{RefundStatusCancelled, RefundStatusSucceeded, RefundStatusSucceeded},
	}
	for _, c := range cases {
		if got := MergeRefundStatus(c.cur, c.in); got != c.want {
			t.Errorf("Merge(%q,%q)=%q want %q", c.cur, c.in, got, c.want)
		}
	}
	if IsTerminalRefundStatus(RefundStatusPending) || !IsTerminalRefundStatus(RefundStatusFailed) {
		t.Error("IsTerminalRefundStatus wrong")
	}
}
