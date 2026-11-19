package postgres

import "testing"

func TestOnboardingSettingsPatch_InvalidBps(t *testing.T) {
	patch := OnboardingSettingsPatch{JoinerDiscountBps: intPtr(20000)}
	if err := patch.Validate(); err == nil {
		t.Fatal("expected invalid bps error")
	}
}

func intPtr(v int) *int { return &v }
