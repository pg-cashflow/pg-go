package postgres

import "strings"

func phonesMatch(a, b string) bool {
	return normalizeDigitsPhone(a) == normalizeDigitsPhone(b)
}

func normalizeDigitsPhone(p string) string {
	raw := strings.TrimSpace(p)
	if raw == "" {
		return ""
	}
	hasPlus := strings.HasPrefix(raw, "+")
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	if hasPlus && len(d) >= 10 && len(d) <= 15 {
		return "+" + d
	}
	if len(d) == 10 {
		return "+91" + d
	}
	if len(d) == 12 && strings.HasPrefix(d, "91") {
		return "+" + d
	}
	if len(d) >= 10 && len(d) <= 15 {
		return "+" + d
	}
	return ""
}
