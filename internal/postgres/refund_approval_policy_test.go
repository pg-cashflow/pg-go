package postgres

import (
	"errors"
	"testing"
)

func TestValidateManagerRefundPolicy(t *testing.T) {
	limit := int64(50000)
	tests := []struct {
		name    string
		enabled bool
		limit   *int64
		amount  int64
		wantErr error
	}{
		{name: "disabled", enabled: false, amount: 100, wantErr: ErrManagerRefundsDisabled},
		{name: "no limit", enabled: true, limit: nil, amount: 90000},
		{name: "equal limit allowed", enabled: true, limit: &limit, amount: 50000},
		{name: "over limit rejected", enabled: true, limit: &limit, amount: 50001, wantErr: ErrManagerRefundLimitExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateManagerRefundPolicy(tc.enabled, tc.limit, tc.amount)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
