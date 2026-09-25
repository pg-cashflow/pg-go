package cashfree

import (
	"testing"
)

func TestParseRupeesToPaise(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"5500", 550000, false},
		{"5500.0", 550000, false},
		{"5500.00", 550000, false},
		{"5500.5", 550050, false},
		{"5500.50", 550050, false},
		{"5500.05", 550005, false},
		{"0.50", 50, false},
		{"0.05", 5, false},
		{"0", 0, false},
		{"0.00", 0, false},
		{" 1234.56 ", 123456, false},
		{"-50.25", -5025, false},
		// Invalid cases
		{"", 0, true},
		{"abc", 0, true},
		{"12.345", 0, true}, // More than 2 decimal places
		{"12.3.4", 0, true},
	}

	for _, tt := range tests {
		got, err := ParseRupeesToPaise(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRupeesToPaise(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseRupeesToPaise(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestFormatPaiseToRupees(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{550000, "5500.00"},
		{550050, "5500.50"},
		{550005, "5500.05"},
		{50, "0.50"},
		{5, "0.05"},
		{0, "0.00"},
		{-5025, "-50.25"},
	}

	for _, tt := range tests {
		got := FormatPaiseToRupees(tt.input)
		if got != tt.want {
			t.Errorf("FormatPaiseToRupees(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
