package search

import (
	"strings"
	"testing"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestValidQueryLength(t *testing.T) {
	if ValidQueryLength("") {
		t.Errorf("empty string should be invalid")
	}
	if ValidQueryLength("a") {
		t.Errorf("single character non-token should be invalid")
	}
	if !ValidQueryLength("ab") {
		t.Errorf("'ab' should be valid")
	}
	longStr := strings.Repeat("x", 101)
	if ValidQueryLength(longStr) {
		t.Errorf("string > 100 characters should be invalid")
	}
	maxValid := strings.Repeat("x", 100)
	if !ValidQueryLength(maxValid) {
		t.Errorf("string of 100 characters should be valid")
	}

	// 40 runes of Devanagari (3 bytes each = 120 bytes)
	devanagari40 := strings.Repeat("र", 40)
	if !ValidQueryLength(devanagari40) {
		t.Errorf("40-rune Devanagari string (%d bytes) should be valid", len(devanagari40))
	}
	devanagari101 := strings.Repeat("र", 101)
	if ValidQueryLength(devanagari101) {
		t.Errorf("101-rune Devanagari string should be invalid")
	}
}

func TestExtractPhoneDigits(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"+91 98765 43210", "9876543210"},
		{"+91-98765-43210", "9876543210"},
		{"09876543210", "9876543210"},
		{"9876543210", "9876543210"},
		{"12345", "12345"},
		{"NoDigitsHere", ""},
	}

	for _, tc := range cases {
		got := ExtractPhoneDigits(tc.input)
		if got != tc.expected {
			t.Errorf("ExtractPhoneDigits(%q) = %q; want %q", tc.input, got, tc.expected)
		}
	}
}

func TestAllowedDocumentEntityTypes(t *testing.T) {
	owner := AllowedDocumentEntityTypes(domain.RoleOwner)
	if len(owner) != 4 {
		t.Errorf("expected 4 allowed doc types for owner, got %d", len(owner))
	}

	mgr := AllowedDocumentEntityTypes(domain.RoleManager)
	for _, docType := range mgr {
		if docType == "payment_note" {
			t.Errorf("manager should not be allowed payment_note document type")
		}
	}

	tenant := AllowedDocumentEntityTypes(domain.RoleTenant)
	for _, docType := range tenant {
		if docType == "inspection" || docType == "payment_note" {
			t.Errorf("tenant should not be allowed %s document type", docType)
		}
	}
}
