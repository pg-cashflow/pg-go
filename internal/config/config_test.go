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
	keys := []string{"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET", "APP_ENV", "FIREBASE_PROJECT_ID", "PAYOUT_ENCRYPTION_SECRET", "PAYOUT_ENCRYPTION_KEY", "PAYOUT_CHECKSUM_SECRET", "PAYOUT_APPROVAL_SECRET", "SMTP_HOST", "TRUSTED_PROXIES"}
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
	_ = os.Setenv("PAYOUT_APPROVAL_SECRET", "approval-secret-min-32-chars-long-abc")
	_ = os.Setenv("SMTP_HOST", "smtp.example.com")
	_ = os.Setenv("TRUSTED_PROXIES", "127.0.0.1")

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

func TestLoad_ProductionRequiresSMTPHost(t *testing.T) {
	keys := []string{
		"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET",
		"APP_ENV", "FIREBASE_PROJECT_ID", "PAYOUT_ENCRYPTION_SECRET", "PAYOUT_CHECKSUM_SECRET",
		"PAYOUT_APPROVAL_SECRET", "SMTP_HOST", "TRUSTED_PROXIES",
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
	_ = os.Setenv("APP_ENV", "production")
	_ = os.Setenv("FIREBASE_PROJECT_ID", "firebase-project-id")
	_ = os.Setenv("PAYOUT_CHECKSUM_SECRET", "checksum-secret-min-32-chars-long-abc")
	_ = os.Setenv("PAYOUT_APPROVAL_SECRET", "approval-secret-min-32-chars-long-abc")
	_ = os.Setenv("PAYOUT_ENCRYPTION_SECRET", "payout-secret-min-32-chars-long-abc")
	_ = os.Setenv("TRUSTED_PROXIES", "127.0.0.1")

	// Missing SMTP_HOST in production must fail closed
	if _, err := Load(); err == nil {
		t.Fatal("expected error when SMTP_HOST is missing in production")
	}

	// Set SMTP_HOST, Load must succeed
	_ = os.Setenv("SMTP_HOST", "smtp.example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed with SMTP_HOST set in production: %v", err)
	}
	if cfg.SMTPHost != "smtp.example.com" {
		t.Fatalf("unexpected SMTPHost: %s", cfg.SMTPHost)
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
		"PAYOUT_APPROVAL_SECRET", "CASHFREE_PG_BASE_URL", "SMTP_HOST", "TRUSTED_PROXIES",
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
		_ = os.Setenv("PAYOUT_APPROVAL_SECRET", "approval-secret-min-32-chars-long-abc")
		_ = os.Setenv("SMTP_HOST", "smtp.example.com")
		_ = os.Setenv("TRUSTED_PROXIES", "127.0.0.1")
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

func TestLoad_WebhookSecretResolution(t *testing.T) {
	cases := []struct {
		name          string
		pgAppID       string
		pgSecretKey   string
		webhookSecret string
		wantErrSub    string
		wantSecret    string
		wantSource    string
	}{
		{
			name:          "override set -> used",
			pgAppID:       "app_1",
			pgSecretKey:   "sec_key_1",
			webhookSecret: "custom_override_secret",
			wantSecret:    "custom_override_secret",
			wantSource:    "override",
		},
		{
			name:          "override empty, PG secret set -> PG secret used",
			pgAppID:       "app_1",
			pgSecretKey:   "sec_key_1",
			webhookSecret: "",
			wantSecret:    "sec_key_1",
			wantSource:    "pg_secret_key",
		},
		{
			name:          "PG app id set, PG secret empty -> error naming CASHFREE_PG_SECRET_KEY",
			pgAppID:       "app_1",
			pgSecretKey:   "",
			webhookSecret: "",
			wantErrSub:    "CASHFREE_PG_SECRET_KEY is required",
		},
		{
			name:          "No PG keys -> no error, no warning",
			pgAppID:       "",
			pgSecretKey:   "",
			webhookSecret: "",
			wantSecret:    "",
			wantSource:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := []string{
				"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET",
				"CASHFREE_PG_APP_ID", "CASHFREE_APP_ID",
				"CASHFREE_PG_SECRET_KEY", "CASHFREE_SECRET_KEY",
				"CASHFREE_PG_WEBHOOK_SECRET", "CASHFREE_WEBHOOK_SECRET",
			}
			prev := map[string]string{}
			for _, k := range keys {
				prev[k] = os.Getenv(k)
				_ = os.Unsetenv(k)
			}
			defer func() {
				for k, v := range prev {
					if v != "" {
						_ = os.Setenv(k, v)
					} else {
						_ = os.Unsetenv(k)
					}
				}
			}()

			_ = os.Setenv("DATABASE_URL", "postgres://localhost/db")
			_ = os.Setenv("JWT_SECRET", "jwt-secret-over-32-characters-minimum")
			_ = os.Setenv("OTP_HMAC_SECRET", "otp-secret-over-32-characters-minimum")
			_ = os.Setenv("MAGIC_LINK_HMAC_SECRET", "magic-link-over-32-characters-minimum")

			if tc.pgAppID != "" {
				_ = os.Setenv("CASHFREE_PG_APP_ID", tc.pgAppID)
			}
			if tc.pgSecretKey != "" {
				_ = os.Setenv("CASHFREE_PG_SECRET_KEY", tc.pgSecretKey)
			}
			if tc.webhookSecret != "" {
				_ = os.Setenv("CASHFREE_PG_WEBHOOK_SECRET", tc.webhookSecret)
			}

			cfg, err := Load()
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("expected error containing %q, got: %v", tc.wantErrSub, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.CashfreeWebhookSecret != tc.wantSecret {
				t.Fatalf("expected CashfreeWebhookSecret %q, got %q", tc.wantSecret, cfg.CashfreeWebhookSecret)
			}
			if cfg.WebhookSecretSource != tc.wantSource {
				t.Fatalf("expected WebhookSecretSource %q, got %q", tc.wantSource, cfg.WebhookSecretSource)
			}
		})
	}
}

func TestLoad_PublicAppURL(t *testing.T) {
	keys := []string{
		"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET",
		"PUBLIC_APP_URL", "FRONTEND_URL",
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

	// 1. FRONTEND_URL alone
	_ = os.Setenv("FRONTEND_URL", "https://frontend.example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FrontendURL != "https://frontend.example.com" {
		t.Fatalf("expected FrontendURL to be %q, got %q", "https://frontend.example.com", cfg.FrontendURL)
	}

	// 2. PUBLIC_APP_URL takes precedence over FRONTEND_URL
	_ = os.Setenv("PUBLIC_APP_URL", "https://public.example.com")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FrontendURL != "https://public.example.com" {
		t.Fatalf("expected FrontendURL to prioritize PUBLIC_APP_URL %q, got %q", "https://public.example.com", cfg.FrontendURL)
	}
}

// ---------------------------------------------------------------------------
// parseVersionedSecrets16 unit tests
// ---------------------------------------------------------------------------

func TestParseVersionedSecrets16(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  map[int16]string
	}{
		{
			name:  "empty string produces empty map",
			input: "",
			want:  map[int16]string{},
		},
		{
			name:  "single valid entry",
			input: "1:secret-v1-long-enough-for-hmac",
			want:  map[int16]string{1: "secret-v1-long-enough-for-hmac"},
		},
		{
			name:  "two valid entries",
			input: "1:secret-v1-hmac-key,2:secret-v2-hmac-key",
			want: map[int16]string{
				1: "secret-v1-hmac-key",
				2: "secret-v2-hmac-key",
			},
		},
		{
			name:  "whitespace around the outer pair is trimmed; interior whitespace in version field causes skip",
			// TrimSpace("  1 : secret-v1 ") → "1 : secret-v1 "
			// SplitN("1 : secret-v1 ", ":", 2) → ["1 ", " secret-v1 "]
			// Atoi("1 ") fails (trailing space) → pair skipped.
			// Only pairs with no whitespace between version digits and colon are parsed.
			input: "  1 : secret-v1 , 2 : secret-v2 ",
			want:  map[int16]string{}, // all three pairs skipped due to interior whitespace
		},
		{
			name:  "version 0 is rejected (reserved)",
			input: "0:secret-v0,1:secret-v1",
			want:  map[int16]string{1: "secret-v1"},
		},
		{
			name:  "negative version is rejected",
			input: "-1:secret-v-neg,1:secret-v1",
			want:  map[int16]string{1: "secret-v1"},
		},
		{
			name:  "version above int16 max (32767) is rejected",
			input: "32768:secret-too-big,1:secret-v1",
			want:  map[int16]string{1: "secret-v1"},
		},
		{
			name:  "version at int16 max boundary (32767) is accepted",
			input: "32767:secret-at-max",
			want:  map[int16]string{32767: "secret-at-max"},
		},
		{
			name:  "malformed pair (no colon) is skipped",
			input: "1secret-v1,2:secret-v2",
			want:  map[int16]string{2: "secret-v2"},
		},
		{
			name:  "non-numeric version is skipped",
			input: "abc:secret-v-abc,1:secret-v1",
			want:  map[int16]string{1: "secret-v1"},
		},
		{
			name:  "empty value is accepted (validation catches it, not parser)",
			input: "1:",
			want:  map[int16]string{1: ""},
		},
		{
			name:  "colon in secret value is preserved (SplitN 2)",
			input: "1:secret:with:colons",
			want:  map[int16]string{1: "secret:with:colons"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseVersionedSecrets16(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %d entries %v, want %d entries %v", len(got), got, len(tc.want), tc.want)
			}
			for ver, wantSec := range tc.want {
				if gotSec, ok := got[ver]; !ok {
					t.Errorf("missing version %d in result", ver)
				} else if gotSec != wantSec {
					t.Errorf("version %d: got %q, want %q", ver, gotSec, wantSec)
				}
			}
		})
	}
}

func TestLoad_KYCIdentitySecrets_Wiring(t *testing.T) {
	keys := []string{
		"DATABASE_URL", "JWT_SECRET", "OTP_HMAC_SECRET", "MAGIC_LINK_HMAC_SECRET",
		"KYC_IDENTITY_SECRET", "KYC_IDENTITY_SECRETS",
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

	// Case 1: KYC_IDENTITY_SECRETS unset → field is nil, not an empty map
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.KYCIdentitySecrets != nil {
		t.Fatalf("expected nil KYCIdentitySecrets when env var unset, got %v", cfg.KYCIdentitySecrets)
	}

	// Case 2: single versioned secret is parsed into the map
	_ = os.Setenv("KYC_IDENTITY_SECRETS", "1:secret-v1-min-32-chars-long-abc-ok")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.KYCIdentitySecrets) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(cfg.KYCIdentitySecrets), cfg.KYCIdentitySecrets)
	}
	if cfg.KYCIdentitySecrets[1] != "secret-v1-min-32-chars-long-abc-ok" {
		t.Fatalf("version 1 secret mismatch: %q", cfg.KYCIdentitySecrets[1])
	}

	// Case 3: two versioned secrets
	_ = os.Setenv("KYC_IDENTITY_SECRETS", "1:secret-v1-min-32-chars-long-abc-ok,2:secret-v2-min-32-chars-long-abc-ok")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.KYCIdentitySecrets) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(cfg.KYCIdentitySecrets))
	}

	// Case 4: KYCIdentitySecret (singular legacy) and KYCIdentitySecrets coexist independently
	_ = os.Setenv("KYC_IDENTITY_SECRET", "legacy-single-secret-min-32-chars-ok")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.KYCIdentitySecret != "legacy-single-secret-min-32-chars-ok" {
		t.Fatalf("KYCIdentitySecret not loaded: %q", cfg.KYCIdentitySecret)
	}
	if len(cfg.KYCIdentitySecrets) != 2 {
		t.Fatalf("KYCIdentitySecrets not independently loaded: %v", cfg.KYCIdentitySecrets)
	}
}

func TestLoad_AppEnvNormalization(t *testing.T) {
	origEnv := os.Getenv("APP_ENV")
	defer func() {
		if origEnv != "" {
			_ = os.Setenv("APP_ENV", origEnv)
		} else {
			_ = os.Unsetenv("APP_ENV")
		}
	}()

	_ = os.Setenv("DATABASE_URL", "postgres://localhost/db")
	_ = os.Setenv("JWT_SECRET", "jwt-secret-min-32-chars-long-abc")
	_ = os.Setenv("OTP_HMAC_SECRET", "otp-secret-min-32-chars-long-abc")
	_ = os.Setenv("MAGIC_LINK_HMAC_SECRET", "magic-secret-min-32-chars-long-abc")

	// Test 1: Mixed case and whitespace for development
	_ = os.Setenv("APP_ENV", "  DeVeLoPmEnT  ")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AppEnv != "development" {
		t.Fatalf("expected AppEnv to be normalized to 'development', got %q", cfg.AppEnv)
	}

	// Test 2: Mixed case and whitespace for staging
	_ = os.Setenv("APP_ENV", "  StAgInG ")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AppEnv != "staging" {
		t.Fatalf("expected AppEnv to be normalized to 'staging', got %q", cfg.AppEnv)
	}

	// Test 3: Mixed case production enforces production validation
	_ = os.Setenv("APP_ENV", "  PrOdUcTiOn  ")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "FIREBASE_PROJECT_ID is required in production") {
		t.Fatalf("expected production validation error for normalized AppEnv, got %v", err)
	}
}


