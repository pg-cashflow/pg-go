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

// KeyRing manages versioned symmetric keys for encryption and rotation.
type KeyRing struct {
	primaryVersion byte
	keys           map[byte][]byte
}

// NewKeyRing creates a new KeyRing initialized with a primary key version.
func NewKeyRing(primaryVersion byte, primaryKey []byte) (*KeyRing, error) {
	if len(primaryKey) != 32 {
		return nil, ErrInvalidKey
	}
	kr := &KeyRing{
		primaryVersion: primaryVersion,
		keys:           make(map[byte][]byte),
	}
	kr.keys[primaryVersion] = primaryKey
	return kr, nil
}

// AddKey registers an additional (e.g. historical or rotating) key version.
func (kr *KeyRing) AddKey(version byte, key []byte) error {
	if kr == nil {
		return errors.New("crypto: nil key ring")
	}
	if len(key) != 32 {
		return ErrInvalidKey
	}
	kr.keys[version] = key
	return nil
}

// PrimaryVersion returns the primary key version used for new encryptions.
func (kr *KeyRing) PrimaryVersion() byte {
	if kr == nil {
		return CurrentKeyVersion
	}
	return kr.primaryVersion
}

// PrimaryKey returns the 32-byte key used for new encryptions.
func (kr *KeyRing) PrimaryKey() []byte {
	if kr == nil {
		return nil
	}
	return kr.keys[kr.primaryVersion]
}

// GetKey retrieves the key for a specific version.
func (kr *KeyRing) GetKey(version byte) ([]byte, bool) {
	if kr == nil {
		return nil, false
	}
	k, ok := kr.keys[version]
	return k, ok
}

// EncryptWithKeyRing encrypts plaintext using the primary key version of the KeyRing.
func EncryptWithKeyRing(kr *KeyRing, plaintext []byte) ([]byte, error) {
	if kr == nil {
		return nil, errors.New("crypto: nil key ring")
	}
	key := kr.PrimaryKey()
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	return Encrypt(key, plaintext, kr.PrimaryVersion())
}

// DecryptWithKeyRing inspects the ciphertext header for its version byte,
// selects the matching key from the KeyRing, and decrypts the payload.
func DecryptWithKeyRing(kr *KeyRing, ciphertext []byte) ([]byte, byte, error) {
	if kr == nil {
		return nil, 0, errors.New("crypto: nil key ring")
	}
	if len(ciphertext) < 1+NonceSize+16 {
		return nil, 0, ErrCiphertextTooShort
	}
	version := ciphertext[0]
	key, ok := kr.GetKey(version)
	if !ok || len(key) != 32 {
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

