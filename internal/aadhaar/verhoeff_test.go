package aadhaar_test

import (
	"fmt"
	"testing"

	"github.com/pg-cashflow/pg-go/internal/aadhaar"
)

func TestVerhoeff(t *testing.T) {
	prefix := "29384756102"
	checkDigit, err := aadhaar.GenerateVerhoeffCheckDigit(prefix)
	if err != nil {
		t.Fatalf("unexpected error generating check digit: %v", err)
	}
	validAadhaar := prefix + fmt.Sprintf("%d", checkDigit)
	if !aadhaar.ValidateVerhoeff(validAadhaar) {
		t.Errorf("expected valid Verhoeff for %s", validAadhaar)
	}

	// Corrupted check digit
	corruptDigit := (checkDigit + 1) % 10
	invalidAadhaar := prefix + fmt.Sprintf("%d", corruptDigit)
	if aadhaar.ValidateVerhoeff(invalidAadhaar) {
		t.Errorf("expected invalid Verhoeff for corrupted number %s", invalidAadhaar)
	}
}

func TestValidateAadhaarNumber(t *testing.T) {
	prefix := "29384756102"
	checkDigit, _ := aadhaar.GenerateVerhoeffCheckDigit(prefix)
	validAadhaar := prefix + fmt.Sprintf("%d", checkDigit)

	cases := []struct {
		name    string
		num     string
		wantErr bool
	}{
		{"valid 12-digit", validAadhaar, false},
		{"too short", "29384756102", true},
		{"too long", "2938475610245", true},
		{"contains letters", "29384756102A", true},
		{"leading 0", "093847561025", true},
		{"leading 1", "193847561025", true},
		{"checksum failure", prefix + fmt.Sprintf("%d", (checkDigit+1)%10), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := aadhaar.ValidateAadhaarNumber(tc.num)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateAadhaarNumber(%q) error = %v, wantErr %v", tc.num, err, tc.wantErr)
			}
		})
	}
}

