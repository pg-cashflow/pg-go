package postgres

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestNullableInt64Patch_UnmarshalJSON(t *testing.T) {
	type body struct {
		Limit NullableInt64Patch `json:"manager_refund_limit_paise"`
	}
	i64 := func(v int64) *int64 { return &v }
	tests := []struct {
		name      string
		in        string
		wantSet   bool
		wantValue *int64
	}{
		{"absent leaves unset", `{}`, false, nil},
		{"null clears the limit", `{"manager_refund_limit_paise":null}`, true, nil},
		{"zero is a real limit", `{"manager_refund_limit_paise":0}`, true, i64(0)},
		{"number sets the limit", `{"manager_refund_limit_paise":500000}`, true, i64(500000)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b body
			if err := json.Unmarshal([]byte(tc.in), &b); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if b.Limit.Set != tc.wantSet {
				t.Fatalf("Set = %v, want %v", b.Limit.Set, tc.wantSet)
			}
			switch {
			case tc.wantValue == nil && b.Limit.Value != nil:
				t.Fatalf("Value = %d, want nil", *b.Limit.Value)
			case tc.wantValue != nil && (b.Limit.Value == nil || *b.Limit.Value != *tc.wantValue):
				t.Fatalf("Value mismatch, want %d", *tc.wantValue)
			}
		})
	}
}

func TestNullableInt64Patch_RejectsNonNumber(t *testing.T) {
	var b struct {
		Limit NullableInt64Patch `json:"x"`
	}
	if err := json.Unmarshal([]byte(`{"x":"abc"}`), &b); err == nil {
		t.Fatal("expected error for string value")
	}
}

func TestOnboardingSettingsPatch_ValidateManagerRefundLimit(t *testing.T) {
	neg := int64(-1)
	zero := int64(0)
	if err := (OnboardingSettingsPatch{ManagerRefundLimitPaise: NullableInt64Patch{Set: true, Value: &neg}}).Validate(); !errors.Is(err, ErrOnboardingSettingsInvalid) {
		t.Fatalf("negative limit: err = %v, want ErrOnboardingSettingsInvalid", err)
	}
	if err := (OnboardingSettingsPatch{ManagerRefundLimitPaise: NullableInt64Patch{Set: true, Value: &zero}}).Validate(); err != nil {
		t.Fatalf("zero limit must be valid: %v", err)
	}
	if err := (OnboardingSettingsPatch{ManagerRefundLimitPaise: NullableInt64Patch{Set: true}}).Validate(); err != nil {
		t.Fatalf("clearing the limit must be valid: %v", err)
	}
}
