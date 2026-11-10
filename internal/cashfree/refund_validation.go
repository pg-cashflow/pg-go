package cashfree

import (
	"fmt"
	"strings"
)

// KnownCashfreeRefundStatuses defines the set of recognized Cashfree refund statuses.
var KnownCashfreeRefundStatuses = map[string]struct{}{
	"SUCCESS":          {},
	"ONHOLD":           {},
	"ON_HOLD":          {},
	"PENDING":          {},
	"PENDING_APPROVAL": {},
	"CANCELLED":        {},
	"CANCELED":         {},
	"FAILED":           {},
	"FLAGGED":          {},
	"REJECTED":         {},
}

// ValidateRefundResponse enforces strict command binding and response verification
// against Cashfree refund responses. It checks for nil references, missing identifiers,
// order and refund reference matching, amount parity, and recognized status enums.
func ValidateRefundResponse(expectedOrderID, expectedRefundRef string, expectedAmountPaise int64, resp *RefundDetails) error {
	if resp == nil {
		return fmt.Errorf("cashfree refund validation: response is nil")
	}

	trimmedCFRefundID := strings.TrimSpace(resp.CFRefundID)
	if trimmedCFRefundID == "" {
		return fmt.Errorf("cashfree refund validation: missing cf_refund_id")
	}

	trimmedOrderID := strings.TrimSpace(resp.OrderID)
	if expectedOrderID != "" && trimmedOrderID != "" && trimmedOrderID != strings.TrimSpace(expectedOrderID) {
		return fmt.Errorf("cashfree refund validation: order_id mismatch: expected %q, got %q", expectedOrderID, resp.OrderID)
	}

	trimmedRefundID := strings.TrimSpace(resp.RefundID)
	if expectedRefundRef != "" && trimmedRefundID != "" && trimmedRefundID != strings.TrimSpace(expectedRefundRef) {
		return fmt.Errorf("cashfree refund validation: refund_id mismatch: expected %q, got %q", expectedRefundRef, resp.RefundID)
	}

	if expectedAmountPaise > 0 && resp.AmountPaise != expectedAmountPaise {
		return fmt.Errorf("cashfree refund validation: amount mismatch: expected %d paise, got %d paise", expectedAmountPaise, resp.AmountPaise)
	}

	status := strings.ToUpper(strings.TrimSpace(resp.RefundStatus))
	if status == "" {
		return fmt.Errorf("cashfree refund validation: missing refund_status")
	}

	if _, ok := KnownCashfreeRefundStatuses[status]; !ok {
		return fmt.Errorf("cashfree refund validation: unrecognized refund status: %q", resp.RefundStatus)
	}

	return nil
}
