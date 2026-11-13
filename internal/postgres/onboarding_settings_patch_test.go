package postgres

import "testing"

func TestOnboardingSettingsPatch_InvalidBps(t *testing.T) {
	patch := OnboardingSettingsPatch{JoinerDiscountBps: intPtr(20000)}
	if err := patch.Validate(); err == nil {
		t.Fatal("expected invalid bps error")
	}
}

func TestOnboardingSettingsPatch_CannotDisableKYCBeforePay(t *testing.T) {
	f := false
	patch := OnboardingSettingsPatch{RequireKYCBeforePay: &f}
	if err := patch.Validate(); err == nil {
		t.Fatal("expected error when trying to disable require_kyc_before_pay")
	}

	tr := true
	patchTrue := OnboardingSettingsPatch{RequireKYCBeforePay: &tr}
	if err := patchTrue.Validate(); err != nil {
		t.Fatalf("unexpected error when setting require_kyc_before_pay to true: %v", err)
	}
}

func intPtr(v int) *int { return &v }
