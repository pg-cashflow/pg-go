package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

const (
	// CurrentKeyVersion is the default key version for new encryptions.
	CurrentKeyVersion byte = 1
	// NonceSize is the standard 96-bit nonce length for AES-GCM.
	NonceSize = 12
)

var (
	ErrCiphertextTooShort = errors.New("crypto: ciphertext too short")
	ErrInvalidKeyVersion  = errors.New("crypto: invalid key version")
	ErrDecryptionFailed   = errors.New("crypto: decryption failed or ciphertext corrupted")
	ErrInvalidKey         = errors.New("crypto: key must be 32 bytes for AES-256")
)

// DeriveKey derives a deterministic 32-byte AES-256 key from a secret string using SHA-256.
func DeriveKey(secret string) []byte {
	hash := sha256.Sum256([]byte(secret))
	return hash[:]
}

// Encrypt encrypts plaintext using AES-256-GCM authenticated envelope encryption:
// [1 byte key version] || [12 bytes random nonce] || [ciphertext + 16 bytes authentication tag].
// The key version is bound as Additional Authenticated Data (AAD) to prevent version-tampering.
func Encrypt(key []byte, plaintext []byte, keyVersion ...byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	version := CurrentKeyVersion
	if len(keyVersion) > 0 {
		version = keyVersion[0]
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: create gcm: %w", err)
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: generate nonce: %w", err)
	}

	// Bind key version in AAD so it cannot be altered without failing authentication
	aad := []byte{version}
	sealed := gcm.Seal(nil, nonce, plaintext, aad)

	out := make([]byte, 1+NonceSize+len(sealed))
	out[0] = version
	copy(out[1:1+NonceSize], nonce)
	copy(out[1+NonceSize:], sealed)

	return out, nil
}

// Decrypt verifies authentication and decrypts ciphertext created by Encrypt.
func Decrypt(key []byte, ciphertext []byte) ([]byte, byte, error) {
	if len(key) != 32 {
		return nil, 0, ErrInvalidKey
	}
	// Minimum length: 1 byte version + 12 bytes nonce + 16 bytes GCM tag
	if len(ciphertext) < 1+NonceSize+16 {
		return nil, 0, ErrCiphertextTooShort
	}

	version := ciphertext[0]
	if version != CurrentKeyVersion {
		return nil, version, ErrInvalidKeyVersion
	}

	nonce := ciphertext[1 : 1+NonceSize]
	sealed := ciphertext[1+NonceSize:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, version, fmt.Errorf("crypto: create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, version, fmt.Errorf("crypto: create gcm: %w", err)
	}

	aad := []byte{version}
	plaintext, err := gcm.Open(nil, nonce, sealed, aad)
	if err != nil {
		return nil, version, ErrDecryptionFailed
	}

	return plaintext, version, nil
}
