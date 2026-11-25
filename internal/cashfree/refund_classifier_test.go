package cashfree

import (
	"errors"
	"net/http"
	"testing"
)

func TestIsRefundDuplicateError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "409 conflict",
			err:  &APIError{StatusCode: http.StatusConflict, Status: "409 Conflict"},
			want: true,
		},
		{
			name: "400 with already_exists body",
			err:  &APIError{StatusCode: http.StatusBadRequest, Body: `{"message": "refund_already_exists"}`},
			want: true,
		},
		{
			name: "400 with already exists body",
			err:  &APIError{StatusCode: http.StatusBadRequest, Body: `{"message": "refund already exists"}`},
			want: true,
		},
		{
			name: "plain error with already exists string",
			err:  errors.New("refund already exists at provider"),
			want: true,
		},
		{
			name: "400 generic error",
			err:  &APIError{StatusCode: http.StatusBadRequest, Body: `{"message": "invalid amount"}`},
			want: false,
		},
		{
			name: "500 server error",
			err:  &APIError{StatusCode: http.StatusInternalServerError, Body: `{"message": "internal error"}`},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRefundDuplicateError(tt.err); got != tt.want {
				t.Errorf("IsRefundDuplicateError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestClassifyRefundGatewayError_408IsAmbiguous(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want RefundGatewayDisposition
	}{
		{
			name: "408 request timeout is ambiguous",
			err:  &APIError{StatusCode: http.StatusRequestTimeout, Status: "408 Request Timeout"},
			want: RefundAmbiguousFailure,
		},
		{
			name: "400 bad request is definitive failure",
			err:  &APIError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Body: `{"message":"invalid amount"}`},
			want: RefundDefinitiveFailure,
		},
		{
			name: "429 rate limit is ambiguous",
			err:  &APIError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests"},
			want: RefundAmbiguousFailure,
		},
		{
			name: "502 bad gateway is ambiguous",
			err:  &APIError{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway"},
			want: RefundAmbiguousFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyRefundGatewayError(tt.err); got != tt.want {
				t.Errorf("ClassifyRefundGatewayError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
