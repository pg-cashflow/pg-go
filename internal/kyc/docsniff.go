package kyc

import (
	"bytes"
	"errors"
	"strings"
)

// ErrUnsupportedDocumentType is returned when uploaded bytes are not a PDF, JPEG or PNG.
// The check uses file signatures (magic bytes), never the client-declared Content-Type.
var ErrUnsupportedDocumentType = errors.New("kyc: document must be a PDF, JPEG or PNG file")

const maxDocumentFilenameLen = 100

var (
	pdfMagic  = []byte("%PDF-")
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
	pngMagic  = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
)

// SniffDocumentMIME returns the allow-listed MIME type that matches the file signature.
// Any other content, including empty input, HTML, SVG, and GIF, returns ErrUnsupportedDocumentType.
func SniffDocumentMIME(b []byte) (string, error) {
	switch {
	case bytes.HasPrefix(b, pdfMagic):
		return "application/pdf", nil
	case bytes.HasPrefix(b, jpegMagic):
		return "image/jpeg", nil
	case bytes.HasPrefix(b, pngMagic):
		return "image/png", nil
	default:
		return "", ErrUnsupportedDocumentType
	}
}

// SanitizeDocumentFilename keeps only the final path element and restricts it to
// [A-Za-z0-9._-]. Leading dots are removed, so the result is never a hidden file.
// An empty result becomes "document".
func SanitizeDocumentFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if len(out) > maxDocumentFilenameLen {
		out = out[:maxDocumentFilenameLen]
	}
	if out == "" {
		return "document"
	}
	return out
}
