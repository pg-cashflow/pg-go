package config

import (
	"os"
	"testing"
)

func TestLoadRequiresSecrets(t *testing.T) {
	keys := []string{"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET", "KYC_IDENTITY_SECRET"}
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
	// Crucial: KYCIdentitySecret must NOT fall back to JWTSecret; it must stay empty so KYC degrades gracefully
	if cfg.KYCIdentitySecret != "" {
		t.Fatalf("expected empty KYCIdentitySecret when unset, got %q", cfg.KYCIdentitySecret)
	}

	// When set, it should be loaded faithfully
	_ = os.Setenv("KYC_IDENTITY_SECRET", "kyc-custom-secret")
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.KYCIdentitySecret != "kyc-custom-secret" {
		t.Fatalf("expected kyc-custom-secret, got %q", cfg2.KYCIdentitySecret)
	}
}

func TestLoadDefaultsWebhookSecretToClientSecret(t *testing.T) {
	keys := []string{"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET", "KYC_IDENTITY_SECRET", "CASHFREE_SECRET_KEY", "CASHFREE_WEBHOOK_SECRET", "CASHFREE_APP_ID", "APP_ENV"}
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
	_ = os.Setenv("KYC_IDENTITY_SECRET", "kyc")
	_ = os.Setenv("CASHFREE_SECRET_KEY", "sk_test")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CashfreeWebhookSecret != "sk_test" {
		t.Fatalf("webhook secret=%q", cfg.CashfreeWebhookSecret)
	}
}

func TestLoad_ProductionRequiresPayoutEncryptionSecret(t *testing.T) {
	keys := []string{"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET", "APP_ENV", "FIREBASE_PROJECT_ID", "PAYOUT_ENCRYPTION_SECRET", "PAYOUT_ENCRYPTION_KEY"}
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
	_ = os.Setenv("JWT_SECRET", "jwt-secret-min-32-chars-long-abc")
	_ = os.Setenv("OTP_HMAC_SECRET", "otp-secret-min-32-chars-long-abc")
	_ = os.Setenv("MAGIC_LINK_HMAC_SECRET", "magic-secret-min-32-chars-long-abc")
	_ = os.Setenv("APP_ENV", "production")
	_ = os.Setenv("FIREBASE_PROJECT_ID", "firebase-project-id")

	// In production, when PAYOUT_ENCRYPTION_SECRET is missing, Load must fail closed
	if _, err := Load(); err == nil {
		t.Fatal("expected error when PAYOUT_ENCRYPTION_SECRET is missing in production")
	}

	// When set, Load must succeed
	_ = os.Setenv("PAYOUT_ENCRYPTION_SECRET", "payout-secret-min-32-chars-long-abc")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed with PAYOUT_ENCRYPTION_SECRET set: %v", err)
	}
	if cfg.PayoutEncryptionSecret != "payout-secret-min-32-chars-long-abc" {
		t.Fatalf("unexpected PayoutEncryptionSecret: %s", cfg.PayoutEncryptionSecret)
	}
}
