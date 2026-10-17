package auth

import "testing"

func TestParseDeviceLabel(t *testing.T) {
	tests := []struct {
		name     string
		ua       string
		expected string
	}{
		{
			name:     "empty string",
			ua:       "",
			expected: "Unknown Device",
		},
		{
			name:     "whitespace string",
			ua:       "   ",
			expected: "Unknown Device",
		},
		{
			name:     "iOS Safari on iPhone",
			ua:       "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1",
			expected: "Safari on iPhone",
		},
		{
			name:     "Chrome on Android",
			ua:       "Mozilla/5.0 (Linux; Android 14; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.6261.119 Mobile Safari/537.36",
			expected: "Chrome on Android",
		},
		{
			name:     "Chrome on Windows",
			ua:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36",
			expected: "Chrome on Windows",
		},
		{
			name:     "Edge on Windows",
			ua:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36 Edg/122.0.2365.92",
			expected: "Edge on Windows",
		},
		{
			name:     "Firefox on macOS",
			ua:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:123.0) Gecko/20100101 Firefox/123.0",
			expected: "Firefox on Mac",
		},
		{
			name:     "Postman Desktop",
			ua:       "PostmanRuntime/7.36.3",
			expected: "Postman",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseDeviceLabel(tc.ua)
			if got != tc.expected {
				t.Errorf("ParseDeviceLabel(%q) = %q, expected %q", tc.ua, got, tc.expected)
			}
		})
	}
}
