package domain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestReferralAdmissionJSONSerialization(t *testing.T) {
	refID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	propID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	referrerID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	dest := domain.ReferralDestReferrerCredit
	setBy := domain.ReferralSetByTenant

	ref := domain.ReferralAdmission{
		ID:                    refID,
		PropertyID:            propID,
		ReferrerTenantID:      &referrerID,
		ReferrerExternalPhone: "+919876543210",
		ReferrerExternalName:  "Test Referrer",
		Phone:                 "+919876543211",
		Name:                  "Test Joiner",
		ReferralCode:          "REF123",
		JoinerDiscountBps:     500,
		ReferrerRewardBps:     500,
		RewardDestination:     &dest,
		DestinationSetBy:      &setBy,
		DestinationSetAt:      &now,
		Status:                "pending",
		PointsAwarded:         0,
		CreatedAt:             now,
	}

	bytes, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(bytes, &raw); err != nil {
		t.Fatalf("unmarshal into map failed: %v", err)
	}

	expectedKeys := []string{
		"id", "property_id", "referrer_tenant_id", "referrer_external_phone",
		"referrer_external_name", "phone", "name", "referral_code",
		"joiner_discount_bps", "referrer_reward_bps", "reward_destination",
		"destination_set_by", "destination_set_at", "status", "points_awarded",
		"created_at",
	}

	for _, k := range expectedKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing expected json key %q in payload: %s", k, string(bytes))
		}
	}

	// Verify no PascalCase keys remain
	for k := range raw {
		if k[0] >= 'A' && k[0] <= 'Z' {
			t.Errorf("found disallowed PascalCase json key %q in payload: %s", k, string(bytes))
		}
	}
}
