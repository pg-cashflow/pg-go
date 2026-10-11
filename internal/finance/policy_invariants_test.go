package finance_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
)

func TestValidateFinanceSettings_FiscalRange(t *testing.T) {
	tests := []struct {
		name    string
		day     int16
		tdrBPS  int
		wantErr bool
	}{
		{"lower_bound_valid", 1, 100, false},
		{"mid_valid", 15, 100, false},
		{"upper_bound_valid_28", 28, 100, false},
		{"zero_invalid", 0, 100, true},
		{"negative_invalid", -1, 100, true},
		{"february_overflow_29", 29, 100, true},
		{"month_overflow_30", 30, 100, true},
		{"month_overflow_31", 31, 100, true},
		{"overflow_32", 32, 100, true},
		{"tdr_bps_zero_valid", 10, 0, false},
		{"tdr_bps_max_valid", 10, 10000, false},
		{"tdr_bps_negative_invalid", 10, -1, true},
		{"tdr_bps_overflow_invalid", 10, 10001, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := domain.PropertyFinanceSettings{
				FiscalMonthStartDay: tc.day,
				TDREffectiveBPS:     tc.tdrBPS,
			}
			err := finance.ValidateFinanceSettings(s)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateFinanceSettings(day=%d, tdr=%d) err = %v, wantErr = %v", tc.day, tc.tdrBPS, err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, finance.ErrInvalidSettings) {
				t.Fatalf("expected ErrInvalidSettings, got %v", err)
			}
		})
	}
}

func TestValidateApprovalPolicy_Invariants(t *testing.T) {
	validBase := domain.ApprovalPolicy{
		SingleExpenseLimitPaise:     500_000,
		ManagerDailyLimitPaise:      1_000_000,
		ManagerMonthlyLimitPaise:    5_000_000,
		OwnerApprovalThresholdPaise: 500_000,
		ReimbursementThresholdPaise: 250_000,
	}

	tests := []struct {
		name    string
		modify  func(p *domain.ApprovalPolicy)
		wantErr bool
	}{
		{
			name:    "valid_strict_ordering",
			modify:  func(p *domain.ApprovalPolicy) {},
			wantErr: false,
		},
		{
			name: "valid_equal_boundaries",
			modify: func(p *domain.ApprovalPolicy) {
				p.SingleExpenseLimitPaise = 1_000_000
				p.ManagerDailyLimitPaise = 1_000_000
				p.ManagerMonthlyLimitPaise = 1_000_000
			},
			wantErr: false,
		},
		{
			name: "violation_single_greater_than_daily",
			modify: func(p *domain.ApprovalPolicy) {
				p.SingleExpenseLimitPaise = 2_000_000
				p.ManagerDailyLimitPaise = 1_000_000
			},
			wantErr: true,
		},
		{
			name: "violation_daily_greater_than_monthly",
			modify: func(p *domain.ApprovalPolicy) {
				p.ManagerDailyLimitPaise = 6_000_000
				p.ManagerMonthlyLimitPaise = 5_000_000
			},
			wantErr: true,
		},
		{
			name: "violation_negative_single",
			modify: func(p *domain.ApprovalPolicy) {
				p.SingleExpenseLimitPaise = -1
			},
			wantErr: true,
		},
		{
			name: "violation_negative_daily",
			modify: func(p *domain.ApprovalPolicy) {
				p.ManagerDailyLimitPaise = -1
			},
			wantErr: true,
		},
		{
			name: "violation_negative_monthly",
			modify: func(p *domain.ApprovalPolicy) {
				p.ManagerMonthlyLimitPaise = -1
			},
			wantErr: true,
		},
		{
			name: "violation_negative_owner_threshold",
			modify: func(p *domain.ApprovalPolicy) {
				p.OwnerApprovalThresholdPaise = -1
			},
			wantErr: true,
		},
		{
			name: "violation_negative_reimbursement",
			modify: func(p *domain.ApprovalPolicy) {
				p.ReimbursementThresholdPaise = -1
			},
			wantErr: true,
		},
		{
			name: "valid_at_ceiling",
			modify: func(p *domain.ApprovalPolicy) {
				p.SingleExpenseLimitPaise = finance.MaxExpenseLimitPaise
				p.ManagerDailyLimitPaise = finance.MaxExpenseLimitPaise
				p.ManagerMonthlyLimitPaise = finance.MaxExpenseLimitPaise
				p.OwnerApprovalThresholdPaise = finance.MaxExpenseLimitPaise
				p.ReimbursementThresholdPaise = finance.MaxExpenseLimitPaise
			},
			wantErr: false,
		},
		{
			name: "violation_single_above_ceiling",
			modify: func(p *domain.ApprovalPolicy) {
				p.SingleExpenseLimitPaise = finance.MaxExpenseLimitPaise + 1
			},
			wantErr: true,
		},
		{
			name: "violation_daily_above_ceiling",
			modify: func(p *domain.ApprovalPolicy) {
				p.ManagerDailyLimitPaise = finance.MaxExpenseLimitPaise + 1
				p.ManagerMonthlyLimitPaise = finance.MaxExpenseLimitPaise + 1
			},
			wantErr: true,
		},
		{
			name: "violation_monthly_above_ceiling",
			modify: func(p *domain.ApprovalPolicy) {
				p.ManagerMonthlyLimitPaise = finance.MaxExpenseLimitPaise + 1
			},
			wantErr: true,
		},
		{
			name: "violation_owner_threshold_above_ceiling",
			modify: func(p *domain.ApprovalPolicy) {
				p.OwnerApprovalThresholdPaise = finance.MaxExpenseLimitPaise + 1
			},
			wantErr: true,
		},
		{
			name: "violation_reimbursement_above_ceiling",
			modify: func(p *domain.ApprovalPolicy) {
				p.ReimbursementThresholdPaise = finance.MaxExpenseLimitPaise + 1
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validBase
			tc.modify(&p)
			err := finance.ValidateApprovalPolicy(p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateApprovalPolicy() err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, finance.ErrInvalidSettings) {
				t.Fatalf("expected ErrInvalidSettings, got %v", err)
			}
		})
	}
}

func TestErrorIdentityAndSpendDelegation(t *testing.T) {
	// Error identity check
	if !errors.Is(domain.ErrPolicyExceeded, finance.ErrPolicyExceeded) {
		t.Fatal("domain.ErrPolicyExceeded must be recognized by errors.Is with finance.ErrPolicyExceeded")
	}
	if !errors.Is(domain.ErrInvalidAmount, finance.ErrInvalidAmount) {
		t.Fatal("domain.ErrInvalidAmount must be recognized by errors.Is with finance.ErrInvalidAmount")
	}

	// Delegation check
	pid := uuid.New()
	pol := finance.DefaultPolicy(pid)
	chk := finance.EvaluateManagerSpend(pol, 2_000_000, 0, 0, false)
	if chk.Reject == nil || !errors.Is(chk.Reject, finance.ErrPolicyExceeded) {
		t.Fatalf("expected finance.ErrPolicyExceeded rejection, got %+v", chk)
	}
}
