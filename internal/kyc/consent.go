package kyc

import (
	"errors"
	"fmt"
)

var (
	// ErrUnsupportedConsentVersion indicates an unregistered consent version string.
	ErrUnsupportedConsentVersion = errors.New("kyc: unsupported consent version")
	// ErrUnsupportedConsentPurpose indicates an unregistered consent purpose string.
	ErrUnsupportedConsentPurpose = errors.New("kyc: unsupported consent purpose")
)

// AllowedConsentPurposes is the server-owned allowlist of valid KYC consent purpose values.
var AllowedConsentPurposes = map[string]bool{
	"residency_verification_and_kyc": true,
	"tenant_verification":            true,
	"rental_onboarding":              true,
	"aadhaar_kyc":                    true,
	"onboarding":                     true,
	"onboarding_kyc":                 true,
}

// ValidateConsentPurpose returns an error if the purpose is not in the server allowlist.
func ValidateConsentPurpose(purpose string) error {
	if !AllowedConsentPurposes[purpose] {
		return fmt.Errorf("%w: %q", ErrUnsupportedConsentPurpose, purpose)
	}
	return nil
}

// CanonicalConsentCatalog maps (version -> locale -> canonical consent text).
var CanonicalConsentCatalog = map[string]map[string]string{
	"v1.0": {
		"en-IN": "I agree to my identity details being verified and stored for PG occupancy records in compliance with the DPDP Act.",
		"te-IN": "నేను DPDP చట్టానికి అనుగుణంగా PG ఆక్యుపెన్సీ రికార్డుల కోసం నా గుర్తింపు వివరాలను ధృవీకరించడానికి మరియు నిల్వ చేయడానికి అంగీకరిస్తున్నాను.",
		"ta-IN": "DPDP சட்டத்திற்கு இணங்க, PG தங்குமிடம் பதிவுகளுக்காக எனது அடையாள விவரங்கள் சரிபார்க்கப்பட்டு சேமிக்கப்படுவதை நான் ஒப்புக்கொள்கிறேன்.",
		"kn-IN": "DPDP ಕಾಯ್ದೆಗೆ ಅನುಗುಣವಾಗಿ PG ವಾಸದ ದಾಖಲೆಗಳಿಗಾಗಿ ನನ್ನ ಗುರುತಿನ ವಿವರಗಳನ್ನು ಪರಿಶೀಲಿಸಲು ಮತ್ತು ಸಂಗ್ರಹಿಸಲು ನಾನು ಸಮ್ಮತಿಸುತ್ತೇನೆ.",
	},
}

// ResolveConsentText returns the canonical server-owned text for a given version and locale.
// It returns ErrUnsupportedConsentVersion if the requested version is not in the canonical catalog.
// Falls back to en-IN if the locale is not found for a supported version.
func ResolveConsentText(version, locale string) (string, error) {
	if version == "" || version == "v1" {
		version = "v1.0"
	}
	byLocale, ok := CanonicalConsentCatalog[version]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedConsentVersion, version)
	}
	if text, ok := byLocale[locale]; ok && text != "" {
		return text, nil
	}
	return byLocale["en-IN"], nil
}
