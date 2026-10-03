package search

import (
	"strings"
	"unicode"
)

// TokenKind represents the classified type of a query term.
type TokenKind string

const (
	TokenPhone       TokenKind = "phone"
	TokenShortNumber TokenKind = "short_num"
	TokenCode        TokenKind = "code"
	TokenText        TokenKind = "text"
)

// Token is a classified segment of a user query.
type Token struct {
	Raw         string
	Kind        TokenKind
	Value       string // normalized representation
	PhoneDigits string // extracted 10-digit phone if applicable
}

// TokenizeQuery splits a query into normalized tokens and classifies each term.
func TokenizeQuery(q string) []Token {
	q = NormalizeQuery(q)
	if q == "" {
		return nil
	}
	parts := strings.Fields(q)
	tokens := make([]Token, 0, len(parts))

	for _, p := range parts {
		t := classifyToken(p)
		tokens = append(tokens, t)
	}
	return tokens
}

func classifyToken(raw string) Token {
	digits := ExtractPhoneDigits(raw)
	// 1. Phone number (10+ digits)
	if len(digits) == 10 {
		return Token{
			Raw:         raw,
			Kind:        TokenPhone,
			Value:       digits,
			PhoneDigits: digits,
		}
	}

	// 2. Short numeric: 1-5 digits (room number or small amount)
	if isAllDigits(raw) && len(raw) <= 5 {
		return Token{
			Raw:   raw,
			Kind:  TokenShortNumber,
			Value: raw,
		}
	}

	// 3. Code-like identifier: contains uppercase/digits/hyphens (e.g. DUE-101, UTR9988)
	if isCodeLike(raw) {
		return Token{
			Raw:   raw,
			Kind:  TokenCode,
			Value: strings.ToUpper(raw),
		}
	}

	// 4. Free text: names, descriptions, notes
	return Token{
		Raw:   raw,
		Kind:  TokenText,
		Value: strings.ToLower(raw),
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func isCodeLike(s string) bool {
	if len(s) < 3 {
		return false
	}
	hasLetter := false
	hasDigit := false
	hasHyphenOrUnderscore := false

	for _, r := range s {
		if unicode.IsLetter(r) {
			hasLetter = true
		} else if unicode.IsDigit(r) {
			hasDigit = true
		} else if r == '-' || r == '_' {
			hasHyphenOrUnderscore = true
		} else {
			return false
		}
	}

	// Alphanumeric mixture or identifier with hyphen
	return (hasLetter && hasDigit) || (hasHyphenOrUnderscore && (hasLetter || hasDigit))
}
