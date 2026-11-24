package crypto

import (
	"errors"
	"fmt"
	"strings"
)

// MinApprovalSigningKeyLength defines the minimum byte length for HMAC-SHA256 payout approval keys.
const MinApprovalSigningKeyLength = 32

var (
	ErrEmptyActiveKID      = errors.New("approval keyring: active_kid cannot be empty")
	ErrEmptyKeys           = errors.New("approval keyring: keys map cannot be empty")
	ErrActiveKeyNotFound   = errors.New("approval keyring: active key not found in keys map")
	ErrApprovalKeyTooShort = errors.New("approval keyring: key must be at least 32 bytes")
)

// ApprovalKeyRing manages versioned HMAC signing and verification keys for payout approval snapshots.
// It guarantees consistent key resolution across API approval handlers and background dispatch workers.
type ApprovalKeyRing struct {
	ActiveKID string
	Keys      map[string][]byte
}

// NewApprovalKeyRing constructs and validates an ApprovalKeyRing.
// It enforces that:
// 1. ActiveKID is non-empty.
// 2. Keys contains ActiveKID.
// 3. Every key in Keys has at least MinApprovalSigningKeyLength bytes.
func NewApprovalKeyRing(activeKID string, keys map[string][]byte) (*ApprovalKeyRing, error) {
	activeKID = strings.TrimSpace(activeKID)
	if activeKID == "" {
		return nil, ErrEmptyActiveKID
	}
	if len(keys) == 0 {
		return nil, ErrEmptyKeys
	}

	activeKey, ok := keys[activeKID]
	if !ok {
		return nil, fmt.Errorf("%w: active_kid %q", ErrActiveKeyNotFound, activeKID)
	}
	if len(activeKey) < MinApprovalSigningKeyLength {
		return nil, fmt.Errorf("%w for active_kid %q (got %d bytes, need >= %d)",
			ErrApprovalKeyTooShort, activeKID, len(activeKey), MinApprovalSigningKeyLength)
	}

	validatedKeys := make(map[string][]byte, len(keys))
	for kid, key := range keys {
		trimmedKID := strings.TrimSpace(kid)
		if trimmedKID == "" {
			return nil, errors.New("approval keyring: key ID cannot be empty")
		}
		if len(key) < MinApprovalSigningKeyLength {
			return nil, fmt.Errorf("%w for key_id %q (got %d bytes, need >= %d)",
				ErrApprovalKeyTooShort, trimmedKID, len(key), MinApprovalSigningKeyLength)
		}
		validatedKeys[trimmedKID] = key
	}

	return &ApprovalKeyRing{
		ActiveKID: activeKID,
		Keys:      validatedKeys,
	}, nil
}

// ActiveKey returns the designated active HMAC signing key and its key ID.
func (r *ApprovalKeyRing) ActiveKey() ([]byte, string, error) {
	if r == nil {
		return nil, "", errors.New("approval keyring is nil")
	}
	key, ok := r.Keys[r.ActiveKID]
	if !ok || len(key) < MinApprovalSigningKeyLength {
		return nil, "", fmt.Errorf("%w: %q", ErrActiveKeyNotFound, r.ActiveKID)
	}
	return key, r.ActiveKID, nil
}

// GetKey returns the HMAC verification key for a specified key ID.
func (r *ApprovalKeyRing) GetKey(kid string) ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	key, ok := r.Keys[kid]
	return key, ok
}

// VerificationKeys returns a copy of all configured verification keys.
func (r *ApprovalKeyRing) VerificationKeys() map[string][]byte {
	if r == nil {
		return nil
	}
	out := make(map[string][]byte, len(r.Keys))
	for k, v := range r.Keys {
		out[k] = v
	}
	return out
}
