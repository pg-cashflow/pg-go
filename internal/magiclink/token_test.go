package magiclink

import "testing"

func TestHashToken_Deterministic(t *testing.T) {
	secret := "test-secret"
	raw := "deadbeefcafebabe"
	h1 := HashToken(secret, raw)
	h2 := HashToken(secret, raw)
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("hmac-sha256 hex len=%d want 64", len(h1))
	}
}

func TestVerifyToken_Basics(t *testing.T) {
	secret := "test-secret"
	raw, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash := HashToken(secret, raw)
	if !VerifyToken(secret, raw, hash) {
		t.Fatal("expected verify true")
	}
	if VerifyToken(secret, raw+"x", hash) {
		t.Fatal("expected verify false for wrong raw")
	}
	if VerifyToken("other", raw, hash) {
		t.Fatal("expected verify false for wrong secret")
	}
}

func TestGenerateToken_Length(t *testing.T) {
	raw, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	// 32 bytes → 64 hex chars
	if len(raw) != 64 {
		t.Fatalf("len=%d want 64", len(raw))
	}
}
