package config

import (
	"os"
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		JWTSecret:             "this-is-a-valid-jwt-secret-that-is-over-32-chars-long",
		OTPHMACSecret:         "this-is-a-valid-otp-hmac-secret-over-32-chars",
		MagicLinkHMACSecret:   "this-is-a-valid-magic-link-hmac-secret-over-32",
		KYCIdentitySecret:     "this-is-a-valid-kyc-identity-secret-over-32-ch",
		CashfreeAppID:         "",
		CashfreeSecretKey:     "",
		CashfreeWebhookSecret: "",
		AdminEmail:            "admin@example.com",
		AppEnv:                "development",
	}
}

func TestValidateForRealDeployment_Valid(t *testing.T) {
	cfg := validConfig()
	if err := cfg.ValidateForRealDeployment(); err != nil {
		t.Fatalf("expected nil error for valid config, got: %v", err)
	}
}

func TestValidateForRealDeployment_ValidWithoutKYC(t *testing.T) {
	cfg := validConfig()
	cfg.KYCIdentitySecret = "" // dormant KYC mode
	if err := cfg.ValidateForRealDeployment(); err != nil {
		t.Fatalf("expected nil error when KYC is dormant, got: %v", err)
	}
}

func TestValidateForRealDeployment_PlaceholderSecrets(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantSub string
	}{
		{
			name: "JWT placeholder",
			mutate: func(c *Config) {
				c.JWTSecret = "change-me-to-a-long-random-string-at-least-32-chars"
			},
			wantSub: "JWT_SECRET must not be a placeholder",
		},
		{
			name: "OTP HMAC placeholder",
			mutate: func(c *Config) {
				c.OTPHMACSecret = "change-me-to-a-long-random-string-at-least-32-chars"
			},
			wantSub: "OTP_HMAC_SECRET must not be a placeholder",
		},
		{
			name: "Magic link HMAC placeholder",
			mutate: func(c *Config) {
				c.MagicLinkHMACSecret = "change-me-to-a-long-random-string-at-least-32-chars"
			},
			wantSub: "MAGIC_LINK_HMAC_SECRET must not be a placeholder",
		},
		{
			name: "KYC identity secret placeholder",
			mutate: func(c *Config) {
				c.KYCIdentitySecret = "change-me-to-a-long-random-string-at-least-32-chars"
			},
			wantSub: "KYC_IDENTITY_SECRET must not be a placeholder",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(cfg)
			err := cfg.ValidateForRealDeployment()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

func TestValidateForRealDeployment_ShortSecrets(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantSub string
	}{
		{
			name: "JWT short",
			mutate: func(c *Config) {
				c.JWTSecret = "short-secret"
			},
			wantSub: "JWT_SECRET must be at least 32 characters",
		},
		{
			name: "OTP short",
			mutate: func(c *Config) {
				c.OTPHMACSecret = "short-secret"
			},
			wantSub: "OTP_HMAC_SECRET must be at least 32 characters",
		},
		{
			name: "Magic link short",
			mutate: func(c *Config) {
				c.MagicLinkHMACSecret = "short-secret"
			},
			wantSub: "MAGIC_LINK_HMAC_SECRET must be at least 32 characters",
		},
		{
			name: "KYC identity secret short",
			mutate: func(c *Config) {
				c.KYCIdentitySecret = "short-secret"
			},
			wantSub: "KYC_IDENTITY_SECRET must be at least 32 characters",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(cfg)
			err := cfg.ValidateForRealDeployment()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

func TestValidateForRealDeployment_CashfreeWebhookSecretMissing(t *testing.T) {
	cfg := validConfig()
	cfg.CashfreeAppID = "cf_app_123"
	cfg.CashfreeSecretKey = "cf_sec_123"
	cfg.CashfreeWebhookSecret = ""

	err := cfg.ValidateForRealDeployment()
	if err == nil {
		t.Fatalf("expected error when Cashfree is enabled but webhook secret missing")
	}
	if !strings.Contains(err.Error(), "CASHFREE_WEBHOOK_SECRET is required") {
		t.Fatalf("expected error containing 'CASHFREE_WEBHOOK_SECRET is required', got: %v", err)
	}
}

func TestValidateForRealDeployment_AutoMigrateProduction(t *testing.T) {
	prev := os.Getenv("AUTO_MIGRATE")
	defer func() { _ = os.Setenv("AUTO_MIGRATE", prev) }()

	_ = os.Setenv("AUTO_MIGRATE", "1")
	cfg := validConfig()
	cfg.AppEnv = "production"

	err := cfg.ValidateForRealDeployment()
	if err == nil {
		t.Fatalf("expected error when AUTO_MIGRATE=1 in production")
	}
	if !strings.Contains(err.Error(), "AUTO_MIGRATE=1 is disallowed in production") {
		t.Fatalf("expected error containing 'AUTO_MIGRATE=1 is disallowed in production', got: %v", err)
	}
}

func TestValidateWarnings(t *testing.T) {
	prev := os.Getenv("CASHFREE_WEBHOOK_SECRET")
	defer func() {
		if prev != "" {
			_ = os.Setenv("CASHFREE_WEBHOOK_SECRET", prev)
		} else {
			_ = os.Unsetenv("CASHFREE_WEBHOOK_SECRET")
		}
	}()
	_ = os.Unsetenv("CASHFREE_WEBHOOK_SECRET")

	cfg := validConfig()
	cfg.CashfreeAppID = "app_123"
	cfg.CashfreeSecretKey = "sec_123"
	cfg.CashfreeWebhookSecret = "sec_123"
	cfg.CashfreeEnv = "sandbox"

	warns := cfg.ValidateWarnings()
	if len(warns) != 2 {
		t.Fatalf("expected 2 warnings (fallback + sandbox), got %d: %v", len(warns), warns)
	}
}

func TestValidateWarnings_KYCWithoutCashfree(t *testing.T) {
	cfg := validConfig()
	cfg.CashfreeAppID = ""
	cfg.CashfreeSecretKey = ""
	cfg.CashfreeWebhookSecret = ""
	cfg.KYCIdentitySecret = "this-is-a-valid-kyc-identity-secret-over-32-ch"

	warns := cfg.ValidateWarnings()
	found := false
	for _, w := range warns {
		if strings.Contains(w, "KYC_IDENTITY_SECRET is set but Cashfree is not configured") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected warning about KYC without Cashfree, got: %v", warns)
	}
}

func TestValidateForRealDeployment_AdminEmailRequired(t *testing.T) {
	cfg := validConfig()
	cfg.AppEnv = "production"
	cfg.AdminEmail = ""
	err := cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "ADMIN_EMAIL") {
		t.Fatalf("expected error mentioning ADMIN_EMAIL in production, got: %v", err)
	}

	cfg.AppEnv = "development"
	cfg.CashfreeAppID = "CF_TEST_APP"
	cfg.CashfreeSecretKey = "CF_TEST_SECRET"
	cfg.CashfreeWebhookSecret = "CF_TEST_WEBHOOK_SECRET"
	cfg.AdminEmail = ""
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "ADMIN_EMAIL") {
		t.Fatalf("expected error mentioning ADMIN_EMAIL when Cashfree is enabled, got: %v", err)
	}
}

func TestValidateWarnings_AdminEmailUnset(t *testing.T) {
	cfg := validConfig()
	cfg.AdminEmail = ""
	warns := cfg.ValidateWarnings()
	found := false
	for _, w := range warns {
		if strings.Contains(w, "ADMIN_EMAIL is not configured") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected warning about unset ADMIN_EMAIL, got: %v", warns)
	}
}

func TestValidateForRealDeployment_PayoutCredentialsRequirePayoutSecrets(t *testing.T) {
	cfg := validConfig()
	cfg.CashfreePayoutClientID = "CF_TEST_PAYOUT_CLIENT_ID"
	cfg.CashfreePayoutClientSecret = "CF_TEST_PAYOUT_CLIENT_SECRET"
	cfg.CashfreePayoutFundsourceID = "FUND_SOURCE_123"
	cfg.CashfreePayoutWebhookSecret = "this-is-a-valid-payout-webhook-secret-32ch"

	// Missing PayoutEncryptionSecret and PayoutExportChecksumSecret
	err := cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "PAYOUT_ENCRYPTION_SECRET is required") {
		t.Fatalf("expected error requiring PAYOUT_ENCRYPTION_SECRET, got: %v", err)
	}
	if !strings.Contains(err.Error(), "PAYOUT_CHECKSUM_SECRET is required") {
		t.Fatalf("expected error requiring PAYOUT_CHECKSUM_SECRET, got: %v", err)
	}

	// Supply valid secrets
	cfg.PayoutEncryptionSecret = "this-is-a-valid-payout-encryption-secret-32"
	cfg.PayoutExportChecksumSecret = "this-is-a-valid-payout-checksum-secret-32ch"
	if err := cfg.ValidateForRealDeployment(); err != nil {
		t.Fatalf("expected valid deployment with payout secrets set, got: %v", err)
	}
}

func TestValidateForRealDeployment_SecretUniqueness(t *testing.T) {
	cfg := validConfig()
	// Reusing same secret for JWT and OTP
	cfg.OTPHMACSecret = cfg.JWTSecret
	err := cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "JWT_SECRET and OTP_HMAC_SECRET must be distinct") {
		t.Fatalf("expected error about secret reuse between JWT and OTP, got: %v", err)
	}

	cfg = validConfig()
	cfg.MagicLinkHMACSecret = cfg.JWTSecret
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "JWT_SECRET and MAGIC_LINK_HMAC_SECRET must be distinct") {
		t.Fatalf("expected error about secret reuse between JWT and MagicLink, got: %v", err)
	}
}

func TestValidateForRealDeployment_MetricsSecret(t *testing.T) {
	cfg := validConfig()
	cfg.MetricsSecret = "short"
	err := cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "METRICS_SECRET must be at least 16 characters") {
		t.Fatalf("expected error for short METRICS_SECRET, got: %v", err)
	}

	cfg.MetricsSecret = "change-me-metrics-secret"
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "METRICS_SECRET must not be a placeholder") {
		t.Fatalf("expected error for placeholder METRICS_SECRET, got: %v", err)
	}
}

func TestValidateForRealDeployment_VersionedPayoutEncryptionKeys(t *testing.T) {
	cfg := validConfig()
	cfg.PayoutEncryptionKeys = map[byte]string{
		1: "short",
	}
	err := cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "PAYOUT_ENCRYPTION_KEYS[1] must be at least 32 characters") {
		t.Fatalf("expected error for short versioned payout key, got: %v", err)
	}
}
