package postgres

import (
	"bytes"
	"testing"

	"github.com/pg-cashflow/pg-go/internal/crypto"
)

func TestKYCRepo_Crypto_SingleKeyAndFallback(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	repo := NewKYCRepo(nil, key)

	original := []byte("secret Aadhaar document payload")
	enc, err := repo.encryptBytes(original)
	if err != nil {
		t.Fatalf("encryptBytes failed: %v", err)
	}

	if bytes.Equal(enc, original) {
		t.Fatalf("expected encrypted bytes to differ from plaintext")
	}

	dec, err := repo.decryptBytes(enc)
	if err != nil {
		t.Fatalf("decryptBytes failed: %v", err)
	}

	if !bytes.Equal(dec, original) {
		t.Fatalf("decrypted bytes mismatch: got %s, want %s", string(dec), string(original))
	}
}

func TestKYCRepo_Crypto_KeyRingRotation(t *testing.T) {
	keyV1 := bytes.Repeat([]byte{0x01}, 32)
	keyV2 := bytes.Repeat([]byte{0x02}, 32)

	// 1. Initial state: Repo with Key v1
	repoV1 := NewKYCRepo(nil, keyV1)
	plaintext := []byte("sensitive tenant identity document")

	c1, err := repoV1.encryptBytes(plaintext)
	if err != nil {
		t.Fatalf("repoV1 encryptBytes failed: %v", err)
	}
	if len(c1) == 0 || c1[0] != 1 {
		t.Fatalf("expected version 1 header in ciphertext, got: %v", c1[0])
	}

	// 2. Rotate to KeyRing with primary version 2 and historical version 1
	kr, err := crypto.NewKeyRing(2, keyV2)
	if err != nil {
		t.Fatalf("NewKeyRing failed: %v", err)
	}
	if err := kr.AddKey(1, keyV1); err != nil {
		t.Fatalf("AddKey failed: %v", err)
	}

	rotatedRepo := NewKYCRepo(nil)
	rotatedRepo.SetKeyRing(kr)

	// 3. Decrypt historical v1 ciphertext with rotated keyring
	dec1, err := rotatedRepo.decryptBytes(c1)
	if err != nil {
		t.Fatalf("decrypt historical v1 ciphertext with keyring failed: %v", err)
	}
	if !bytes.Equal(dec1, plaintext) {
		t.Fatalf("decrypted v1 mismatch: got %s, want %s", string(dec1), string(plaintext))
	}

	// 4. Encrypt new data with rotated keyring (should use primary version 2)
	c2, err := rotatedRepo.encryptBytes(plaintext)
	if err != nil {
		t.Fatalf("encrypt with rotated keyring failed: %v", err)
	}
	if len(c2) == 0 || c2[0] != 2 {
		t.Fatalf("expected version 2 header in ciphertext, got: %v", c2[0])
	}

	// 5. Decrypt v2 ciphertext
	dec2, err := rotatedRepo.decryptBytes(c2)
	if err != nil {
		t.Fatalf("decrypt v2 ciphertext failed: %v", err)
	}
	if !bytes.Equal(dec2, plaintext) {
		t.Fatalf("decrypted v2 mismatch: got %s, want %s", string(dec2), string(plaintext))
	}

	// 6. Verify single-key repo without v1 fails to decrypt v1 ciphertext
	repoV2Only := NewKYCRepo(nil, keyV2)
	// crypto.Decrypt checks CurrentKeyVersion (1) which matches c1 version byte 1, but wrong key -> AEAD failure
	_, err = repoV2Only.decryptBytes(c1)
	if err == nil {
		t.Fatalf("expected decryption failure when decrypting v1 ciphertext with keyV2 only")
	}
}

func TestKYCRepo_Crypto_UnencryptedDevMode(t *testing.T) {
	repo := NewKYCRepo(nil) // No key

	data := []byte("plain text document")
	enc, err := repo.encryptBytes(data)
	if err != nil {
		t.Fatalf("unexpected encrypt error: %v", err)
	}
	if !bytes.Equal(enc, data) {
		t.Fatalf("expected plaintext to be returned as-is")
	}

	dec, err := repo.decryptBytes(data)
	if err != nil {
		t.Fatalf("unexpected decrypt error: %v", err)
	}
	if !bytes.Equal(dec, data) {
		t.Fatalf("expected data to be returned as-is")
	}
}

func TestKYCRepo_Crypto_LegacyMagicBytes(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	repo := NewKYCRepo(nil, key)

	// JPEG header
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	dec, err := repo.decryptBytes(jpeg)
	if err != nil {
		t.Fatalf("decrypt legacy JPEG failed: %v", err)
	}
	if !bytes.Equal(dec, jpeg) {
		t.Fatalf("expected legacy JPEG to pass through unmolested")
	}

	// PNG header
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	dec, err = repo.decryptBytes(png)
	if err != nil {
		t.Fatalf("decrypt legacy PNG failed: %v", err)
	}
	if !bytes.Equal(dec, png) {
		t.Fatalf("expected legacy PNG to pass through unmolested")
	}

	// PDF header
	pdf := []byte{'%', 'P', 'D', 'F', '-', '1', '.', '4'}
	dec, err = repo.decryptBytes(pdf)
	if err != nil {
		t.Fatalf("decrypt legacy PDF failed: %v", err)
	}
	if !bytes.Equal(dec, pdf) {
		t.Fatalf("expected legacy PDF to pass through unmolested")
	}
}

func TestKYCRepo_Crypto_InvalidKeyFailsClosed(t *testing.T) {
	badKey := []byte("too-short")
	repo := NewKYCRepo(nil, badKey)

	_, err := repo.encryptBytes([]byte("test"))
	if err == nil {
		t.Fatalf("expected error on encrypt with malformed key")
	}

	_, err = repo.decryptBytes([]byte("test"))
	if err == nil {
		t.Fatalf("expected error on decrypt with malformed key")
	}
}
