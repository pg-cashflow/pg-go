package magiclink

import (
	"net/url"
	"strings"
	"unicode"
)

// BuildWALink returns a wa.me deep-link, or "" if phone is nil/empty.
func BuildWALink(phone *string, msg string) string {
	if phone == nil {
		return ""
	}
	digits := digitsOnly(*phone)
	if digits == "" {
		return ""
	}
	u := url.URL{
		Scheme:   "https",
		Host:     "wa.me",
		Path:     "/" + digits,
		RawQuery: "text=" + url.QueryEscape(msg),
	}
	return u.String()
}

func digitsOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
