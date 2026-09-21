package localization

import "golang.org/x/text/language"

// LocaleMetadata describes a supported application locale.
type LocaleMetadata struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	NativeName string `json:"native_name"`
}

const DefaultLocale = "en-IN"

// SupportedLocales contains all locales supported in Phase 1.
var SupportedLocales = []LocaleMetadata{
	{Code: "en-IN", Name: "English", NativeName: "English"},
	{Code: "te-IN", Name: "Telugu", NativeName: "తెలుగు"},
	{Code: "ta-IN", Name: "Tamil", NativeName: "தமிழ்"},
	{Code: "kn-IN", Name: "Kannada", NativeName: "ಕನ್ನಡ"},
}

var supportedTags = []language.Tag{
	language.MustParse("en-IN"),
	language.MustParse("te-IN"),
	language.MustParse("ta-IN"),
	language.MustParse("kn-IN"),
}

var tagMatcher = language.NewMatcher(supportedTags)

// IsValid checks if the provided locale string is in the supported catalog.
func IsValid(code string) bool {
	for _, l := range SupportedLocales {
		if l.Code == code {
			return true
		}
	}
	return false
}

// CanonicalTag returns the BCP-47 language.Tag for a supported code, or the default tag.
func CanonicalTag(code string) language.Tag {
	tag, err := language.Parse(code)
	if err != nil {
		return supportedTags[0]
	}
	matched, _, _ := tagMatcher.Match(tag)
	return matched
}
