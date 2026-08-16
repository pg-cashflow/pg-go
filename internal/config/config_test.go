package config

import (
	"os"
	"testing"
)

func TestLoadRequiresSecrets(t *testing.T) {
	keys := []string{"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET"}
	prev := map[string]string{}
	for _, k := range keys {
		prev[k] = os.Getenv(k)
		_ = os.Unsetenv(k)
	}
	defer func() {
		for k, v := range prev {
			if v == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, v)
			}
		}
	}()

	_ = os.Setenv("DATABASE_URL", "postgres://localhost/db")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when JWT_SECRET empty")
	}
	_ = os.Setenv("JWT_SECRET", "jwt")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when OTP_HMAC_SECRET empty")
	}
	_ = os.Setenv("OTP_HMAC_SECRET", "otp")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when MAGIC_LINK_HMAC_SECRET empty")
	}
	_ = os.Setenv("MAGIC_LINK_HMAC_SECRET", "magic")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JWTSecret != "jwt" {
		t.Fatalf("got %#v", cfg)
	}
}
