package api

import (
	"errors"
	"net/http"

	"github.com/pg-cashflow/pg-go/internal/cashfree"
)

// RefundGatewayDisposition describes how the system should handle a gateway refund response.
type RefundGatewayDisposition int

const (
	// RefundDefinitiveFailure indicates an unrecoverable client rejection (e.g. 400 Bad Request, 422 Unprocessable).
	// Safe to mark refund failed and clean up allocations / release reservation.
	RefundDefinitiveFailure RefundGatewayDisposition = iota

	// RefundAmbiguousFailure indicates an ambiguous outcome, state conflict, rate limit, server error, or transport timeout
	// (e.g. 409 Conflict, 429 Too Many Requests, 5xx, or network failure).
	// The reservation and pre-allocated dues MUST be preserved while background reconciliation or webhook processing resolves the outcome.
	RefundAmbiguousFailure
)

// IsRefundDuplicateError reports whether a gateway error represents an already-existing refund
// (such as 409 Conflict, or 400 Bad Request with a duplicate message body).
func IsRefundDuplicateError(err error) bool {
	return cashfree.IsRefundDuplicateError(err)
}

// ClassifyRefundGatewayError determines whether a Cashfree CreateRefund error is a definitive failure
// (safe to clean up) or an ambiguous failure/conflict (must preserve reservation and reconcile).
func ClassifyRefundGatewayError(err error) RefundGatewayDisposition {
	if err == nil {
		return RefundAmbiguousFailure
	}
	var apiErr *cashfree.APIError
	if errors.As(err, &apiErr) {
		// Non-4xx responses (5xx, 0) are server or transport failures
		if apiErr.StatusCode < 400 || apiErr.StatusCode >= 500 {
			return RefundAmbiguousFailure
		}
		// 429 Too Many Requests, 409 Conflict, and auth failures (401/403) must not release reservations.
		// 401/403 indicate gateway credential/IP configuration errors, NOT refund business rejection.
		if apiErr.StatusCode == http.StatusTooManyRequests ||
			apiErr.StatusCode == http.StatusConflict ||
			apiErr.StatusCode == http.StatusUnauthorized ||
			apiErr.StatusCode == http.StatusForbidden {
			return RefundAmbiguousFailure
		}
		// If Cashfree returns 400 with duplicate refund details, treat as ambiguous (preserve reservation for reconciliation)
		if IsRefundDuplicateError(err) {
			return RefundAmbiguousFailure
		}
		// Other 4xx errors (e.g. 400, 404, 422) are definitive client-level rejections.
		return RefundDefinitiveFailure
	}
	// Transport, network errors, and context timeouts are ambiguous
	return RefundAmbiguousFailure
}
