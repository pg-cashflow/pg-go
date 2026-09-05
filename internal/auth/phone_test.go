package auth

import (
	"testing"
)

func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// Standard valid Indian numbers
		{"bare 10-digit", "9876543210", "+919876543210"},
		{"10-digit with spaces", "98765 43210", "+919876543210"},
		{"10-digit with dashes", "98765-43210", "+919876543210"},
		{"10-digit with parentheses", "(987) 654-3210", "+919876543210"},
		{"leading 0 11-digit", "09876543210", "+919876543210"},
		{"12-digit starting with 91", "919876543210", "+919876543210"},
		{"full E.164 +91", "+919876543210", "+919876543210"},
		{"full E.164 +91 with spaces", "+91 98765 43210", "+919876543210"},

		// Valid International numbers
		{"US number with +1", "+14155552671", "+14155552671"},
		{"UK number with +44", "+447911123456", "+447911123456"},

		// Invalid / ambiguous / garbage inputs
		{"empty string", "", ""},
		{"only spaces", "   ", ""},
		{"only plus", "+", ""},
		{"plus with short digits", "+12345", ""},
		{"too short bare number", "12345", ""},
		{"9 digits", "987654321", ""},
		{"letters only", "abcdefghij", ""},
		{"mixed letters and digits", "98765abcde", ""},
		{"too long (>15 digits)", "+1234567890123456", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizePhone(tt.input)
			if got != tt.expected {
				t.Errorf("NormalizePhone(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}
