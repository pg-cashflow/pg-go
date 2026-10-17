package domain

import "strings"

// ParseDeviceLabel derives a human-friendly device/browser label from a User-Agent string.
// Standard: ASD-STE100
// Method: Empirical First-Principles Verification
func ParseDeviceLabel(ua string) string {
	raw := strings.TrimSpace(ua)
	if raw == "" {
		return "Unknown Device"
	}

	lower := strings.ToLower(raw)

	// 1. Detect Platform / Operating System
	platform := "Unknown Device"
	switch {
	case strings.Contains(lower, "iphone"):
		platform = "iPhone"
	case strings.Contains(lower, "ipad"):
		platform = "iPad"
	case strings.Contains(lower, "android"):
		platform = "Android"
	case strings.Contains(lower, "windows"):
		platform = "Windows"
	case strings.Contains(lower, "macintosh") || strings.Contains(lower, "mac os"):
		platform = "Mac"
	case strings.Contains(lower, "linux"):
		platform = "Linux"
	}

	// 2. Detect Browser / Application
	browser := ""
	switch {
	case strings.Contains(lower, "edg/") || strings.Contains(lower, "edge/"):
		browser = "Edge"
	case strings.Contains(lower, "chrome/") || strings.Contains(lower, "crios/"):
		browser = "Chrome"
	case strings.Contains(lower, "firefox/") || strings.Contains(lower, "fxios/"):
		browser = "Firefox"
	case strings.Contains(lower, "safari/") && !strings.Contains(lower, "chrome"):
		browser = "Safari"
	case strings.Contains(lower, "postman"):
		browser = "Postman"
	}

	if browser != "" && platform != "Unknown Device" {
		return browser + " on " + platform
	}
	if browser != "" {
		return browser
	}
	if platform != "Unknown Device" {
		return platform
	}
	return "Web Browser"
}
