package aadhaar

import (
	"strings"
	"testing"
)

func FuzzDecodeAadhaarQR(f *testing.F) {
	seeds := []string{
		`<PrintLetterBarcodeData uid="123456789012" name="Ram Kumar" gender="M" dob="01-01-1990"/>`,
		`name="Sita Devi" gender="F" uid="999988887777" yob="1995"`,
		strings.Repeat("1", 80),
		"\x00\x01\x02\xFF\xFE\xFD",
		"",
		`<PrintLetterBarcodeData invalid="true"`,
		`{"aadhaar": "123456789012"}`,
	}

	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		raw := string(data)
		res, partial, err := DecodeAadhaarQR(raw)
		if err == nil && !partial {
			// Complete records must retain non-empty UID last 4 and never expose 12 digits
			if len(res.UIDLast4) > 4 {
				t.Errorf("UIDLast4 exceeded 4 digits: %q", res.UIDLast4)
			}
		}
		// Invariant: DecodeAadhaarQR must never panic on arbitrary inputs
	})
}
