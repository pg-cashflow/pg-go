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
// It guarantees consistent, immutable key resolution across API approval handlers and background dispatch workers.
type ApprovalKeyRing struct {
	activeKID string
	keys      map[string][]byte
}

// NewApprovalKeyRing constructs and validates an immutable ApprovalKeyRing.
// It enforces that:
// 1. activeKID is non-empty.
// 2. keys contains activeKID.
// 3. Every key in keys has at least MinApprovalSigningKeyLength bytes.
// All input slices are deep-copied to prevent external mutation.
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
		cp := make([]byte, len(key))
		copy(cp, key)
		validatedKeys[trimmedKID] = cp
	}

	return &ApprovalKeyRing{
		activeKID: activeKID,
		keys:      validatedKeys,
	}, nil
}

// ActiveKID returns the active key ID.
func (r *ApprovalKeyRing) ActiveKID() string {
	if r == nil {
		return ""
	}
	return r.activeKID
}

// ActiveKey returns a copy of the designated active HMAC signing key and its key ID.
func (r *ApprovalKeyRing) ActiveKey() ([]byte, string, error) {
	if r == nil {
		return nil, "", errors.New("approval keyring is nil")
	}
	key, ok := r.keys[r.activeKID]
	if !ok || len(key) < MinApprovalSigningKeyLength {
		return nil, "", fmt.Errorf("%w: %q", ErrActiveKeyNotFound, r.activeKID)
	}
	cp := make([]byte, len(key))
	copy(cp, key)
	return cp, r.activeKID, nil
}

// GetKey returns a copy of the HMAC verification key for a specified key ID.
func (r *ApprovalKeyRing) GetKey(kid string) ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	key, ok := r.keys[kid]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(key))
	copy(cp, key)
	return cp, true
}

// VerificationKeys returns a deep copy of all configured verification keys.
func (r *ApprovalKeyRing) VerificationKeys() map[string][]byte {
	if r == nil {
		return nil
	}
	out := make(map[string][]byte, len(r.keys))
	for k, v := range r.keys {
		cp := make([]byte, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}
