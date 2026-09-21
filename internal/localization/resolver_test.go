package localization

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveLocale(t *testing.T) {
	tests := []struct {
		name       string
		userPref   *string
		acceptLang string
		expected   string
	}{
		{
			name:       "user preference takes precedence",
			userPref:   strPtr("te-IN"),
			acceptLang: "ta-IN,en;q=0.8",
			expected:   "te-IN",
		},
		{
			name:       "invalid user preference falls back to header",
			userPref:   strPtr("invalid-locale"),
			acceptLang: "ta-IN",
			expected:   "ta-IN",
		},
		{
			name:       "accept header with dialect matching",
			userPref:   nil,
			acceptLang: "kn-IN,kn;q=0.9",
			expected:   "kn-IN",
		},
		{
			name:       "accept header with general language code",
			userPref:   nil,
			acceptLang: "te",
			expected:   "te-IN",
		},
		{
			name:       "unsupported language in header falls back to default",
			userPref:   nil,
			acceptLang: "fr-FR,de;q=0.8",
			expected:   "en-IN",
		},
		{
			name:       "hi-IN unsupported Indian language falls back to default en-IN",
			userPref:   nil,
			acceptLang: "hi-IN,hi;q=0.9",
			expected:   "en-IN",
		},
		{
			name:       "zh non-Indic language falls back to default en-IN",
			userPref:   nil,
			acceptLang: "zh-CN,zh;q=0.9,en;q=0.8",
			expected:   "en-IN",
		},
		{
			name:       "oversized header > 256 bytes is ignored (CVE-2022-32149 guard)",
			userPref:   nil,
			acceptLang: "te-IN," + strings.Repeat("a", 260),
			expected:   "en-IN",
		},
		{
			name:       "valid header within 256 bytes boundary is accepted",
			userPref:   nil,
			acceptLang: "te-IN",
			expected:   "te-IN",
		},
		{
			name:       "no preference and no header falls back to default",
			userPref:   nil,
			acceptLang: "",
			expected:   "en-IN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tt.acceptLang != "" {
				req.Header.Set("Accept-Language", tt.acceptLang)
			}
			result := ResolveLocale(tt.userPref, req)
			if result != tt.expected {
				t.Errorf("ResolveLocale() = %s, want %s", result, tt.expected)
			}
		})
	}
}

func strPtr(s string) *string {
	return &s
}
