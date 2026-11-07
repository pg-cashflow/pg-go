package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/pg-cashflow/pg-go/internal/cashfree"
)

func TestClassifyRefundGatewayError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want RefundGatewayDisposition
	}{
		{
			name: "nil error yields ambiguous",
			err:  nil,
			want: RefundAmbiguousFailure,
		},
		{
			name: "409 conflict yields ambiguous to preserve reservation",
			err:  &cashfree.APIError{StatusCode: http.StatusConflict, Status: "409 Conflict"},
			want: RefundAmbiguousFailure,
		},
		{
			name: "401 unauthorized yields ambiguous to preserve reservation on config/cred failure",
			err:  &cashfree.APIError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"},
			want: RefundAmbiguousFailure,
		},
		{
			name: "403 forbidden yields ambiguous to preserve reservation on IP/perm failure",
			err:  &cashfree.APIError{StatusCode: http.StatusForbidden, Status: "403 Forbidden"},
			want: RefundAmbiguousFailure,
		},
		{
			name: "429 rate limit yields ambiguous to preserve reservation",
			err:  &cashfree.APIError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests"},
			want: RefundAmbiguousFailure,
		},
		{
			name: "500 internal server error yields ambiguous",
			err:  &cashfree.APIError{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error"},
			want: RefundAmbiguousFailure,
		},
		{
			name: "502 bad gateway yields ambiguous",
			err:  &cashfree.APIError{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway"},
			want: RefundAmbiguousFailure,
		},
		{
			name: "503 service unavailable yields ambiguous",
			err:  &cashfree.APIError{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable"},
			want: RefundAmbiguousFailure,
		},
		{
			name: "504 gateway timeout yields ambiguous",
			err:  &cashfree.APIError{StatusCode: http.StatusGatewayTimeout, Status: "504 Gateway Timeout"},
			want: RefundAmbiguousFailure,
		},
		{
			name: "network transport error yields ambiguous",
			err:  errors.New("dial tcp: lookup api.cashfree.com: i/o timeout"),
			want: RefundAmbiguousFailure,
		},
		{
			name: "context deadline exceeded yields ambiguous",
			err:  context.DeadlineExceeded,
			want: RefundAmbiguousFailure,
		},
		{
			name: "400 bad request with refund_already_exists yields ambiguous failure",
			err:  &cashfree.APIError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Body: `{"message": "refund_already_exists"}`},
			want: RefundAmbiguousFailure,
		},
		{
			name: "400 bad request with already exists yields ambiguous failure",
			err:  &cashfree.APIError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Body: `{"message": "refund already exists for this order"}`},
			want: RefundAmbiguousFailure,
		},
		{
			name: "400 bad request yields definitive failure",
			err:  &cashfree.APIError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"},
			want: RefundDefinitiveFailure,
		},
		{
			name: "404 not found yields definitive failure",
			err:  &cashfree.APIError{StatusCode: http.StatusNotFound, Status: "404 Not Found"},
			want: RefundDefinitiveFailure,
		},
		{
			name: "422 unprocessable entity yields definitive failure",
			err:  &cashfree.APIError{StatusCode: http.StatusUnprocessableEntity, Status: "422 Unprocessable Entity"},
			want: RefundDefinitiveFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRefundGatewayError(tt.err)
			if got != tt.want {
				t.Errorf("ClassifyRefundGatewayError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
