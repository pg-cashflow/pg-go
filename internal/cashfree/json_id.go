package cashfree

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ProviderID decodes a Cashfree provider identifier that may be delivered as
// either a JSON string ("refund_40564529") or a bare JSON number (12345).
//
// Cashfree's published refund examples use alphanumeric IDs such as
// "refund_40564529"; payment IDs (cf_payment_id) are numeric in current
// documentation but the type is kept consistent here for forward compatibility.
// Using json.Number for these fields causes a JSON decode error when
// Cashfree sends a string value, so this custom type accepts both forms.
type ProviderID string

// UnmarshalJSON accepts:
//
//	"refund_40564529"   – JSON string (Cashfree refund IDs)
//	12345               – bare JSON number (Cashfree payment IDs, legacy)
//	null                – treated as empty string
func (p *ProviderID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*p = ""
		return nil
	}
	// String token: strip the surrounding quotes.
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("cashfree ProviderID: %w", err)
		}
		*p = ProviderID(s)
		return nil
	}
	// Bare number token: accept as a string representation.
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("cashfree ProviderID: unexpected token %q", b)
	}
	*p = ProviderID(n.String())
	return nil
}

// MarshalJSON always emits a JSON string, regardless of whether the original
// value was a number.
func (p ProviderID) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(p))
}

// String returns the underlying string value, trimmed of whitespace.
func (p ProviderID) String() string {
	return strings.TrimSpace(string(p))
}
