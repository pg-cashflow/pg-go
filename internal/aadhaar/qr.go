package aadhaar

import (
	"encoding/xml"
	"regexp"
	"strings"
)

// AadhaarData holds fields extracted from a UIDAI QR payload.
// Only last-4 of UID is retained for storage; full UID is never persisted by callers.
type AadhaarData struct {
	Name       string `json:"name,omitempty"`
	DOB        string `json:"dob,omitempty"`
	Gender     string `json:"gender,omitempty"`
	UIDLast4   string `json:"uid_last4,omitempty"`
	YOB        string `json:"yob,omitempty"`
}

var (
	attrName   = regexp.MustCompile(`(?i)\bname\s*=\s*"([^"]*)"`)
	attrDOB    = regexp.MustCompile(`(?i)\bdob\s*=\s*"([^"]*)"`)
	attrYOB    = regexp.MustCompile(`(?i)\byob\s*=\s*"([^"]*)"`)
	attrGender = regexp.MustCompile(`(?i)\bgender\s*=\s*"([^"]*)"`)
	attrUID    = regexp.MustCompile(`(?i)\buid\s*=\s*"([^"]*)"`)
	digitsRe   = regexp.MustCompile(`\d{4,12}`)
)

// DecodeAadhaarQR tolerates partial XML and opaque secure-QR payloads.
// Returns partial=true when some but not all fields could be recovered, or when
// the payload is unreadable (blank data, partial=true, err=nil).
func DecodeAadhaarQR(raw string) (AadhaarData, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return AadhaarData{}, true, nil
	}

	var data AadhaarData
	looksLikeAttrs := attrName.MatchString(raw) || attrUID.MatchString(raw) || attrGender.MatchString(raw)
	if strings.Contains(strings.ToLower(raw), "printletterbarcodedata") || strings.Contains(raw, "<") || looksLikeAttrs {
		data = decodeXMLLike(raw)
	} else {
		// Secure QR / binary / opaque — try light heuristics only.
		data = decodeOpaque(raw)
	}

	complete := data.Name != "" && (data.DOB != "" || data.YOB != "") && data.Gender != "" && data.UIDLast4 != ""
	if complete {
		return data, false, nil
	}
	// Unreadable or incomplete → blank-or-partial with partial=true, no error.
	return data, true, nil
}

type printLetterBarcodeData struct {
	XMLName xml.Name `xml:"PrintLetterBarcodeData"`
	UID     string   `xml:"uid,attr"`
	Name    string   `xml:"name,attr"`
	Gender  string   `xml:"gender,attr"`
	DOB     string   `xml:"dob,attr"`
	YOB     string   `xml:"yob,attr"`
}

func decodeXMLLike(raw string) AadhaarData {
	var plbd printLetterBarcodeData
	// Try wrapping if fragment lacks root.
	candidates := []string{raw}
	if !strings.HasPrefix(strings.TrimSpace(raw), "<") {
		candidates = append(candidates, "<PrintLetterBarcodeData "+raw+" />")
	}
	for _, c := range candidates {
		if err := xml.Unmarshal([]byte(c), &plbd); err == nil && (plbd.Name != "" || plbd.UID != "") {
			return fromPLBD(plbd)
		}
	}
	// Regex fallback for truncated / malformed XML.
	return AadhaarData{
		Name:     firstSub(attrName, raw),
		DOB:      firstSub(attrDOB, raw),
		YOB:      firstSub(attrYOB, raw),
		Gender:   normalizeGender(firstSub(attrGender, raw)),
		UIDLast4: last4(firstSub(attrUID, raw)),
	}
}

func fromPLBD(p printLetterBarcodeData) AadhaarData {
	return AadhaarData{
		Name:     strings.TrimSpace(p.Name),
		DOB:      strings.TrimSpace(p.DOB),
		YOB:      strings.TrimSpace(p.YOB),
		Gender:   normalizeGender(p.Gender),
		UIDLast4: last4(p.UID),
	}
}

func decodeOpaque(raw string) AadhaarData {
	// Secure QR is binary; without UIDAI SDK we cannot decode. Return blank partial.
	// If the string looks like a 12-digit UID pasted by mistake, keep last4 only.
	var data AadhaarData
	if m := digitsRe.FindString(raw); len(m) >= 4 {
		data.UIDLast4 = m[len(m)-4:]
	}
	return data
}

func firstSub(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func last4(uid string) string {
	uid = digitsRe.FindString(uid)
	if len(uid) < 4 {
		return ""
	}
	return uid[len(uid)-4:]
}

func normalizeGender(g string) string {
	g = strings.TrimSpace(strings.ToUpper(g))
	switch g {
	case "M", "MALE":
		return "M"
	case "F", "FEMALE":
		return "F"
	case "T", "O", "OTHER", "TRANSGENDER":
		return "T"
	default:
		return g
	}
}
