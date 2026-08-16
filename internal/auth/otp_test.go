package auth

import "testing"

func TestGenerateOTP(t *testing.T) {
	code, err := GenerateOTP()
	if err != nil {
		t.Fatalf("GenerateOTP: %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("expected 6 digits, got %q", code)
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			t.Fatalf("non-digit in otp: %q", code)
		}
	}
}

func TestHashAndVerifyOTP(t *testing.T) {
	const secret = "test-otp-hmac-secret"
	code, err := GenerateOTP()
	if err != nil {
		t.Fatalf("GenerateOTP: %v", err)
	}
	hash := HashOTP(secret, code)
	if hash == "" || hash == code {
		t.Fatalf("hash should be non-empty and different from plaintext")
	}
	if !VerifyOTP(secret, code, hash) {
		t.Fatal("VerifyOTP failed for matching otp")
	}
	if VerifyOTP(secret, "000000", hash) && code != "000000" {
		t.Fatal("VerifyOTP accepted wrong otp")
	}
	if VerifyOTP("other-secret", code, hash) {
		t.Fatal("VerifyOTP accepted wrong secret")
	}
}

func TestHashOTPDeterministic(t *testing.T) {
	const secret = "secret"
	a := HashOTP(secret, "123456")
	b := HashOTP(secret, "123456")
	if a != b {
		t.Fatalf("hash not deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(a))
	}
}
