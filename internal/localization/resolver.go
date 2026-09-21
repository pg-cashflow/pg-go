package localization

import (
	"net/http"
	"strings"

	"golang.org/x/text/language"
)

// ResolveLocale determines the effective locale for a request based on:
// 1. Explicit user preference (if provided and valid)
// 2. Request Accept-Language header
// 3. Application default (en-IN)
func ResolveLocale(userPreference *string, r *http.Request) string {
	if userPreference != nil {
		pref := strings.TrimSpace(*userPreference)
		if IsValid(pref) {
			return pref
		}
	}

	if r != nil {
		accept := r.Header.Get("Accept-Language")
		// Guard against oversized header values (CVE-2022-32149 defense-in-depth).
		// Overly long headers are ignored entirely and fall back to default.
		if len(accept) > 256 {
			return DefaultLocale
		}
		if strings.TrimSpace(accept) != "" {
			tags, _, err := language.ParseAcceptLanguage(accept)
			if err == nil && len(tags) > 0 {
				matchedTag, _, confidence := tagMatcher.Match(tags...)
				// Only accept if confidence is High or Low (not No match)
				if confidence != language.No {
					matchedStr := matchedTag.String()
					// Check if matched tag directly corresponds to one of our supported codes
					for _, s := range SupportedLocales {
						if s.Code == matchedStr {
							return s.Code
						}
					}
					// Also check language base tag (e.g. "te" -> "te-IN")
					base, _ := matchedTag.Base()
					for _, s := range SupportedLocales {
						sTag, _ := language.Parse(s.Code)
						sBase, _ := sTag.Base()
						if sBase == base {
							return s.Code
						}
					}
				}
			}
		}
	}

	return DefaultLocale
}
