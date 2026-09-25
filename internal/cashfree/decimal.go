package cashfree

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	ErrInvalidDecimal = errors.New("cashfree: invalid decimal amount")
)

// ParseRupeesToPaise parses a decimal rupee string (e.g. "5500.00", "120.50", "300")
// into integer paise (e.g. 550000, 12050, 30000) using exact string manipulation.
// It avoids any floating-point arithmetic (no float * 100) to prevent precision drift.
func ParseRupeesToPaise(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("%w: empty string", ErrInvalidDecimal)
	}

	negative := false
	if strings.HasPrefix(s, "-") {
		negative = true
		s = strings.TrimPrefix(s, "-")
	}

	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return 0, fmt.Errorf("%w: multiple decimal points in %q", ErrInvalidDecimal, raw)
	}

	wholeStr := parts[0]
	if wholeStr == "" {
		wholeStr = "0"
	}
	whole, err := strconv.ParseInt(wholeStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid whole number %q", ErrInvalidDecimal, raw)
	}

	var paise int64
	if len(parts) == 2 {
		fracStr := parts[1]
		if len(fracStr) == 0 {
			// e.g. "5500."
			paise = 0
		} else if len(fracStr) == 1 {
			// e.g. "5500.5" -> 50 paise
			d, err := strconv.ParseInt(fracStr, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("%w: invalid fraction %q", ErrInvalidDecimal, raw)
			}
			paise = d * 10
		} else if len(fracStr) == 2 {
			// e.g. "5500.50" -> 50 paise
			d, err := strconv.ParseInt(fracStr, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("%w: invalid fraction %q", ErrInvalidDecimal, raw)
			}
			paise = d
		} else {
			return 0, fmt.Errorf("%w: more than 2 decimal places in %q", ErrInvalidDecimal, raw)
		}
	}

	total := whole*100 + paise
	if negative {
		total = -total
	}
	return total, nil
}

// FormatPaiseToRupees formats integer paise into a standard decimal rupees string with 2 decimal places (e.g. 550000 -> "5500.00").
func FormatPaiseToRupees(paise int64) string {
	sign := ""
	if paise < 0 {
		sign = "-"
		paise = -paise
	}
	whole := paise / 100
	rem := paise % 100
	return fmt.Sprintf("%s%d.%02d", sign, whole, rem)
}
