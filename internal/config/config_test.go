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

func TestLoadDefaultsWebhookSecretToClientSecret(t *testing.T) {
	keys := []string{"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET", "CASHFREE_SECRET_KEY", "CASHFREE_WEBHOOK_SECRET", "CASHFREE_APP_ID", "APP_ENV"}
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
	_ = os.Setenv("JWT_SECRET", "jwt")
	_ = os.Setenv("OTP_HMAC_SECRET", "otp")
	_ = os.Setenv("MAGIC_LINK_HMAC_SECRET", "magic")
	_ = os.Setenv("CASHFREE_SECRET_KEY", "sk_test")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CashfreeWebhookSecret != "sk_test" {
		t.Fatalf("webhook secret=%q", cfg.CashfreeWebhookSecret)
	}
}
