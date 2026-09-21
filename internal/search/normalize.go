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

// ValidQueryLength returns true when q meets minimum length rules.
func ValidQueryLength(q string) bool {
	if len(q) < 2 && !exactTokenRe.MatchString(q) {
		return false
	}
	return true
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
