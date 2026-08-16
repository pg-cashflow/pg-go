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
