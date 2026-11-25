package crypto_test

import (
	"strings"
	"testing"

	"github.com/pg-cashflow/pg-go/internal/crypto"
)

func TestApprovalKeyRing_Validation(t *testing.T) {
	validKey32A := []byte(strings.Repeat("a", 32))
	validKey32B := []byte(strings.Repeat("b", 32))
	shortKey := []byte("too-short-key")

	tests := []struct {
		name      string
		activeKID string
		keys      map[string][]byte
		wantErr   bool
	}{
		{
			name:      "valid single key",
			activeKID: "v1",
			keys: map[string][]byte{
				"v1": validKey32A,
			},
			wantErr: false,
		},
		{
			name:      "valid multiple keys with active v2",
			activeKID: "v2",
			keys: map[string][]byte{
				"v1": validKey32A,
				"v2": validKey32B,
			},
			wantErr: false,
		},
		{
			name:      "empty active kid",
			activeKID: "",
			keys: map[string][]byte{
				"v1": validKey32A,
			},
			wantErr: true,
		},
		{
			name:      "empty keys map",
			activeKID: "v1",
			keys:      map[string][]byte{},
			wantErr:   true,
		},
		{
			name:      "active kid not in map",
			activeKID: "v3",
			keys: map[string][]byte{
				"v1": validKey32A,
				"v2": validKey32B,
			},
			wantErr: true,
		},
		{
			name:      "active key too short",
			activeKID: "v1",
			keys: map[string][]byte{
				"v1": shortKey,
			},
			wantErr: true,
		},
		{
			name:      "secondary key too short",
			activeKID: "v2",
			keys: map[string][]byte{
				"v1": shortKey,
				"v2": validKey32B,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ring, err := crypto.NewApprovalKeyRing(tt.activeKID, tt.keys)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewApprovalKeyRing() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				key, kid, err := ring.ActiveKey()
				if err != nil {
					t.Fatalf("ActiveKey() error = %v", err)
				}
				if kid != tt.activeKID {
					t.Errorf("ActiveKey() kid = %q, want %q", kid, tt.activeKID)
				}
				if len(key) < crypto.MinApprovalSigningKeyLength {
					t.Errorf("ActiveKey() key len = %d, want >= 32", len(key))
				}
			}
		})
	}
}

func TestApprovalKeyRing_RotationSupport(t *testing.T) {
	keyV1 := []byte(strings.Repeat("1", 32))
	keyV2 := []byte(strings.Repeat("2", 32))

	ring, err := crypto.NewApprovalKeyRing("v2", map[string][]byte{
		"v1": keyV1,
		"v2": keyV2,
	})
	if err != nil {
		t.Fatalf("failed to create keyring: %v", err)
	}

	// Active key must be v2
	activeKey, activeKID, err := ring.ActiveKey()
	if err != nil {
		t.Fatalf("ActiveKey error: %v", err)
	}
	if activeKID != "v2" {
		t.Errorf("expected activeKID v2, got %s", activeKID)
	}
	if string(activeKey) != string(keyV2) {
		t.Errorf("expected activeKey v2 bytes, got %s", string(activeKey))
	}

	// Old key v1 must still be retrievable for verification
	v1Key, ok := ring.GetKey("v1")
	if !ok {
		t.Errorf("expected v1 key to be retrievable for verification")
	}
	if string(v1Key) != string(keyV1) {
		t.Errorf("expected v1 key content, got %s", string(v1Key))
	}

	// Unknown key must return false
	_, ok = ring.GetKey("unknown")
	if ok {
		t.Errorf("expected unknown key to return false")
	}

	// VerificationKeys map must contain both
	allKeys := ring.VerificationKeys()
	if len(allKeys) != 2 {
		t.Errorf("expected 2 verification keys, got %d", len(allKeys))
	}
}

func TestApprovalKeyRing_Immutability(t *testing.T) {
	origV1 := []byte(strings.Repeat("a", 32))
	inputMap := map[string][]byte{"v1": origV1}

	ring, err := crypto.NewApprovalKeyRing("v1", inputMap)
	if err != nil {
		t.Fatalf("NewApprovalKeyRing failed: %v", err)
	}

	// Mutate caller's input slice and map
	origV1[0] = 'X'
	inputMap["v2"] = []byte(strings.Repeat("b", 32))

	active, _, err := ring.ActiveKey()
	if err != nil {
		t.Fatalf("ActiveKey failed: %v", err)
	}
	if active[0] != 'a' {
		t.Fatalf("ring internal key was mutated by input slice modification")
	}
	if _, ok := ring.GetKey("v2"); ok {
		t.Fatalf("ring internal map was mutated by input map modification")
	}

	// Mutate slice returned by ActiveKey, GetKey, and VerificationKeys
	active[0] = 'Y'
	gotV1, _ := ring.GetKey("v1")
	gotV1[1] = 'Z'
	vKeys := ring.VerificationKeys()
	vKeys["v1"][2] = 'W'
	vKeys["injected"] = []byte(strings.Repeat("c", 32))

	freshActive, _, _ := ring.ActiveKey()
	if freshActive[0] != 'a' || freshActive[1] != 'a' || freshActive[2] != 'a' {
		t.Fatalf("ring internal key was mutated via returned slices: %q", string(freshActive))
	}
	if _, ok := ring.GetKey("injected"); ok {
		t.Fatalf("ring internal map was mutated via VerificationKeys map")
	}
}
