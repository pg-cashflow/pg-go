package aadhaar

import (
	"errors"
	"strings"
)

var ErrLast4AlreadySet = errors.New("aadhaar: last-4 already set")

// PersistableLast4 returns the last-4 that may be stored.
// QR payloads persist only when the Secure QR signature verified.
// Manual last-4 is allowed only when no QR was submitted.
func PersistableLast4(fromQR, verified bool, qrLast4, manualLast4 string) string {
	if fromQR {
		if verified {
			return strings.TrimSpace(qrLast4)
		}
		return ""
	}
	return strings.TrimSpace(manualLast4)
}

// GuardOverwrite rejects changing an already-stored last-4 to a different value.
func GuardOverwrite(existing *string, next string) error {
	if next == "" {
		return nil
	}
	if existing != nil && strings.TrimSpace(*existing) != "" && *existing != next {
		return ErrLast4AlreadySet
	}
	return nil
}
