package cashfree

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	ErrInvalidDecimal = errors.New("cashfree: invalid decimal amount")
)

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ParseRupeesToPaise parses a decimal rupee string (e.g. "5500.00", "120.50", "300")
// into integer paise (e.g. 550000, 12050, 30000) using exact string manipulation.
// It avoids any floating-point arithmetic (no float * 100) to prevent precision drift
// and rejects malformed precision or values outside int64.
func ParseRupeesToPaise(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("%w: empty string", ErrInvalidDecimal)
	}

	negative := false
	if strings.HasPrefix(s, "-") {
		negative = true
		s = strings.TrimPrefix(s, "-")
	} else if strings.HasPrefix(s, "+") {
		s = strings.TrimPrefix(s, "+")
	}
	if s == "" || s == "." {
		return 0, fmt.Errorf("%w: invalid decimal %q", ErrInvalidDecimal, raw)
	}

	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return 0, fmt.Errorf("%w: multiple decimal points in %q", ErrInvalidDecimal, raw)
	}

	wholeStr := parts[0]
	if wholeStr == "" {
		wholeStr = "0"
	} else if !isASCIIDigits(wholeStr) {
		return 0, fmt.Errorf("%w: invalid whole number %q", ErrInvalidDecimal, raw)
	}
	whole, err := strconv.ParseUint(wholeStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid whole number %q", ErrInvalidDecimal, raw)
	}

	var paise uint64
	if len(parts) == 2 {
		fracStr := parts[1]
		if len(fracStr) == 0 {
			// e.g. "5500."
			paise = 0
		} else if !isASCIIDigits(fracStr) {
			return 0, fmt.Errorf("%w: invalid fraction %q", ErrInvalidDecimal, raw)
		} else if len(fracStr) == 1 {
			// e.g. "5500.5" -> 50 paise
			paise = uint64(fracStr[0]-'0') * 10
		} else if len(fracStr) == 2 {
			// e.g. "5500.50" -> 50 paise
			paise = uint64(fracStr[0]-'0')*10 + uint64(fracStr[1]-'0')
		} else {
			return 0, fmt.Errorf("%w: more than 2 decimal places in %q", ErrInvalidDecimal, raw)
		}
	}

	const maxInt64 = uint64(math.MaxInt64)
	const minInt64Mag = uint64(math.MaxInt64) + 1
	limit := maxInt64
	if negative {
		limit = minInt64Mag
	}
	if whole > (limit-paise)/100 {
		return 0, fmt.Errorf("%w: amount overflows int64 %q", ErrInvalidDecimal, raw)
	}
	mag := whole*100 + paise
	if negative {
		if mag == minInt64Mag {
			return math.MinInt64, nil
		}
		return -int64(mag), nil
	}
	return int64(mag), nil
}

// FormatPaiseToRupees formats integer paise into a standard decimal rupees string with 2 decimal places (e.g. 550000 -> "5500.00").
func FormatPaiseToRupees(paise int64) string {
	whole := paise / 100
	rem := paise % 100
	if rem < 0 {
		rem = -rem
	}
	if paise < 0 && whole == 0 {
		return fmt.Sprintf("-0.%02d", rem)
	}
	return fmt.Sprintf("%d.%02d", whole, rem)
}
