package search

import (
	"regexp"
	"strings"
	"unicode"
)

var exactTokenRe = regexp.MustCompile(`^[A-Za-z0-9-]{6,}$`)

// NormalizeQuery trims and collapses internal whitespace.
func NormalizeQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	var b strings.Builder
	lastSpace := false
	for _, r := range q {
		if unicode.IsSpace(r) {
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	return b.String()
}

const (
	MinQueryLength = 2
	MaxQueryLength = 100
)

// ValidQueryLength returns true when q meets length bounds.
func ValidQueryLength(q string) bool {
	if len(q) < MinQueryLength && !exactTokenRe.MatchString(q) {
		return false
	}
	if len(q) > MaxQueryLength {
		return false
	}
	return true
}

// ExtractPhoneDigits extracts only the digit characters from a string.
// If the string contains at least 10 digits, returns the last 10 digits.
func ExtractPhoneDigits(s string) string {
	var digits strings.Builder
	for _, r := range s {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	if len(d) >= 10 {
		return d[len(d)-10:]
	}
	return d
}

// LikePattern escapes ILIKE wildcards for a contains match.
func LikePattern(q string) string {
	q = strings.ReplaceAll(q, "\\", "\\\\")
	q = strings.ReplaceAll(q, "%", "\\%")
	q = strings.ReplaceAll(q, "_", "\\_")
	return "%" + q + "%"
}

// IsExactTokenQuery matches due codes / UTR-style tokens for lexical boosting.
func IsExactTokenQuery(q string) bool {
	return exactTokenRe.MatchString(q)
}
