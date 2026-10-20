package kyc

import (
	"errors"
	"strings"
	"testing"
)

func TestSniffDocumentMIME(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		want    string
		wantErr bool
	}{
		{"pdf", []byte("%PDF-1.7 rest"), "application/pdf", false},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00}, "image/jpeg", false},
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00}, "image/png", false},
		{"empty", []byte{}, "", true},
		{"nil", nil, "", true},
		{"html disguised as pdf label", []byte("<html><script>alert(1)</script>"), "", true},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), "", true},
		{"gif", []byte("GIF89a"), "", true},
		{"pdf magic not at start", []byte("xx%PDF-1.4"), "", true},
		{"truncated png signature", []byte{0x89, 'P', 'N', 'G'}, "", true},
		{"jpeg prefix only 2 bytes", []byte{0xFF, 0xD8}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SniffDocumentMIME(tt.input)
			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedDocumentType) {
					t.Fatalf("want ErrUnsupportedDocumentType, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("want %q, got %q", tt.want, got)
			}
		})
	}
}

func TestSanitizeDocumentFilename(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "aadhaar.pdf", "aadhaar.pdf"},
		{"unix path", "/etc/passwd", "passwd"},
		{"windows path", `C:\Users\x\card.jpg`, "card.jpg"},
		{"traversal", "../../secret.pdf", "secret.pdf"},
		{"hidden file", ".htaccess", "htaccess"},
		{"quotes and header injection", "a\"b\r\nX-Evil: 1.pdf", "a_b__X-Evil__1.pdf"},
		{"unicode", "आधार कार्ड.pdf", "__________.pdf"},
		{"empty", "", "document"},
		{"only dots", "...", "document"},
		{"only separator", "/", "document"},
		{"control chars dropped via underscore", "a\x00b.png", "a_b.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeDocumentFilename(tt.input); got != tt.want {
				t.Fatalf("want %q, got %q", tt.want, got)
			}
		})
	}

	long := strings.Repeat("a", 500) + ".pdf"
	if got := SanitizeDocumentFilename(long); len(got) != maxDocumentFilenameLen {
		t.Fatalf("long name not capped: len=%d", len(got))
	}
}
