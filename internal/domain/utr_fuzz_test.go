package domain_test

import (
	"strings"
	"testing"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// FuzzNormalizeUTR validates Gate 10: native Go fuzzing for UTR string normalization.
// Invariant: NormalizeUTR must never panic on arbitrary string inputs and must
// return uppercase trimmed alphanumeric UTRs (6-50 chars) or a non-nil error.
func FuzzNormalizeUTR(f *testing.F) {
	seeds := []string{
		"CMS123456789012",
		"UTR999988887777",
		"123456789012",
		"hdfc123456789012",
		"   sbi123456789012345   ",
		"",
		"too_s",
		"this_utr_is_way_too_long_to_be_valid_in_any_bank_exceeding_fifty_characters_1234567890",
		"special!@#chars$%",
		"\x00\x01\x02\xff",
	}

	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		res, err := domain.NormalizeUTR(raw)
		if err == nil {
			// Invariant on success:
			// 1. Result must be trimmed and uppercase
			if res != strings.ToUpper(strings.TrimSpace(raw)) {
				t.Fatalf("NormalizeUTR(%q) = %q not uppercase trimmed", raw, res)
			}
			// 2. Length must be within 6..50 per domain utrRegex
			if len(res) < 6 || len(res) > 50 {
				t.Fatalf("NormalizeUTR(%q) = %q has invalid length %d", raw, res, len(res))
			}
		}
	})
}
