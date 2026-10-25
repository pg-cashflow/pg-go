package crypto

import (
	"bytes"
	"testing"
)

func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	key := DeriveKey("super-secret-master-key-32-chars-min")
	plaintext := []byte("9876543210123456")

	ciphertext, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext must not match plaintext")
	}

	decrypted, version, err := Decrypt(key, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if version != CurrentKeyVersion {
		t.Errorf("expected version %d, got %d", CurrentKeyVersion, version)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("decrypted %s does not match plaintext %s", string(decrypted), string(plaintext))
	}
}

func TestEncrypt_RandomNonces(t *testing.T) {
	key := DeriveKey("test-secret-key")
	plaintext := []byte("1234567890")

	c1, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("c1 failed: %v", err)
	}
	c2, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("c2 failed: %v", err)
	}

	if bytes.Equal(c1, c2) {
		t.Fatal("encrypting the same plaintext twice must produce different ciphertexts (random nonces)")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	key := DeriveKey("test-secret-key")
	plaintext := []byte("sensitive-bank-account-1234")

	ciphertext, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	// Flip a bit in the ciphertext payload
	tampered := make([]byte, len(ciphertext))
	copy(tampered, ciphertext)
	tampered[len(tampered)-1] ^= 0x01

	_, _, err = Decrypt(key, tampered)
	if err == nil {
		t.Fatal("expected decryption failure on tampered ciphertext, got nil")
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	key1 := DeriveKey("key-1")
	key2 := DeriveKey("key-2")
	plaintext := []byte("test-data")

	ciphertext, err := Encrypt(key1, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	_, _, err = Decrypt(key2, ciphertext)
	if err == nil {
		t.Fatal("expected decryption failure with wrong key, got nil")
	}
}

func TestDecrypt_TruncatedCiphertext(t *testing.T) {
	key := DeriveKey("test-secret-key")
	short := []byte{1, 2, 3}

	_, _, err := Decrypt(key, short)
	if err != ErrCiphertextTooShort {
		t.Fatalf("expected ErrCiphertextTooShort, got %v", err)
	}
}

func TestDecrypt_InvalidKeyVersion(t *testing.T) {
	key := DeriveKey("test-secret-key")
	ciphertext, err := Encrypt(key, []byte("hello"))
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	ciphertext[0] = 99 // unsupported version
	_, _, err = Decrypt(key, ciphertext)
	if err != ErrInvalidKeyVersion {
		t.Fatalf("expected ErrInvalidKeyVersion, got %v", err)
	}
}

func TestKeyRing_VersionedRotation(t *testing.T) {
	keyV1 := DeriveKey("old-payout-secret-v1")
	keyV2 := DeriveKey("new-payout-secret-v2")
	plaintext := []byte("account-1234567890")

	// 1. Data encrypted under version 1
	c1, err := Encrypt(keyV1, plaintext, 1)
	if err != nil {
		t.Fatalf("Encrypt v1 failed: %v", err)
	}

	// 2. KeyRing with primary version 2, and historical key version 1
	kr, err := NewKeyRing(2, keyV2)
	if err != nil {
		t.Fatalf("NewKeyRing failed: %v", err)
	}
	if err := kr.AddKey(1, keyV1); err != nil {
		t.Fatalf("AddKey v1 failed: %v", err)
	}

	// 3. Encrypt new data with KeyRing (should use primary version 2)
	c2, err := EncryptWithKeyRing(kr, plaintext)
	if err != nil {
		t.Fatalf("EncryptWithKeyRing failed: %v", err)
	}
	if c2[0] != 2 {
		t.Fatalf("expected version 2 on new ciphertext, got %d", c2[0])
	}

	// 4. Decrypt old version 1 ciphertext using KeyRing
	dec1, ver1, err := DecryptWithKeyRing(kr, c1)
	if err != nil {
		t.Fatalf("DecryptWithKeyRing old ciphertext failed: %v", err)
	}
	if ver1 != 1 {
		t.Fatalf("expected version 1, got %d", ver1)
	}
	if !bytes.Equal(dec1, plaintext) {
		t.Fatalf("decrypted %s does not match plaintext %s", string(dec1), string(plaintext))
	}

	// 5. Decrypt new version 2 ciphertext using KeyRing
	dec2, ver2, err := DecryptWithKeyRing(kr, c2)
	if err != nil {
		t.Fatalf("DecryptWithKeyRing new ciphertext failed: %v", err)
	}
	if ver2 != 2 {
		t.Fatalf("expected version 2, got %d", ver2)
	}
	if !bytes.Equal(dec2, plaintext) {
		t.Fatalf("decrypted %s does not match plaintext %s", string(dec2), string(plaintext))
	}

	// 6. If key version 1 is missing from key ring, decrypting c1 must fail
	krOnlyV2, _ := NewKeyRing(2, keyV2)
	_, _, err = DecryptWithKeyRing(krOnlyV2, c1)
	if err != ErrInvalidKeyVersion {
		t.Fatalf("expected ErrInvalidKeyVersion for missing key version, got %v", err)
	}
}

func TestDeriveKeyHKDF(t *testing.T) {
	secret := []byte("high-entropy-master-secret-seed")
	salt := []byte("kyc-payout-salt-v1")
	info := "pg-cashflow/payout-encryption/v1"

	k1, err := DeriveKeyHKDF(secret, salt, info)
	if err != nil {
		t.Fatalf("DeriveKeyHKDF failed: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(k1))
	}

	// Deterministic derivation check
	k2, err := DeriveKeyHKDF(secret, salt, info)
	if err != nil {
		t.Fatalf("second DeriveKeyHKDF failed: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("DeriveKeyHKDF must be deterministic for identical inputs")
	}

	// Domain separation check: different info must yield different key
	kDifferentInfo, err := DeriveKeyHKDF(secret, salt, "pg-cashflow/kyc-document/v1")
	if err != nil {
		t.Fatalf("DeriveKeyHKDF with different info failed: %v", err)
	}
	if bytes.Equal(k1, kDifferentInfo) {
		t.Fatal("different info parameter must produce distinct keys")
	}

	// Empty secret check
	if _, err := DeriveKeyHKDF(nil, salt, info); err == nil {
		t.Fatal("expected error for empty secret")
	}

	// Roundtrip encryption test using HKDF key
	plaintext := []byte("test-aadhaar-123456789012")
	ciphertext, err := Encrypt(k1, plaintext)
	if err != nil {
		t.Fatalf("Encrypt with HKDF key failed: %v", err)
	}
	decrypted, _, err := Decrypt(k1, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt with HKDF key failed: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted %s does not match plaintext %s", decrypted, plaintext)
	}
}

