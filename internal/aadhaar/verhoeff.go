package aadhaar

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

var (
	ErrAadhaarLength   = errors.New("aadhaar number must be exactly 12 digits")
	ErrAadhaarInvalid  = errors.New("aadhaar number must contain digits only")
	ErrAadhaarLeading  = errors.New("aadhaar number cannot begin with 0 or 1")
	ErrAadhaarChecksum = errors.New("aadhaar number checksum validation failed")
)

var (
	verhoeffD = [][]int{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		{1, 2, 3, 4, 0, 6, 7, 8, 9, 5},
		{2, 3, 4, 0, 1, 7, 8, 9, 5, 6},
		{3, 4, 0, 1, 2, 8, 9, 5, 6, 7},
		{4, 0, 1, 2, 3, 9, 5, 6, 7, 8},
		{5, 9, 8, 7, 6, 0, 4, 3, 2, 1},
		{6, 5, 9, 8, 7, 1, 0, 4, 3, 2},
		{7, 6, 5, 9, 8, 2, 1, 0, 4, 3},
		{8, 7, 6, 5, 9, 3, 2, 1, 0, 4},
		{9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
	}
	verhoeffP = [][]int{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		{1, 5, 7, 6, 2, 8, 3, 0, 9, 4},
		{5, 8, 0, 3, 7, 9, 6, 1, 4, 2},
		{8, 9, 1, 6, 0, 4, 3, 5, 2, 7},
		{9, 4, 5, 3, 1, 2, 6, 8, 7, 0},
		{4, 2, 8, 6, 5, 7, 3, 9, 0, 1},
		{2, 7, 9, 3, 8, 0, 6, 4, 1, 5},
		{7, 0, 4, 6, 9, 1, 3, 2, 5, 8},
	}
	verhoeffInv = []int{0, 4, 3, 2, 1, 5, 6, 7, 8, 9}
)

// ValidateVerhoeff validates whether a numeric string satisfies the Verhoeff checksum algorithm.
func ValidateVerhoeff(num string) bool {
	if num == "" {
		return false
	}
	c := 0
	for i := 0; i < len(num); i++ {
		ch := num[len(num)-1-i]
		if ch < '0' || ch > '9' {
			return false
		}
		digit := int(ch - '0')
		c = verhoeffD[c][verhoeffP[i%8][digit]]
	}
	return c == 0
}

// GenerateVerhoeffCheckDigit calculates the Verhoeff check digit for a given prefix string.
func GenerateVerhoeffCheckDigit(num string) (int, error) {
	if num == "" {
		return 0, errors.New("empty string")
	}
	c := 0
	for i := 0; i < len(num); i++ {
		ch := num[len(num)-1-i]
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid character %q at position %d", ch, len(num)-1-i)
		}
		digit := int(ch - '0')
		c = verhoeffD[c][verhoeffP[(i+1)%8][digit]]
	}
	return verhoeffInv[c], nil
}

// ValidateAadhaarNumber performs syntactic and checksum validation on a 12-digit Aadhaar number string.
// It checks:
// 1. Length is exactly 12 characters.
// 2. All characters are ASCII digits.
// 3. Leading digit is neither '0' nor '1' (per UIDAI standard).
// 4. Verhoeff checksum algorithm passes.
func ValidateAadhaarNumber(aadhaar string) error {
	cleaned := strings.TrimSpace(aadhaar)
	if len(cleaned) != 12 {
		return ErrAadhaarLength
	}
	for _, ch := range cleaned {
		if !unicode.IsDigit(ch) || ch > '9' {
			return ErrAadhaarInvalid
		}
	}
	if cleaned[0] == '0' || cleaned[0] == '1' {
		return ErrAadhaarLeading
	}
	if !ValidateVerhoeff(cleaned) {
		return ErrAadhaarChecksum
	}
	return nil
}
