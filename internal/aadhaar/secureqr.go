package aadhaar

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"unicode"
)

var (
	ErrInvalidSignature = errors.New("aadhaar: secure QR signature invalid")
	ErrSecureQRFormat   = errors.New("aadhaar: secure QR payload unreadable")
)

// Secure QR signatures are RSA-SHA256 over the data bytes (excluding the trailing 256-byte signature).
// There is no bundled UIDAI key: a guessed key would be worse than fail-closed.
// Set AADHAAR_QR_PUBLIC_KEY_PEM (UIDAI offline public key / cert PEM) at process start.

var (
	secureKeyMu sync.RWMutex
	securePub   *rsa.PublicKey
)

func init() {
	_ = SetSecureQRPublicKeyPEM("") // no default trust
}

// SetSecureQRPublicKeyPEM installs the UIDAI (or test) RSA public key used to verify Secure QR.
func SetSecureQRPublicKeyPEM(pemStr string) error {
	secureKeyMu.Lock()
	defer secureKeyMu.Unlock()
	pemStr = strings.TrimSpace(pemStr)
	if pemStr == "" {
		securePub = nil
		return nil
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return fmt.Errorf("aadhaar: invalid public key pem")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		cert, cerr := x509.ParseCertificate(block.Bytes)
		if cerr != nil {
			return fmt.Errorf("aadhaar: parse public key: %w", err)
		}
		var ok bool
		securePub, ok = cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("aadhaar: cert is not RSA")
		}
		return nil
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("aadhaar: not an RSA public key")
	}
	securePub = rsaPub
	return nil
}

func currentSecurePub() *rsa.PublicKey {
	secureKeyMu.RLock()
	defer secureKeyMu.RUnlock()
	return securePub
}

func looksLikeSecureQR(raw string) bool {
	s := strings.TrimSpace(raw)
	if len(s) < 50 {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func decodeSecureQR(raw string) (AadhaarData, error) {
	raw = strings.TrimSpace(raw)
	n := new(big.Int)
	if _, ok := n.SetString(raw, 10); !ok {
		return AadhaarData{}, ErrSecureQRFormat
	}
	buf := n.Bytes()
	if len(buf) < 256+16 {
		return AadhaarData{}, ErrSecureQRFormat
	}
	sig := buf[len(buf)-256:]
	data := buf[:len(buf)-256]

	pub := currentSecurePub()
	if pub == nil {
		return AadhaarData{}, fmt.Errorf("%w: UIDAI public key not configured", ErrInvalidSignature)
	}
	if err := verifySecureQR(pub, data, sig); err != nil {
		padded := append([]byte{0}, data...)
		if err2 := verifySecureQR(pub, padded, sig); err2 != nil {
			return AadhaarData{}, ErrInvalidSignature
		}
		data = padded
	}

	fields := splitFF(data)
	if len(fields) < 4 {
		return AadhaarData{}, ErrSecureQRFormat
	}
	// Byte 0 of data is email/mobile flags; first 0xFF-separated field may include it.
	uid := digitsOnly(string(fields[0]))
	if len(uid) < 4 && len(fields) > 1 {
		uid = digitsOnly(string(fields[1]))
	}
	name, gender, dob, yob := pickDemographics(fields)
	return AadhaarData{
		Name:     name,
		DOB:      dob,
		YOB:      yob,
		Gender:   normalizeGender(gender),
		UIDLast4: last4(uid),
		Verified: true,
	}, nil
}

func verifySecureQR(pub *rsa.PublicKey, data, sig []byte) error {
	sum := sha256.Sum256(data)
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig)
}

func splitFF(data []byte) [][]byte {
	var out [][]byte
	start := 0
	if len(data) > 0 {
		start = 1 // skip email/mobile indicator byte
	}
	prev := start
	for i := start; i < len(data); i++ {
		if data[i] == 255 {
			out = append(out, data[prev:i])
			prev = i + 1
		}
	}
	if prev < len(data) {
		out = append(out, data[prev:])
	}
	return out
}

func pickDemographics(fields [][]byte) (name, gender, dob, yob string) {
	// UIDAI order after indicator: uid, name, gender, yob, then address fields.
	idx := 0
	if len(fields) > 0 && len(digitsOnly(string(fields[0]))) >= 4 {
		idx = 1
	}
	if idx < len(fields) {
		name = strings.TrimSpace(string(fields[idx]))
	}
	if idx+1 < len(fields) {
		gender = strings.TrimSpace(string(fields[idx+1]))
	}
	if idx+2 < len(fields) {
		y := strings.TrimSpace(string(fields[idx+2]))
		if len(y) == 4 && digitsOnly(y) == y {
			yob = y
		} else {
			dob = y
		}
	}
	return name, gender, dob, yob
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
