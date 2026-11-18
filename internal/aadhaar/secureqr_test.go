package aadhaar

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"testing"
)

func TestDecodeSecureQRVerified(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if err := SetSecureQRPublicKeyPEM(string(pemBytes)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetSecureQRPublicKeyPEM("") })

	// indicator + uid + name + gender + yob, 0xFF separated, then 256-byte sig.
	var data []byte
	data = append(data, 0) // no email/mobile
	data = append(data, []byte("123412341234")...)
	data = append(data, 255)
	data = append(data, []byte("Test User")...)
	data = append(data, 255)
	data = append(data, []byte("M")...)
	data = append(data, 255)
	data = append(data, []byte("1990")...)
	sum := sha256.Sum256(data)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	buf := append(append([]byte{}, data...), sig...)
	n := new(big.Int).SetBytes(buf)
	raw := n.Text(10)

	got, partial, err := DecodeAadhaarQR(raw)
	if err != nil {
		t.Fatal(err)
	}
	if partial {
		t.Fatalf("expected complete, got %+v", got)
	}
	if !got.Verified || got.Name != "Test User" || got.UIDLast4 != "1234" || got.Gender != "M" || got.YOB != "1990" {
		t.Fatalf("got %+v", got)
	}
}

func TestDecodeSecureQRRejectsBadSignature(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	_ = SetSecureQRPublicKeyPEM(string(pemBytes))
	t.Cleanup(func() { _ = SetSecureQRPublicKeyPEM("") })

	data := []byte{0, '1', '2', '3', '4'}
	sig := make([]byte, 256) // zeros
	buf := append(data, sig...)
	raw := new(big.Int).SetBytes(buf).Text(10)
	_, _, err = DecodeAadhaarQR(raw)
	if err == nil {
		t.Fatal("expected signature error")
	}
}

func TestDecodeSecureQR_WithPhotoExtraction(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if err := SetSecureQRPublicKeyPEM(string(pemBytes)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetSecureQRPublicKeyPEM("") })

	t.Run("JPEG 2000 codestream with internal 0xFF bytes", func(t *testing.T) {
		// Mock J2K codestream starting with 0xFF4FFF51 (SOC + SIZ) and containing internal 0xFF markers
		j2kPhoto := []byte{0xFF, 0x4F, 0xFF, 0x51, 0x00, 0x2A, 0xFF, 0x52, 0x12, 0x34, 0xFF, 0xD9}

		var data []byte
		data = append(data, 0) // no email/mobile
		data = append(data, []byte("999988887777")...)
		data = append(data, 255)
		data = append(data, []byte("Aadhaar Owner")...)
		data = append(data, 255)
		data = append(data, []byte("F")...)
		data = append(data, 255)
		data = append(data, []byte("1995")...)
		data = append(data, 255) // separator before photo
		data = append(data, j2kPhoto...)

		sum := sha256.Sum256(data)
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		buf := append(append([]byte{}, data...), sig...)
		raw := new(big.Int).SetBytes(buf).Text(10)

		got, partial, err := DecodeAadhaarQR(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if partial {
			t.Fatalf("expected complete, got partial: %+v", got)
		}
		if got.Name != "Aadhaar Owner" || got.Gender != "F" || got.YOB != "1995" || got.UIDLast4 != "7777" {
			t.Fatalf("demographic fields corrupted: %+v", got)
		}
		if len(got.PhotoBytes) != len(j2kPhoto) {
			t.Fatalf("expected photo len %d, got %d", len(j2kPhoto), len(got.PhotoBytes))
		}
		for i := range j2kPhoto {
			if got.PhotoBytes[i] != j2kPhoto[i] {
				t.Fatalf("photo mismatch at byte %d: expected %x, got %x", i, j2kPhoto[i], got.PhotoBytes[i])
			}
		}
	})

	t.Run("Standard JPEG photo extraction", func(t *testing.T) {
		jpegPhoto := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xFF, 0xD9}

		var data []byte
		data = append(data, 0)
		data = append(data, []byte("111122223333")...)
		data = append(data, 255)
		data = append(data, []byte("Priya Sharma")...)
		data = append(data, 255)
		data = append(data, []byte("F")...)
		data = append(data, 255)
		data = append(data, []byte("1992")...)
		data = append(data, 255)
		data = append(data, jpegPhoto...)

		sum := sha256.Sum256(data)
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		buf := append(append([]byte{}, data...), sig...)
		raw := new(big.Int).SetBytes(buf).Text(10)

		got, partial, err := DecodeAadhaarQR(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if partial {
			t.Fatalf("expected complete, got partial: %+v", got)
		}
		if got.Name != "Priya Sharma" || got.UIDLast4 != "3333" {
			t.Fatalf("demographics mismatch: %+v", got)
		}
		if len(got.PhotoBytes) != len(jpegPhoto) {
			t.Fatalf("expected photo len %d, got %d", len(jpegPhoto), len(got.PhotoBytes))
		}
	})
}
