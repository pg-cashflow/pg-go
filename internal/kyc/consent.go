package kyc

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
// Falls back to en-IN if the locale or version is not found in the catalog.
func ResolveConsentText(version, locale string) string {
	if version == "" {
		version = "v1.0"
	}
	byLocale, ok := CanonicalConsentCatalog[version]
	if !ok {
		byLocale = CanonicalConsentCatalog["v1.0"]
	}
	if text, ok := byLocale[locale]; ok && text != "" {
		return text
	}
	return CanonicalConsentCatalog["v1.0"]["en-IN"]
}
