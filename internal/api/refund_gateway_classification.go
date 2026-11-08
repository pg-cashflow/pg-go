package api

import (
	"github.com/pg-cashflow/pg-go/internal/cashfree"
)

// RefundGatewayDisposition describes how the system should handle a gateway refund response.
type RefundGatewayDisposition = cashfree.RefundGatewayDisposition

const (
	RefundDefinitiveFailure = cashfree.RefundDefinitiveFailure
	RefundAmbiguousFailure  = cashfree.RefundAmbiguousFailure
)

// IsRefundDuplicateError reports whether a gateway error represents an already-existing refund.
func IsRefundDuplicateError(err error) bool {
	return cashfree.IsRefundDuplicateError(err)
}

// ClassifyRefundGatewayError determines whether a Cashfree CreateRefund error is a definitive failure or ambiguous failure.
func ClassifyRefundGatewayError(err error) RefundGatewayDisposition {
	return cashfree.ClassifyRefundGatewayError(err)
}
