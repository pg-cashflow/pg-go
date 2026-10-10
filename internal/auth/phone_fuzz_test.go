package auth_test

import (
	"strings"
	"testing"

	"github.com/pg-cashflow/pg-go/internal/auth"
)

// FuzzNormalizePhone validates Gate 10: native Go fuzzing for phone normalizer.
// Invariant: NormalizePhone must never panic on arbitrary string inputs and must
// return either empty string or a valid E.164 phone string starting with '+' and 10..15 digits.
func FuzzNormalizePhone(f *testing.F) {
	seeds := []string{
		"9876543210",
		"+919876543210",
		"09876543210",
		"919876543210",
		"+14155552671",
		"",
		"123",
		"not_a_phone",
		"+0000000000000000000000000",
		"98765 43210",
		"+91-98765-43210",
		"\x00\xff\xfe",
	}

	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		res := auth.NormalizePhone(raw)
		if res != "" {
			// Invariants on non-empty normalized phone:
			// 1. Must start with '+'
			if !strings.HasPrefix(res, "+") {
				t.Fatalf("NormalizePhone(%q) = %q does not start with +", raw, res)
			}
			digits := res[1:]
			// 2. Length must be between 10 and 15 digits
			if len(digits) < 10 || len(digits) > 15 {
				t.Fatalf("NormalizePhone(%q) = %q digit count %d out of range [10, 15]", raw, res, len(digits))
			}
			// 3. Must only contain ASCII digits
			for _, ch := range digits {
				if ch < '0' || ch > '9' {
					t.Fatalf("NormalizePhone(%q) = %q contains non-digit %c", raw, res, ch)
				}
			}
		}
	})
}
