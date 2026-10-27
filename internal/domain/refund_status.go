package domain

// IsTerminalRefundStatus reports whether a refund status is final from the gateway's point of view.
func IsTerminalRefundStatus(s string) bool {
	switch s {
	case RefundStatusSucceeded, RefundStatusFailed, RefundStatusCancelled:
		return true
	}
	return false
}

// MergeRefundStatus returns the status that must be persisted when `incoming` arrives for a row
// currently at `current`. Rules (keep in sync with the CASE in PaymentRepo.CreateOrUpdateRefund):
//   - succeeded is final.
//   - failed/cancelled may only be upgraded to succeeded (gateway truth about money movement).
//   - any non-terminal status accepts the incoming value.
func MergeRefundStatus(current, incoming string) string {
	switch current {
	case RefundStatusSucceeded:
		return current
	case RefundStatusFailed, RefundStatusCancelled:
		if incoming == RefundStatusSucceeded {
			return incoming
		}
		return current
	}
	return incoming
}
