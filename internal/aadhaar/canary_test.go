//go:build canary

package aadhaar

import (
	"os"
	"strings"
	"testing"
)

// TestUIDAISecureQR_LiveCanary tests offline Secure QR verification against configured UIDAI public cert.
//
// NOTE (ADR 004 Inherent Blind Spot):
// This test verifies that the currently configured AADHAAR_QR_PUBLIC_KEY_PEM can successfully verify
// and decode AADHAAR_QR_CANARY_FIXTURE. Because Secure QR verification is entirely offline by design,
// this automated test CANNOT detect UIDAI rotating their certificate in the wild if the fixture
// remains static.
//
// Ops Policy: As documented in ADR 004, a quarterly recurring calendar ticket must re-fetch UIDAI's
// official public certificate from their portal, re-generate a fresh test fixture from an active
// e-Aadhaar/mAadhaar, and update this fixture and PEM.
func TestUIDAISecureQR_LiveCanary(t *testing.T) {
	pem := strings.TrimSpace(os.Getenv("AADHAAR_QR_PUBLIC_KEY_PEM"))
	if pem == "" {
		t.Fatal("AADHAAR_QR_PUBLIC_KEY_PEM environment variable is required for canary testing but was empty")
	}

	fixture := strings.TrimSpace(os.Getenv("AADHAAR_QR_CANARY_FIXTURE"))
	if fixture == "" {
		t.Fatal("AADHAAR_QR_CANARY_FIXTURE environment variable is required for canary testing but was empty")
	}

	if err := SetSecureQRPublicKeyPEM(pem); err != nil {
		t.Fatalf("failed to install UIDAI public key PEM: %v", err)
	}
	t.Cleanup(func() { _ = SetSecureQRPublicKeyPEM("") })

	data, partial, err := DecodeAadhaarQR(fixture)
	if err != nil {
		t.Fatalf("secure QR canary verification failed: %v", err)
	}
	if partial {
		t.Fatal("expected full cryptographic verification, got partial decode")
	}
	if !data.Verified {
		t.Fatal("expected data.Verified to be true")
	}
	if data.Name == "" || data.UIDLast4 == "" {
		t.Fatalf("missing expected demographic fields: %+v", data)
	}
}
