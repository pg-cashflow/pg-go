package api

import "testing"

func TestManagerRefundExceedsLimit(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	tests := []struct {
		name   string
		limit  *int64
		amount int64
		want   bool
	}{
		{"nil limit never blocks", nil, 1_000_000_00, false},
		{"below limit", i64(5000_00), 4999_99, false},
		{"equal to limit is allowed", i64(5000_00), 5000_00, false},
		{"above limit blocks", i64(5000_00), 5000_01, true},
		{"zero limit blocks every refund", i64(0), 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := managerRefundExceedsLimit(tc.limit, tc.amount); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
