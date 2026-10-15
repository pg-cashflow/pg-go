package join

import (
	"testing"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestValidateProfileV1Other(t *testing.T) {
	in := ProfileInput{
		Name: "A", PermanentAddress: "p", CurrentAddress: "c",
		EmergencyContactName: "EC", EmergencyPhone: "+919876543211",
		OccupationType: domain.OccupationOther,
		Consent:        true, IDPhotoBytes: []byte{1},
	}
	if err := ValidateProfileV1(in, "+919876543210", 1); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidateProfileV1EmergencySamePhone(t *testing.T) {
	in := ProfileInput{
		EmergencyContactName: "EC", EmergencyPhone: "+919876543210",
		OccupationType: domain.OccupationOther,
	}
	if err := ValidateProfileV1(in, "+919876543210", 1); err != ErrEmergencySameAsLogin {
		t.Fatalf("got %v", err)
	}
}
