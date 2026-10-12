package config

import (
	"os"
	"strings"
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
	keys := []string{"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET", "APP_ENV", "FIREBASE_PROJECT_ID", "PAYOUT_ENCRYPTION_SECRET", "PAYOUT_ENCRYPTION_KEY", "PAYOUT_CHECKSUM_SECRET"}
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
	_ = os.Setenv("PAYOUT_CHECKSUM_SECRET", "checksum-secret-min-32-chars-long-abc")

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

func TestLoad_CashfreePGBaseURL(t *testing.T) {
	keys := []string{
		"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET",
		"APP_ENV", "FIREBASE_PROJECT_ID", "PAYOUT_ENCRYPTION_SECRET", "CASHFREE_PG_BASE_URL",
	}
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
	_ = os.Setenv("APP_ENV", "development")

	// Case 1: Unset default returns empty string and succeeds
	_ = os.Unsetenv("CASHFREE_PG_BASE_URL")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed with unset base URL: %v", err)
	}
	if cfg.CashfreePGBaseURL != "" {
		t.Fatalf("expected empty CashfreePGBaseURL, got %q", cfg.CashfreePGBaseURL)
	}

	// Case 2: Production refusal
	_ = os.Setenv("APP_ENV", "production")
	_ = os.Setenv("FIREBASE_PROJECT_ID", "firebase-project-id")
	_ = os.Setenv("PAYOUT_ENCRYPTION_SECRET", "payout-secret-min-32-chars-long-abc")
	_ = os.Setenv("CASHFREE_PG_BASE_URL", "http://localhost:8081")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when CASHFREE_PG_BASE_URL is set in production")
	}

	// Reset to development
	_ = os.Setenv("APP_ENV", "development")

	// Case 3: Bad scheme (https instead of http)
	_ = os.Setenv("CASHFREE_PG_BASE_URL", "https://localhost:8081")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when CASHFREE_PG_BASE_URL scheme is not http")
	}

	// Case 4: Remote host refusal
	_ = os.Setenv("CASHFREE_PG_BASE_URL", "http://api.cashfree.com/pg")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when CASHFREE_PG_BASE_URL targets remote host")
	}

	// Case 5: Private network non-loopback IP refusal
	_ = os.Setenv("CASHFREE_PG_BASE_URL", "http://192.168.1.100:8081")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when CASHFREE_PG_BASE_URL targets private non-loopback IP")
	}

	// Case 6: Valid loopback host: localhost
	_ = os.Setenv("CASHFREE_PG_BASE_URL", "http://localhost:8081")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load failed for http://localhost:8081: %v", err)
	}
	if cfg.CashfreePGBaseURL != "http://localhost:8081" {
		t.Fatalf("expected http://localhost:8081, got %q", cfg.CashfreePGBaseURL)
	}

	// Case 7: Valid loopback host: 127.0.0.1
	_ = os.Setenv("CASHFREE_PG_BASE_URL", "http://127.0.0.1:8081")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load failed for http://127.0.0.1:8081: %v", err)
	}
	if cfg.CashfreePGBaseURL != "http://127.0.0.1:8081" {
		t.Fatalf("expected http://127.0.0.1:8081, got %q", cfg.CashfreePGBaseURL)
	}
}

func TestLoad_CashfreePGBaseURL_ProductionGuard(t *testing.T) {
	keys := []string{
		"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET",
		"APP_ENV", "CASHFREE_ENV", "FIREBASE_PROJECT_ID", "PAYOUT_ENCRYPTION_SECRET",
		"CASHFREE_PG_BASE_URL",
	}
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

	baseSetup := func() {
		_ = os.Setenv("DATABASE_URL", "postgres://localhost/db")
		_ = os.Setenv("JWT_SECRET", "jwt-secret-min-32-chars-long-abc")
		_ = os.Setenv("OTP_HMAC_SECRET", "otp-secret-min-32-chars-long-abc")
		_ = os.Setenv("MAGIC_LINK_HMAC_SECRET", "magic-secret-min-32-chars-long-abc")
		_ = os.Setenv("FIREBASE_PROJECT_ID", "firebase-project-id")
		_ = os.Setenv("PAYOUT_ENCRYPTION_SECRET", "payout-secret-min-32-chars-long-abc")
		_ = os.Setenv("PAYOUT_CHECKSUM_SECRET", "checksum-secret-min-32-chars-long-abc")
		_ = os.Setenv("CASHFREE_PG_BASE_URL", "http://127.0.0.1:8081")
	}

	tests := []struct {
		name        string
		appEnv      string
		cashfreeEnv string
		expectErr   bool
	}{
		{
			name:        "app_env_production_cf_sandbox",
			appEnv:      "production",
			cashfreeEnv: "sandbox",
			expectErr:   true,
		},
		{
			name:        "app_env_production_cf_unset",
			appEnv:      "production",
			cashfreeEnv: "",
			expectErr:   true,
		},
		{
			name:        "app_env_development_cf_production",
			appEnv:      "development",
			cashfreeEnv: "production",
			expectErr:   true,
		},
		{
			name:        "app_env_unset_cf_production_case_insensitive",
			appEnv:      "",
			cashfreeEnv: "PRODUCTION",
			expectErr:   true,
		},
		{
			name:        "app_env_development_cf_sandbox_allowed",
			appEnv:      "development",
			cashfreeEnv: "sandbox",
			expectErr:   false,
		},
		{
			name:        "app_env_test_cf_unset_allowed",
			appEnv:      "test",
			cashfreeEnv: "",
			expectErr:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseSetup()
			if tc.appEnv != "" {
				_ = os.Setenv("APP_ENV", tc.appEnv)
			} else {
				_ = os.Unsetenv("APP_ENV")
			}
			if tc.cashfreeEnv != "" {
				_ = os.Setenv("CASHFREE_ENV", tc.cashfreeEnv)
			} else {
				_ = os.Unsetenv("CASHFREE_ENV")
			}

			cfg, err := Load()
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error when overriding base URL in production (appEnv=%q, cashfreeEnv=%q)", tc.appEnv, tc.cashfreeEnv)
				}
				if !strings.Contains(err.Error(), "CASHFREE_PG_BASE_URL override is prohibited in production") {
					t.Fatalf("expected production prohibited error, got: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error for non-production override: %v", err)
				}
				if cfg.CashfreePGBaseURL != "http://127.0.0.1:8081" {
					t.Fatalf("expected CashfreePGBaseURL to be set, got %q", cfg.CashfreePGBaseURL)
				}
			}
		})
	}
}

func TestLoadRejectsNonPositiveWebhookTimestampTolerance(t *testing.T) {
	keys := []string{
		"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET",
		"FIREBASE_PROJECT_ID", "PAYOUT_ENCRYPTION_SECRET", "WEBHOOK_TIMESTAMP_TOLERANCE_SEC",
	}
	prev := map[string]string{}
	for _, k := range keys {
		prev[k] = os.Getenv(k)
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
	_ = os.Setenv("FIREBASE_PROJECT_ID", "firebase-project-id")
	_ = os.Setenv("PAYOUT_ENCRYPTION_SECRET", "payout-secret-min-32-chars-long-abc")

	for _, invalid := range []string{"0", "-1", "-300"} {
		_ = os.Setenv("WEBHOOK_TIMESTAMP_TOLERANCE_SEC", invalid)
		if _, err := Load(); err == nil {
			t.Fatalf("expected error when WEBHOOK_TIMESTAMP_TOLERANCE_SEC=%s, got nil", invalid)
		}
	}

	_ = os.Setenv("WEBHOOK_TIMESTAMP_TOLERANCE_SEC", "300")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error for tolerance 300: %v", err)
	}
	if cfg.WebhookTimestampToleranceSec != 300 {
		t.Fatalf("expected tolerance 300, got %d", cfg.WebhookTimestampToleranceSec)
	}
}
