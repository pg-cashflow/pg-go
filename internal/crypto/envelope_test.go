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
