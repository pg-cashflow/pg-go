package cashfree

import (
	"testing"
)

func TestValidateRefundResponse(t *testing.T) {
	valid := &RefundDetails{
		CFRefundID:   "cf_ref_123",
		RefundID:     "ref_abc",
		OrderID:      "order_xyz",
		AmountPaise:  50000,
		RefundStatus: "SUCCESS",
	}

	tests := []struct {
		name         string
		expectedOrd  string
		expectedRef  string
		expectedAmt  int64
		resp         *RefundDetails
		expectErr    bool
		errSubstring string
	}{
		{
			name:        "valid success response",
			expectedOrd: "order_xyz",
			expectedRef: "ref_abc",
			expectedAmt: 50000,
			resp:        valid,
			expectErr:   false,
		},
		{
			name:        "valid on_hold status",
			expectedOrd: "order_xyz",
			expectedRef: "ref_abc",
			expectedAmt: 50000,
			resp: &RefundDetails{
				CFRefundID:   "cf_ref_123",
				RefundID:     "ref_abc",
				OrderID:      "order_xyz",
				AmountPaise:  50000,
				RefundStatus: "ONHOLD",
			},
			expectErr: false,
		},
		{
			name:        "valid cancelled status",
			expectedOrd: "order_xyz",
			expectedRef: "ref_abc",
			expectedAmt: 50000,
			resp: &RefundDetails{
				CFRefundID:   "cf_ref_123",
				RefundID:     "ref_abc",
				OrderID:      "order_xyz",
				AmountPaise:  50000,
				RefundStatus: "CANCELLED",
			},
			expectErr: false,
		},
		{
			name:        "nil response",
			expectedOrd: "order_xyz",
			expectedRef: "ref_abc",
			expectedAmt: 50000,
			resp:        nil,
			expectErr:   true,
		},
		{
			name:        "missing cf_refund_id",
			expectedOrd: "order_xyz",
			expectedRef: "ref_abc",
			expectedAmt: 50000,
			resp: &RefundDetails{
				CFRefundID:   "",
				RefundID:     "ref_abc",
				OrderID:      "order_xyz",
				AmountPaise:  50000,
				RefundStatus: "SUCCESS",
			},
			expectErr: true,
		},
		{
			name:        "order id mismatch",
			expectedOrd: "order_xyz",
			expectedRef: "ref_abc",
			expectedAmt: 50000,
			resp: &RefundDetails{
				CFRefundID:   "cf_ref_123",
				RefundID:     "ref_abc",
				OrderID:      "order_wrong",
				AmountPaise:  50000,
				RefundStatus: "SUCCESS",
			},
			expectErr: true,
		},
		{
			name:        "refund id mismatch",
			expectedOrd: "order_xyz",
			expectedRef: "ref_abc",
			expectedAmt: 50000,
			resp: &RefundDetails{
				CFRefundID:   "cf_ref_123",
				RefundID:     "ref_wrong",
				OrderID:      "order_xyz",
				AmountPaise:  50000,
				RefundStatus: "SUCCESS",
			},
			expectErr: true,
		},
		{
			name:        "amount mismatch",
			expectedOrd: "order_xyz",
			expectedRef: "ref_abc",
			expectedAmt: 50000,
			resp: &RefundDetails{
				CFRefundID:   "cf_ref_123",
				RefundID:     "ref_abc",
				OrderID:      "order_xyz",
				AmountPaise:  25000,
				RefundStatus: "SUCCESS",
			},
			expectErr: true,
		},
		{
			name:        "unrecognized status",
			expectedOrd: "order_xyz",
			expectedRef: "ref_abc",
			expectedAmt: 50000,
			resp: &RefundDetails{
				CFRefundID:   "cf_ref_123",
				RefundID:     "ref_abc",
				OrderID:      "order_xyz",
				AmountPaise:  50000,
				RefundStatus: "SOME_UNKNOWN_STATUS",
			},
			expectErr: true,
		},
		{
			name:        "empty status",
			expectedOrd: "order_xyz",
			expectedRef: "ref_abc",
			expectedAmt: 50000,
			resp: &RefundDetails{
				CFRefundID:   "cf_ref_123",
				RefundID:     "ref_abc",
				OrderID:      "order_xyz",
				AmountPaise:  50000,
				RefundStatus: "",
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRefundResponse(tt.expectedOrd, tt.expectedRef, tt.expectedAmt, tt.resp)
			if (err != nil) != tt.expectErr {
				t.Errorf("ValidateRefundResponse() error = %v, expectErr %v", err, tt.expectErr)
			}
		})
	}
}
