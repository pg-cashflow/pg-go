package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		DatabaseURL:           "postgres://pgapp_app:secret@localhost:5432/pgapp",
		DatabaseMaintURL:      "postgres://pgapp_maint:secret@localhost:5432/pgapp",
		DatabaseWorkerURL:     "postgres://pgapp_worker:secret@localhost:5432/pgapp",
		JWTSecret:             "this-is-a-valid-jwt-secret-that-is-over-32-chars-long",
		OTPHMACSecret:         "this-is-a-valid-otp-hmac-secret-over-32-chars",
		MagicLinkHMACSecret:   "this-is-a-valid-magic-link-hmac-secret-over-32",
		KYCIdentitySecret:     "this-is-a-valid-kyc-identity-secret-over-32-ch",
		CashfreeAppID:         "",
		CashfreeSecretKey:     "",
		CashfreeWebhookSecret: "",
		AdminEmail:            "admin@example.com",
		FrontendURL:           "https://pg.example.com",
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
	if err != nil {
		t.Fatalf("PG secret is present; Validate must not require a separate webhook secret (ADR-031), got: %v", err)
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
	if len(warns) != 1 {
		t.Fatalf("expected 1 warning (sandbox only, L1 fallback removed), got %d: %v", len(warns), warns)
	}
	if !strings.Contains(warns[0], "sandbox mode") {
		t.Fatalf("expected sandbox mode warning, got: %v", warns[0])
	}
}

func TestValidateWarnings_CorrectConfigProducesZeroWarnings(t *testing.T) {
	cfg := validConfig()
	cfg.CashfreeAppID = "app_123"
	cfg.CashfreeSecretKey = "sec_123"
	cfg.CashfreeWebhookSecret = "sec_123"
	cfg.CashfreeEnv = "production"
	cfg.AdminEmail = "admin@example.com"

	warns := cfg.ValidateWarnings()
	if len(warns) != 0 {
		t.Fatalf("expected 0 warnings for correct config, got %d: %v", len(warns), warns)
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
	cfg.CashfreePayoutAuthMode = "ip"

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

	cfg = validConfig()
	cfg.AppEnv = "production"
	cfg.MetricsSecret = ""
	cfg.DatabaseWorkerURL = "postgres://worker@localhost/db"
	cfg.DatabaseURL = "postgres://app@localhost/db"
	cfg.DatabaseMaintURL = "postgres://maint@localhost/db"
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "METRICS_SECRET is required in production") {
		t.Fatalf("expected production METRICS_SECRET error, got: %v", err)
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

func TestValidateForRealDeployment_PayoutWebhookSecret_ExemptFrom32Chars(t *testing.T) {
	// A 20-character payout webhook secret passes (ADR-031 D4 / Ticket R2)
	cfg := validConfig()
	cfg.CashfreePayoutClientID = "CF_TEST_PAYOUT_CLIENT_ID"
	cfg.CashfreePayoutClientSecret = "CF_TEST_PAYOUT_CLIENT_SECRET"
	cfg.CashfreePayoutFundsourceID = "FUND_SOURCE_123"
	cfg.PayoutEncryptionSecret = "this-is-a-valid-payout-encryption-secret-32"
	cfg.PayoutExportChecksumSecret = "this-is-a-valid-payout-checksum-secret-32ch"
	cfg.CashfreePayoutWebhookSecret = "twenty-char-secret!!" // exactly 20 characters
	cfg.CashfreePayoutAuthMode = "ip"

	if err := cfg.ValidateForRealDeployment(); err != nil {
		t.Fatalf("expected 20-character payout webhook secret to pass, got: %v", err)
	}

	// change-me-x fails
	cfg.CashfreePayoutWebhookSecret = "change-me-vendor-secret"
	err := cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "CF_PAYOUT_WEBHOOK_SECRET must not be a placeholder") {
		t.Fatalf("expected error for change-me placeholder, got: %v", err)
	}

	// empty with CF_PAYOUT_CLIENT_ID set fails
	cfg.CashfreePayoutWebhookSecret = ""
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "CF_PAYOUT_WEBHOOK_SECRET is required") {
		t.Fatalf("expected error for empty CF_PAYOUT_WEBHOOK_SECRET, got: %v", err)
	}

	// 31-character JWT_SECRET still fails
	cfg.CashfreePayoutWebhookSecret = "twenty-char-secret!!"
	cfg.JWTSecret = "1234567890123456789012345678901" // 31 chars
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "JWT_SECRET must be at least 32 characters") {
		t.Fatalf("expected error for 31-character JWT_SECRET, got: %v", err)
	}
}

func TestValidateForRealDeployment_DatabaseWorkerURL_Production(t *testing.T) {
	cfg := validConfig()
	cfg.AppEnv = "production"
	cfg.MetricsSecret = "metrics-secret-16b"

	// Missing DATABASE_WORKER_URL
	cfg.DatabaseWorkerURL = ""
	err := cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_WORKER_URL is required in production") {
		t.Fatalf("expected error requiring DATABASE_WORKER_URL in production, got: %v", err)
	}

	// DATABASE_WORKER_URL equals DATABASE_URL
	cfg.DatabaseWorkerURL = cfg.DatabaseURL
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_WORKER_URL must differ from DATABASE_URL in production") {
		t.Fatalf("expected error for worker URL == app URL, got: %v", err)
	}

	// DATABASE_WORKER_URL equals DATABASE_MAINT_URL
	cfg.DatabaseWorkerURL = cfg.DatabaseMaintURL
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_WORKER_URL must differ from DATABASE_MAINT_URL in production") {
		t.Fatalf("expected error for worker URL == maint URL, got: %v", err)
	}

	// Valid separate worker URL passes
	cfg.DatabaseWorkerURL = "postgres://pgapp_worker:secret@localhost:5432/pgapp"
	if err := cfg.ValidateForRealDeployment(); err != nil {
		t.Fatalf("expected valid production config to pass, got: %v", err)
	}
}

func TestValidateWarnings_DatabaseWorkerURL_Fallback(t *testing.T) {
	prev := os.Getenv("DATABASE_WORKER_URL")
	defer func() {
		if prev != "" {
			_ = os.Setenv("DATABASE_WORKER_URL", prev)
		} else {
			_ = os.Unsetenv("DATABASE_WORKER_URL")
		}
	}()
	_ = os.Unsetenv("DATABASE_WORKER_URL")

	cfg := validConfig()
	cfg.AppEnv = "development"
	cfg.DatabaseMaintURL = "postgres://pgapp_maint:secret@localhost:5432/pgapp"
	cfg.DatabaseWorkerURL = cfg.DatabaseMaintURL

	warns := cfg.ValidateWarnings()
	found := false
	for _, w := range warns {
		if strings.Contains(w, "DATABASE_WORKER_URL not set; falling back to DATABASE_MAINT_URL") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected warning for worker URL falling back to maint URL, got: %v", warns)
	}
}

func TestValidateForRealDeployment_FrontendURL(t *testing.T) {
	// In production, missing FrontendURL must fail
	cfg := validConfig()
	cfg.AppEnv = "production"
	cfg.FrontendURL = ""
	cfg.MetricsSecret = "metrics-secret-16b"
	cfg.DatabaseWorkerURL = "postgres://worker@localhost/db"
	cfg.DatabaseURL = "postgres://app@localhost/db"
	cfg.DatabaseMaintURL = "postgres://maint@localhost/db"
	err := cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "PUBLIC_APP_URL (or FRONTEND_URL) is required in production") {
		t.Fatalf("expected error for missing FrontendURL in production, got: %v", err)
	}

	// In production, HTTP scheme must fail
	cfg.FrontendURL = "http://pg.example.com"
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "must be a valid absolute HTTPS URL in production") {
		t.Fatalf("expected error for http URL in production, got: %v", err)
	}

	// In production, relative URL must fail
	cfg.FrontendURL = "/app"
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "must be a valid absolute HTTPS URL in production") {
		t.Fatalf("expected error for relative URL in production, got: %v", err)
	}

	// In production, valid HTTPS URL must pass
	cfg.FrontendURL = "https://pg.example.com"
	if err := cfg.ValidateForRealDeployment(); err != nil {
		t.Fatalf("expected valid https URL in production to pass, got: %v", err)
	}

	// In development, http URL must pass
	cfg.AppEnv = "development"
	cfg.FrontendURL = "http://localhost:5173"
	if err := cfg.ValidateForRealDeployment(); err != nil {
		t.Fatalf("expected http URL in development to pass, got: %v", err)
	}

	// In development, invalid URL format must fail
	cfg.FrontendURL = "not-a-valid-url"
	err = cfg.ValidateForRealDeployment()
	if err == nil || !strings.Contains(err.Error(), "must be a valid absolute HTTP or HTTPS URL") {
		t.Fatalf("expected error for malformed URL in development, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// KYCIdentitySecrets validation tests
// ---------------------------------------------------------------------------

func TestValidateForRealDeployment_VersionedKYCIdentitySecrets(t *testing.T) {
	cases := []struct {
		name    string
		secrets map[int16]string
		wantSub string // non-empty -> expect error containing this substring
	}{
		{
			name:    "nil map (env var unset) passes - KYC dormant",
			secrets: nil,
			wantSub: "",
		},
		{
			name:    "single valid secret passes",
			secrets: map[int16]string{1: "this-is-a-valid-kyc-identity-v1-secret-32ch"},
			wantSub: "",
		},
		{
			name:    "two distinct valid secrets pass",
			secrets: map[int16]string{1: "this-is-a-valid-kyc-identity-v1-secret-32ch", 2: "this-is-a-valid-kyc-identity-v2-secret-32ch"},
			wantSub: "",
		},
		{
			name:    "short secret rejected",
			secrets: map[int16]string{1: "short"},
			wantSub: "KYC_IDENTITY_SECRETS[1] must be at least 32 characters",
		},
		{
			name:    "placeholder secret rejected",
			secrets: map[int16]string{1: "change-me-kyc-identity-v1-placeholder-val"},
			wantSub: "KYC_IDENTITY_SECRETS[1] must not be a placeholder value",
		},
		{
			name: "duplicate value across versions rejected",
			secrets: map[int16]string{
				1: "same-secret-both-versions-long-enough-abc",
				2: "same-secret-both-versions-long-enough-abc",
			},
			wantSub: "must be distinct secrets (rotation requires unique keys per version)",
		},
		{
			name: "mixed: one valid, one short â€” both errors surface",
			secrets: map[int16]string{
				1: "this-is-a-valid-kyc-identity-v1-secret-32ch",
				2: "short",
			},
			wantSub: "KYC_IDENTITY_SECRETS[2] must be at least 32 characters",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.KYCIdentitySecrets = tc.secrets
			err := cfg.ValidateForRealDeployment()
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

func TestValidateForRealDeployment_PayoutAuthMode(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test rsa key: %v", err)
	}

	pkixBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal pkix: %v", err)
	}
	pkixPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkixBytes}))

	pkcs1Bytes := x509.MarshalPKCS1PublicKey(&priv.PublicKey)
	pkcs1PEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: pkcs1Bytes}))

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-cert"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create test cert: %v", err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))

	baseCfg := func() *Config {
		cfg := validConfig()
		cfg.CashfreePayoutClientID = "CF_TEST_PAYOUT_CLIENT_ID"
		cfg.CashfreePayoutClientSecret = "CF_TEST_PAYOUT_CLIENT_SECRET"
		cfg.CashfreePayoutFundsourceID = "FUND_SOURCE_123"
		cfg.PayoutEncryptionSecret = "this-is-a-valid-payout-encryption-secret-32"
		cfg.PayoutExportChecksumSecret = "this-is-a-valid-payout-checksum-secret-32ch"
		cfg.CashfreePayoutWebhookSecret = "twenty-char-secret!!"
		return cfg
	}

	t.Run("missing auth mode fails", func(t *testing.T) {
		cfg := baseCfg()
		cfg.CashfreePayoutAuthMode = ""
		err := cfg.ValidateForRealDeployment()
		if err == nil || !strings.Contains(err.Error(), "CF_PAYOUT_AUTH_MODE is required") {
			t.Fatalf("expected error for missing CF_PAYOUT_AUTH_MODE, got: %v", err)
		}
	})

	t.Run("invalid auth mode fails", func(t *testing.T) {
		cfg := baseCfg()
		cfg.CashfreePayoutAuthMode = "invalid_mode"
		err := cfg.ValidateForRealDeployment()
		if err == nil || !strings.Contains(err.Error(), "CF_PAYOUT_AUTH_MODE must be either 'signature' or 'ip'") {
			t.Fatalf("expected error for invalid CF_PAYOUT_AUTH_MODE, got: %v", err)
		}
	})

	t.Run("ip mode passes without public key", func(t *testing.T) {
		cfg := baseCfg()
		cfg.CashfreePayoutAuthMode = "ip"
		if err := cfg.ValidateForRealDeployment(); err != nil {
			t.Fatalf("expected ip mode to pass, got: %v", err)
		}
	})

	t.Run("signature mode requires public key", func(t *testing.T) {
		cfg := baseCfg()
		cfg.CashfreePayoutAuthMode = "signature"
		cfg.CashfreePayoutPublicKeyPEM = ""
		err := cfg.ValidateForRealDeployment()
		if err == nil || !strings.Contains(err.Error(), "CF_PAYOUT_PUBLIC_KEY is required") {
			t.Fatalf("expected error for missing public key, got: %v", err)
		}
	})

	t.Run("signature mode rejects corrupt key", func(t *testing.T) {
		cfg := baseCfg()
		cfg.CashfreePayoutAuthMode = "signature"
		cfg.CashfreePayoutPublicKeyPEM = "-----BEGIN PUBLIC KEY-----\nNOT_A_VALID_KEY\n-----END PUBLIC KEY-----"
		err := cfg.ValidateForRealDeployment()
		if err == nil || !strings.Contains(err.Error(), "CF_PAYOUT_PUBLIC_KEY must be a valid PEM-encoded") {
			t.Fatalf("expected error for corrupt public key, got: %v", err)
		}
	})

	t.Run("signature mode accepts PKIX public key", func(t *testing.T) {
		cfg := baseCfg()
		cfg.CashfreePayoutAuthMode = "signature"
		cfg.CashfreePayoutPublicKeyPEM = pkixPEM
		if err := cfg.ValidateForRealDeployment(); err != nil {
			t.Fatalf("expected PKIX public key to pass, got: %v", err)
		}
	})

	t.Run("signature mode accepts PKCS1 public key", func(t *testing.T) {
		cfg := baseCfg()
		cfg.CashfreePayoutAuthMode = "signature"
		cfg.CashfreePayoutPublicKeyPEM = pkcs1PEM
		if err := cfg.ValidateForRealDeployment(); err != nil {
			t.Fatalf("expected PKCS#1 public key to pass, got: %v", err)
		}
	})

	t.Run("signature mode accepts X.509 certificate", func(t *testing.T) {
		cfg := baseCfg()
		cfg.CashfreePayoutAuthMode = "signature"
		cfg.CashfreePayoutPublicKeyPEM = certPEM
		if err := cfg.ValidateForRealDeployment(); err != nil {
			t.Fatalf("expected X.509 certificate to pass, got: %v", err)
		}
	})
}

