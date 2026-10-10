package auth

import (
	"strings"
)

// NormalizePhone standardizes phone numbers into strict E.164 format.
// Valid Indian phone numbers:
//   - 10 digits (e.g. "9876543210") -> "+919876543210"
//   - 11 digits with leading 0 (e.g. "09876543210") -> "+919876543210"
//   - 12 digits starting with 91 (e.g. "919876543210") -> "+919876543210"
//
// International numbers:
//   - Must start with "+" and have 10 to 15 digits.
//
// Any input that is too short (<10 digits), too long (>15 digits), or malformed returns "".
func NormalizePhone(p string) string {
	raw := strings.TrimSpace(p)
	if raw == "" {
		return ""
	}

	hasPlus := strings.HasPrefix(raw, "+")

	// Extract only digits
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()

	if hasPlus {
		// International E.164: must have between 10 and 15 digits
		if len(d) >= 10 && len(d) <= 15 {
			return "+" + d
		}
		return ""
	}

	// Leading 0 + 10 digits (e.g. "09876543210")
	if len(d) == 11 && strings.HasPrefix(d, "0") {
		d = d[1:]
	}

	// 12 digits starting with 91 (e.g. "919876543210")
	if len(d) == 12 && strings.HasPrefix(d, "91") {
		return "+" + d
	}

	// Standard 10-digit Indian mobile number
	if len(d) == 10 {
		return "+91" + d
	}

	// Ambiguous or malformed
	return ""
}
