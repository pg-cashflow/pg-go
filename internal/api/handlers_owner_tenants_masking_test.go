package api

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestTenantResponse_IdentityFieldMasking(t *testing.T) {
	aadhaar := "4321"
	phone := "9876543210"
	guardianPhone := "9123456789"
	guardianRef := "cashfree_kyc_ref_98765"
	guardianName := "Jane Parent"
	guardianRel := "mother"
	now := time.Now().UTC()
	majorityDate := now.Add(365 * 24 * time.Hour) // minor

	tenant := &domain.Tenant{
		ID:                        uuid.New(),
		PropertyID:                uuid.New(),
		Name:                      "Minor Tenant",
		Phone:                     &phone,
		AadhaarLast4:              &aadhaar,
		GuardianName:              &guardianName,
		GuardianPhone:             &guardianPhone,
		GuardianRelation:          &guardianRel,
		GuardianKYCReferenceID:    &guardianRef,
		GuardianConsentVerifiedAt: &now,
		MajorityDate:              &majorityDate,
		Status:                    domain.TenantStatusActive,
		CreatedAt:                 now,
		UpdatedAt:                 now,
	}

	resp := tenantResponse(tenant)

	// 1. Verify Aadhaar last 4 is projected in masked format (UIDAI standard)
	aadhaarVal, ok := resp["aadhaar_last4"].(*string)
	if !ok || aadhaarVal == nil {
		t.Fatalf("expected aadhaar_last4 to be non-nil string pointer")
	}
	if *aadhaarVal != "XXXX-XXXX-4321" {
		t.Errorf("expected aadhaar_last4 'XXXX-XXXX-4321', got %s", *aadhaarVal)
	}

	// 2. Verify Guardian Phone is masked
	gPhoneVal, ok := resp["guardian_phone"].(*string)
	if !ok || gPhoneVal == nil {
		t.Fatalf("expected guardian_phone to be non-nil string pointer")
	}
	if *gPhoneVal != "******6789" {
		t.Errorf("expected guardian_phone '******6789', got %s", *gPhoneVal)
	}

	// 3. Verify Guardian KYC reference ID is masked
	gRefVal, ok := resp["guardian_kyc_reference_id"].(*string)
	if !ok || gRefVal == nil {
		t.Fatalf("expected guardian_kyc_reference_id to be non-nil string pointer")
	}
	if *gRefVal != "ref_***8765" {
		t.Errorf("expected guardian_kyc_reference_id 'ref_***8765', got %s", *gRefVal)
	}

	// 4. Test Nil values are preserved as nil without panic
	tenantNil := &domain.Tenant{
		ID:         uuid.New(),
		PropertyID: uuid.New(),
		Name:       "Adult Tenant",
		Status:     domain.TenantStatusActive,
	}
	respNil := tenantResponse(tenantNil)
	if p, ok := respNil["aadhaar_last4"].(*string); ok && p != nil {
		t.Errorf("expected nil aadhaar_last4, got %v", *p)
	}
	if p, ok := respNil["guardian_phone"].(*string); ok && p != nil {
		t.Errorf("expected nil guardian_phone, got %v", *p)
	}
	if p, ok := respNil["guardian_kyc_reference_id"].(*string); ok && p != nil {
		t.Errorf("expected nil guardian_kyc_reference_id, got %v", *p)
	}
}
