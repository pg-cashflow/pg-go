package finance

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestMergeFinanceConfig_PartialUpdatePreservesStoredValues(t *testing.T) {
	st := NewMemoryStore()
	pub := &capturePublisher{}
	svc := NewService(st, pub)
	ctx := context.Background()
	pid := uuid.New()

	// Initial ensure defaults
	if err := st.EnsureDefaults(ctx, pid); err != nil {
		t.Fatalf("ensure defaults failed: %v", err)
	}

	curP, err := st.GetPolicy(ctx, pid)
	if err != nil {
		t.Fatalf("get policy failed: %v", err)
	}
	if !curP.EmergencyBypassEnabled {
		t.Fatalf("expected EmergencyBypassEnabled to default to true")
	}
	origDaily := curP.ManagerDailyLimitPaise

	// Update only SingleExpenseLimitPaise
	newSingle := int64(600_000)
	polPatch := &domain.ApprovalPolicyPatch{
		SingleExpenseLimitPaise: &newSingle,
	}

	stOut, polOut, _, err := svc.MergeFinanceConfig(ctx, pid, nil, polPatch, nil)
	if err != nil {
		t.Fatalf("MergeFinanceConfig failed: %v", err)
	}

	if polOut.SingleExpenseLimitPaise != 600_000 {
		t.Errorf("SingleExpenseLimitPaise = %d, want 600000", polOut.SingleExpenseLimitPaise)
	}
	if polOut.ManagerDailyLimitPaise != origDaily {
		t.Errorf("ManagerDailyLimitPaise = %d, want preserved %d", polOut.ManagerDailyLimitPaise, origDaily)
	}
	if !polOut.EmergencyBypassEnabled {
		t.Errorf("EmergencyBypassEnabled was reset to false, expected true to be preserved")
	}
	if stOut.FiscalMonthStartDay != 1 {
		t.Errorf("FiscalMonthStartDay = %d, want preserved 1", stOut.FiscalMonthStartDay)
	}
}

func TestMergeFinanceConfig_ValidationRejectsInvalidValues(t *testing.T) {
	st := NewMemoryStore()
	pub := &capturePublisher{}
	svc := NewService(st, pub)
	ctx := context.Background()
	pid := uuid.New()

	cases := []struct {
		name     string
		setPatch *domain.FinanceSettingsPatch
		polPatch *domain.ApprovalPolicyPatch
	}{
		{
			name: "negative daily limit",
			polPatch: func() *domain.ApprovalPolicyPatch {
				v := int64(-500)
				return &domain.ApprovalPolicyPatch{ManagerDailyLimitPaise: &v}
			}(),
		},
		{
			name: "negative single limit",
			polPatch: func() *domain.ApprovalPolicyPatch {
				v := int64(-1)
				return &domain.ApprovalPolicyPatch{SingleExpenseLimitPaise: &v}
			}(),
		},
		{
			name: "oversized daily limit exceeding 10 crore",
			polPatch: func() *domain.ApprovalPolicyPatch {
				v := int64(2_000_000_000_00)
				return &domain.ApprovalPolicyPatch{ManagerDailyLimitPaise: &v}
			}(),
		},
		{
			name: "fiscal day zero",
			setPatch: func() *domain.FinanceSettingsPatch {
				v := int16(0)
				return &domain.FinanceSettingsPatch{FiscalMonthStartDay: &v}
			}(),
		},
		{
			name: "fiscal day negative",
			setPatch: func() *domain.FinanceSettingsPatch {
				v := int16(-3)
				return &domain.FinanceSettingsPatch{FiscalMonthStartDay: &v}
			}(),
		},
		{
			name: "fiscal day exceeds 31",
			setPatch: func() *domain.FinanceSettingsPatch {
				v := int16(32)
				return &domain.FinanceSettingsPatch{FiscalMonthStartDay: &v}
			}(),
		},
		{
			name: "tdr bps negative",
			setPatch: func() *domain.FinanceSettingsPatch {
				v := -10
				return &domain.FinanceSettingsPatch{TDREffectiveBPS: &v}
			}(),
		},
		{
			name: "tdr bps exceeds 10000",
			setPatch: func() *domain.FinanceSettingsPatch {
				v := 10001
				return &domain.FinanceSettingsPatch{TDREffectiveBPS: &v}
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := svc.MergeFinanceConfig(ctx, pid, tc.setPatch, tc.polPatch, nil)
			if err == nil {
				t.Fatalf("expected error for case %s, got nil", tc.name)
			}
			if !errors.Is(err, ErrInvalidSettings) {
				t.Fatalf("expected ErrInvalidSettings, got: %v", err)
			}
		})
	}
}

func TestPatchUnifiedSettings_Validation(t *testing.T) {
	st := NewMemoryStore()
	pub := &capturePublisher{}
	svc := NewService(st, pub)
	ctx := context.Background()
	pid := uuid.New()

	badPolicy := domain.ApprovalPolicy{
		ManagerDailyLimitPaise: -100,
	}
	_, _, _, err := svc.PatchUnifiedSettings(ctx, pid, nil, &badPolicy, nil)
	if !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("expected ErrInvalidSettings for negative limit, got %v", err)
	}

	badSettings := domain.PropertyFinanceSettings{
		FiscalMonthStartDay: 35,
	}
	_, _, _, err = svc.PatchUnifiedSettings(ctx, pid, &badSettings, nil, nil)
	if !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("expected ErrInvalidSettings for fiscal day 35, got %v", err)
	}
}
